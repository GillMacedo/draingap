# Roadmap

## 0.1.0 (this release)
- Check that a Go program handles the stop signal Kubernetes sends, that `(*http.Server).Shutdown` waits, and that the program doesn't exit while it drains.
- Add up the preStop delay, the sleep before Shutdown and the Shutdown deadline, following flags and env vars to the container's `args` and `env`, and compare them with `terminationGracePeriodSeconds`.
- Warn about `sh -c` commands without `exec`.

## Next
- gRPC: `(*grpc.Server).GracefulStop` and `Stop`, and the common pattern of a timer that falls back to `Stop`.
- Read `STOPSIGNAL`, `ENTRYPOINT` and `CMD` from a Dockerfile with `--dockerfile`.
- Count sequential shutdown steps that share a context (OpenTelemetry providers, database pools) as part of the budget.
- Report readiness: a program that keeps answering its readiness probe with 200 after SIGTERM while it waits for traffic to stop.
- Frameworks with their own shutdown: echo's `Shutdown`, fx lifecycle hooks.
- SARIF output for GitHub code scanning.

## Not planned
- Running the program or connecting to a cluster.
- Changing source files or manifests. draingap names the change and leaves it to you.
