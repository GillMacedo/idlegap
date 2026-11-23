// Command web is a small service used to show idlegap on a setup that is
// fine: its idle timeout is longer than ingress-nginx's.
package main

import (
	"log"
	"net/http"
	"time"
)

const idleTimeout = 75 * time.Second

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// The debug server isn't behind the Ingress, so idlegap skips it.
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       idleTimeout,
	}
	log.Fatal(srv.ListenAndServe())
}
