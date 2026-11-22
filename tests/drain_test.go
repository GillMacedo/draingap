package tests

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealShutdown runs each test program, starts a request that takes
// 1.5s, sends SIGTERM 100ms after the handler starts, and checks what really happens against
// what draingap says. A program drains when the request completes and
// the program exits by itself. Programs with an error-level program
// finding must not drain; programs without one must.
func TestRealShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs SIGTERM, which Windows doesn't have")
	}
	if testing.Short() {
		t.Skip("runs every program for a few seconds")
	}
	bin := t.TempDir()
	for _, c := range progCases {
		c := c
		if c.prog == "noserver" {
			continue // nothing to send a request to
		}
		t.Run(c.prog, func(t *testing.T) {
			t.Parallel()
			r := draingap(t, "--pkg", prog(c.prog), manifest("basic.yaml"))
			programError := false
			for _, code := range codes(r.stdout) {
				// DG3xx are about the manifest's time budget, not about
				// what the program does when it gets the signal.
				if strings.HasPrefix(code, "error DG1") || strings.HasPrefix(code, "error DG2") {
					programError = true
				}
			}
			exe := filepath.Join(bin, c.prog)
			build := exec.Command("go", "build", "-o", exe, prog(c.prog))
			build.Dir = root
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			drained, how := drains(t, exe)
			t.Logf("drained=%v: %s", drained, how)
			if drained == programError {
				t.Errorf("draingap program error = %v, but the real program drained = %v (%s)\n%s", programError, drained, how, r.stdout)
			}
		})
	}
}

// TestExamplesShutdown does the same for the two programs the README
// runs: orders drains, billing doesn't.
func TestExamplesShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs SIGTERM, which Windows doesn't have")
	}
	if testing.Short() {
		t.Skip("runs programs for a few seconds")
	}
	bin := t.TempDir()
	for name, want := range map[string]bool{"orders": true, "billing": false} {
		exe := filepath.Join(bin, name)
		build := exec.Command("go", "build", "-o", exe, "./examples/"+name)
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		if got, how := drains(t, exe); got != want {
			t.Errorf("examples/%s drained = %v, want %v (%s)", name, got, want, how)
		}
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func drains(t *testing.T, exe string) (bool, string) {
	addr := freePort(t)
	cmd := exec.Command(exe, "-addr", addr)
	cmd.Env = append(os.Environ(), "SHUTDOWN_TIMEOUT=10s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "slow request started" {
				started <- struct{}{}
			}
		}
	}()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill()

	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the program never listened on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	type resp struct {
		body string
		err  error
	}
	got := make(chan resp, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			got <- resp{err: err}
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		got <- resp{string(b), err}
	}()
	// Send the signal only once the handler is running.
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the /slow handler never started")
	}
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var r resp
	select {
	case r = <-got:
	case <-time.After(30 * time.Second):
		return false, "the request never finished"
	}
	if r.err != nil || strings.TrimSpace(r.body) != "done" {
		return false, fmt.Sprintf("the request was cut: %v %q", r.err, r.body)
	}
	select {
	case <-exited:
		return true, "the request finished and the program exited"
	case <-time.After(30 * time.Second):
		return false, "the request finished, but the program was still running 30s after SIGTERM"
	}
}
