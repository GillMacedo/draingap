---
name: checking-go-graceful-shutdown-on-kubernetes
description: Checks whether a Go HTTP service drains its requests when Kubernetes stops the pod, by reading the program's source (through go list) and its rendered Kubernetes manifests (Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, Pod, List). Reports a program that doesn't handle SIGTERM (or the container's stopSignal), handles only os.Interrupt, ignores the signal or swallows it on an unread channel; http.Server.Shutdown given a context that is already canceled (from signal.NotifyContext); main exiting while Shutdown drains (log.Fatal(srv.ListenAndServe()) and similar); servers never shut down; Shutdown with no deadline; a preStop delay plus in-code sleep plus Shutdown deadline longer than terminationGracePeriodSeconds; no delay before the listener closes; and sh -c commands that keep the shell as PID 1. Use when a Go service returns 502, 503, connection refused or connection reset during rollouts, scale-down or node drains, when pods take the full grace period to stop, when reviewing a change to shutdown code, terminationGracePeriodSeconds, preStop hooks or container commands, or when asked whether a Go service shuts down gracefully on Kubernetes.
---

# Checking Go graceful shutdown on Kubernetes

draingap reads the Go program with `go list -deps -export` (the code must build for linux), type-checks the main module's packages, and follows signal handling, `(*http.Server).Shutdown` calls, their contexts and deadlines, and what `main` does after `Serve` returns. It reads the container's grace period, preStop hook, stopSignal, command, args and env from the manifests, and adds up the shutdown timeline. It needs Go 1.22 or newer. It doesn't run the program, but `go list -export` compiles it, which may download modules.

## Get the two inputs

1. **The program:** the directory of its `main` package, such as `./cmd/server`. If the image is built from another commit, check that commit out first and point `--pkg` there.
2. **Rendered manifests**, never a chart or kustomization directory:
   - Helm: `helm template RELEASE CHART -n NS -f values.yaml > rendered.yaml && echo --- >> rendered.yaml`
   - Kustomize: `kustomize build DIR > rendered.yaml && echo --- >> rendered.yaml`
   - A plain file: make sure it ends with a `---` line. A file that ends inside a workload without one is refused as possibly cut off.
   - From a cluster: `kubectl get deploy NAME -o json > live.json` (JSON needs no `---`).

The `&&` adds the final `---` only when the render command succeeds. Check that it exited 0 before you go on.

## Run it

Build it once, then run it:

    go build -o "${TMPDIR:-/tmp}/draingap" $(ls "${CLAUDE_PLUGIN_ROOT}"/scripts/*.go | grep -v _test.go)
    "${TMPDIR:-/tmp}/draingap" --pkg ./cmd/server --container server rendered.yaml

`--container` names the containers that run this program, comma-separated, as `name` or `Kind/workload/name`. Leave it out when the manifests have one container. If the image's Dockerfile sets `STOPSIGNAL`, pass it with `--stop-signal SIGINT` (a `lifecycle.stopSignal` in the manifest still wins). Pass `--tags` with the build tags of the release build. Add `--format json` for machine-readable output, and `--fail-on warn` to fail on warnings too.

## Read the exit code first

- **0:** nothing at or above `--fail-on` (default `error`). Notes may still be printed; read them.
- **1:** findings at or above `--fail-on`.
- **2:** nothing was checked. Stderr says why: a manifest that is empty, not YAML or JSON, a command's error output, unrendered, cut off, or has no workload; a container name that isn't there; or a program that doesn't build for linux, isn't a main package, uses cgo or isn't in a module. Fix the input and run again. Never describe exit 2 as a clean result.

## Check each finding

Each container gets a timeline first: stop signal, grace period, preStop, sleep in code, Shutdown deadline, and how much of the grace period that needs. Then the findings:

- **Signal (DG101 to DG105):** the program doesn't handle the stop signal (DG101), handles other signals only, usually `os.Interrupt` (DG102), ignores it or catches it on a channel nothing reads (DG103). DG104 and DG105 are notes: the signal list couldn't be read, or only a dependency calls `os/signal`.
- **Program (DG201 to DG207):** Shutdown gets an already canceled context (DG201); the program exits while Shutdown drains (DG202); Shutdown has no deadline (DG203, warn); no Shutdown at all (DG204); `http.ListenAndServe` with no server value (DG205); a second server never shut down (DG207, warn). DG106 means no `net/http` server was found in the module's code, so a framework (gin, echo, grpc) wasn't followed. DG206 means the Shutdown deadline couldn't be worked out, so the budget wasn't checked.
- **Budget (DG301 to DG305):** preStop + sleep + deadline is longer than the grace period (DG301) or uses all of it (DG302); preStop alone uses the grace period (DG303); no delay before the listener closes (DG304, warn); grace period 0 (DG305).
- **Command (DG401):** the container runs the program through `sh -c` without `exec`.
- **Notes (DG403, DG901):** a preStop or sleep duration that isn't known; calls in functions nothing reachable from `main` calls.

Read the code at each position before you report it. For DG202, show the line after `Serve` that ends the program. For DG301, say which number to change and what it should be.

## Report

1. Errors first: what happens to a request in flight when the pod stops, the file and line, and the fix.
2. Then warnings, and whether they matter for this service (DG304 matters for anything behind a Service; DG203 matters with long requests or WebSockets).
3. Say what wasn't checked: notes, the image's ENTRYPOINT and STOPSIGNAL when the manifest doesn't set them, frameworks and gRPC servers, and that the check used the source at `--pkg`, not the image.

## Don't

Don't change the cluster or apply manifests. Don't say the service shuts down gracefully because draingap found nothing: it checks the shutdown path it can see in the module's code and the numbers in the manifest, not handlers that ignore their request context, background workers, or what a framework does.
