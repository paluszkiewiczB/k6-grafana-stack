package main

import (
	"log"
	"math/rand"
	"net/http"
	"time"
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
		stable(w, r)
		break
	case "/unstable":
		wrap(stable, injectFails, slowDown)(w, r)
		break
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
	}
}

func stable(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(200)
	_, _ = w.Write([]byte("hello world"))
}
func injectFails(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if rand.Intn(2) == 0 {
			next(writer, request)
			return
		}

		log.Printf("random fail")
		writer.WriteHeader(500)
	}
}

func slowDown(next http.HandlerFunc) http.HandlerFunc {
	ms := rand.Intn(1000)
	log.Printf("slowing down by: %d ms", ms)
	time.Sleep(time.Duration(ms * int(time.Millisecond)))
	return next
}

type middleware func(next http.HandlerFunc) http.HandlerFunc

func wrap(f http.HandlerFunc, m ...middleware) http.HandlerFunc {
	handler := f
	for _, mid := range m {
		handler = mid(handler)
	}
	return handler
}
