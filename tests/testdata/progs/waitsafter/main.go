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

func main() {
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler()}
	done := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
		<-c
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(done)
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}
