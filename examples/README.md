# Examples

Both programs were written for this repository and use only the standard library. Service names, image names and registries are made up.

- `orders/main.go` shuts down the way Kubernetes expects: `signal.NotifyContext` for SIGTERM and SIGINT, `Serve` in a goroutine, `Shutdown` with a `context.WithTimeout` deadline from the `-shutdown-timeout` flag (default 20s), and `main` waits for Serve to return before it exits.
  - `orders/deploy.yaml`: a 5s preStop `sleep` action and a 30s grace period. draingap finds nothing.
  - `orders/deploy-tight.yaml`: `-shutdown-timeout=30s`, a 10s `exec` preStop and the default 30s grace period. draingap reports DG301.
- `billing/main.go` has three bugs: it listens for `os.Interrupt` only (DG102), derives the Shutdown context from the signal context (DG201), and calls `log.Fatal(srv.ListenAndServe())` (DG202). `billing/deploy.yaml` starts it through `sh -c` without `exec` (DG401).

Both programs serve `/slow`, which takes 1.5s. `TestExamplesShutdown` in `tests/drain_test.go` runs them, sends SIGTERM during a `/slow` request, and checks that `orders` finishes it and `billing` doesn't. To try it by hand in bash:

    go build -o /tmp/orders ./examples/orders
    /tmp/orders -addr 127.0.0.1:8080 &
    sleep 1
    curl -sS 127.0.0.1:8080/slow &
    sleep 0.3
    kill -TERM %1
    wait

`orders` logs `stop signal received, draining`, curl prints `done`, and then it logs `drained`. Build `billing` the same way and it exits at once with no log line, and curl reports an empty reply.
