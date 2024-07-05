package main

import (
	"fmt"
	"log"

	"net/http"
	"os"
	"time"
)

func main() {
	println("starting server...")

	http.HandleFunc("/", indexHandler)

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Printf("Error starting server: %s", err.Error())
		os.Exit(1)
	}

}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	println("request received")
	_, _ = fmt.Fprintf(w, "Request time: %s", time.Now().Format(time.RFC3339))
}
