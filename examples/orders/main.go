// Command orders is an example HTTP service that shuts down the way
// Kubernetes expects: it handles SIGTERM, lets Shutdown wait for
// requests in flight with a deadline, and waits for Shutdown before main
// returns.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 20*time.Second, "how long Shutdown waits for requests in flight")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("slow request started")
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprintln(w, "done")
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	log.Printf("listening on %s", *addr)

	select {
	case err := <-served:
		log.Fatal(err)
	case <-ctx.Done():
	}
	log.Print("stop signal received, draining")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Printf("serve: %v", err)
	}
	log.Print("drained")
}
