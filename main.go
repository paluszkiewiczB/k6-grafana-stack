package main

import (
	"log"
	"math/rand"
	"net/http"
)

func main() {
	err := http.ListenAndServe("0.0.0.0:8080", new(server))
	if err != nil {
		log.Fatal(err)
	}
}

type server struct{}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/stable":
		stable(w)
		break
	case "/unstable":
		unstable(w)
		break
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
	}
}

func stable(w http.ResponseWriter) {
	w.WriteHeader(200)
	_, _ = w.Write([]byte("hello world"))
}
func unstable(w http.ResponseWriter) {
	if rand.Intn(2) == 0 {
		w.WriteHeader(500)
		return
	}

	stable(w)
}
