// Command billing is an example HTTP service with three shutdown bugs
// that each cut requests in flight when Kubernetes stops the pod.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("slow request started")
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprintln(w, "done")
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Bug 1: os.Interrupt is SIGINT. Kubernetes sends SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		<-ctx.Done()
		// Bug 2: ctx is already canceled here, so Shutdown doesn't wait.
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	// Bug 3: ListenAndServe returns ErrServerClosed as soon as Shutdown
	// starts, and log.Fatal exits before the drain is done.
	log.Fatal(srv.ListenAndServe())
}
