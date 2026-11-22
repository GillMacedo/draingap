# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-22

### Added
- `draingap` CLI and the `checking-go-graceful-shutdown-on-kubernetes` skill: read a Go program through `go list -deps -export` and its rendered Kubernetes manifests, and check that it drains requests in flight before Kubernetes kills it.
- Signal checks: no handler for the stop signal, only `os.Interrupt`, `signal.Ignore`, and a `signal.Notify` channel nothing reads.
- Program checks: `Shutdown` with an already canceled context, the program exiting while Shutdown drains, `Shutdown` with no deadline, no `Shutdown`, `http.ListenAndServe` with no server value, and a second server never shut down.
- Time budget: preStop, sleep before Shutdown and the Shutdown deadline against `terminationGracePeriodSeconds`, with deadlines followed to constants, `flag` and `pflag` defaults overridden by container `args`, and `os.Getenv` values from container `env`.
- `sh -c` container commands without `exec`.
- `--pkg`, `--container`, `--stop-signal`, `--tags`, `--format text|json` and `--fail-on error|warn|note|never`.
- Refuses with exit 2 when a manifest is empty, cut off, not YAML or JSON, a command's error output, unrendered or has no workload, and when the program doesn't build for linux or isn't a main package.
