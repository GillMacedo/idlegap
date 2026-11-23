// Command api is a small service used to show idlegap. It runs: start it
// with go run ./examples/alb/api and request http://localhost:8080/.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	readTimeout := flag.Duration("read-timeout", 30*time.Second, "how long to wait for a request")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       *readTimeout,
		WriteTimeout:      30 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
