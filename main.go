package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
)

type command struct {
	Op    string `json:"op,omitempty"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type fsm struct {
	store map[string]string
	mu    sync.Mutex
}

func (f *fsm) Apply(log *raft.Log) interface{} {
	var c command
	if err := json.Unmarshal(log.Data, &c); err != nil {
		panic(err)
	}
	switch c.Op {
	case "set":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.store[c.Key] = c.Value
		return nil
	default:
		panic(fmt.Sprintf("unrecognized command op: %s", c.Op))
	}
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{store: f.store}, nil
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	var store map[string]string
	if err := json.NewDecoder(rc).Decode(&store); err != nil {
		return err
	}
	f.store = store
	return nil
}

type fsmSnapshot struct {
	store map[string]string
}

func (f *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := func() error {
		b, err := json.Marshal(f.store)
		if err != nil {
			return err
		}
		if _, err := sink.Write(b); err != nil {
			return err
		}
		return sink.Close()
	}(); err != nil {
		sink.Cancel()
		return err
	}
	return nil
}

func (f *fsmSnapshot) Release() {}

func main() {

	raftDir := "raft"
	os.MkdirAll(raftDir, 0700)

	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(os.Getenv("NODE_ID"))

	// hostname, err := os.Hostname()
	// if err != nil {
	// 	log.Fatalf("failed to resolve hostname: %v", err)
	// }

	raw_addr := fmt.Sprintf("%s:%s", os.Getenv("NODE_ID"), os.Getenv("RAFT_PORT"))

	addr, err := net.ResolveTCPAddr("tcp", raw_addr)
	if err != nil {
		log.Fatalf("failed to resolve TCP address: %v", err)
	}

	fmt.Printf(">>> %v\n", addr.String())

	transport, err := raft.NewTCPTransport(raw_addr, addr, 3, 10*time.Second, os.Stdout)
	if err != nil {
		log.Fatalf("failed to create transport: %v", err)
	}

	println("1")

	snapshots, err := raft.NewFileSnapshotStore(raftDir, 1, os.Stdout)
	if err != nil {
		log.Fatalf("failed to create snapshot store: %v", err)
	}

	println("2")

	logStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "raft.db"))
	if err != nil {
		log.Fatalf("failed to create log store: %v", err)
	}

	println("3")
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(raftDir, "stable-raft.db"))
	if err != nil {
		log.Fatalf("failed to create stable store: %v", err)
	}

	println("4")

	fsm := &fsm{store: make(map[string]string)}
	raftNode, err := raft.NewRaft(raftConfig, fsm, logStore, stableStore, snapshots, transport)
	if err != nil {
		log.Fatalf("failed to create raft node: %v", err)
	}
	println("5")

	if os.Getenv("JOIN") != "" {
		println("joinging...")
		// time.Sleep(time.Second * 10)
		joinRaftCluster(os.Getenv("JOIN"), os.Getenv("NODE_ID"), raw_addr)
	} else {
		println("bootstrap")
		raftNode.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      raftConfig.LocalID,
					Address: transport.LocalAddr(),
				},
			},
		})
	}

	http.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		var c command
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.Op = "set"
		b, err := json.Marshal(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		applyFuture := raftNode.Apply(b, 5*time.Second)
		if err := applyFuture.Error(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("/join", func(w http.ResponseWriter, r *http.Request) {
		followerId := r.URL.Query().Get("followerId")
		followerAddr := r.URL.Query().Get("followerAddr")

		fmt.Printf("/join %s %s\n", followerId, followerAddr)

		if raftNode.State() != raft.Leader {
			println("not the leader.")
			json.NewEncoder(w).Encode(struct {
				Error string `json:"error"`
			}{
				"Not the leader",
			})
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		err := raftNode.AddVoter(raft.ServerID(followerId), raft.ServerAddress(followerAddr), 0, 0).Error()
		if err != nil {
			log.Printf("Failed to add follower: %s", err)
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		}

		println("add voter success.")
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		fsm.mu.Lock()
		defer fsm.mu.Unlock()
		value, ok := fsm.store[key]
		if !ok {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		w.Write([]byte(value))
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("HTTP_PORT")), nil))
}

func joinRaftCluster(leader, followerId, followerAddr string) {
	fmt.Printf("joinRaftCluster: %v %v %v\n", leader, followerId, followerAddr)
	url := fmt.Sprintf("http://%s/join?followerId=%s&followerAddr=%s", leader, followerId, followerAddr)
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("failed to join raft cluster: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("failed to join raft cluster: %s", resp.Status)
	}
}
