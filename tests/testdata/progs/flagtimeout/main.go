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

var (
	_ = context.Background
	_ = errors.Is
	_ = os.Exit
	_ = signal.Notify
	_ = syscall.SIGTERM
	_ = log.Print
)

var addr = flag.String("addr", ":8080", "listen address")

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("slow request started")
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprintln(w, "done")
	})
	return mux
}

var shutdownTimeout time.Duration

func main() {
	flag.DurationVar(&shutdownTimeout, "shutdown-timeout", 25*time.Second, "shutdown deadline")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler()}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	go srv.ListenAndServe()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	srv.Shutdown(sctx)
}
