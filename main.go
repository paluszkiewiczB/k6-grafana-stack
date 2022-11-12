package main

import (
	"log"
	"net/http"
)

func main() {
	err := http.ListenAndServe("0.0.0.0:8080", new(server))
	if err != nil {
		return
	}
}

type server struct{}

func (s *server) ServeHTTP(w http.ResponseWriter, e *http.Request) {
	w.WriteHeader(200)
	_, err := w.Write([]byte("hello world"))
	if err != nil {
		log.Printf("could not write response body: %v", err)
	}
}
