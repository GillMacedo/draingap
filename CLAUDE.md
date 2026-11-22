# draingap

Claude Code skill and Go CLI that checks whether a Go HTTP service drains its requests before Kubernetes kills it, by reading the program's source and its rendered manifests together.

## Architecture
`plugin/scripts` is one `package main` with no dependencies outside the standard library, so the installed plugin builds with `go build` alone. `load.go` runs `go list -deps -export` and type-checks the module's packages; `program.go` finds signal handling, Shutdown and Serve calls and works out deadlines; `manifest.go` reads workloads; `check.go` joins them into findings; `draingap.go` is the CLI. `yaml_*.go` is go.yaml.in/yaml/v2 v2.4.2 copied with the package renamed. `plugin/skills/checking-go-graceful-shutdown-on-kubernetes/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet and tests with `-race`. The tests build and run real programs and send them SIGTERM.
- `make lint`: plugin and marketplace manifest validation (needs Claude Code)
- `make demo`: run the three README examples

## Conventions
- Standard library only. Never run the program being checked; `go list -export` compiling it is the limit.
- Each check has a stable `DGnnn` code: DG1xx signal, DG2xx program, DG3xx time budget, DG4xx command, DG9xx notes. Don't reuse codes. Every code is in the README table and the skill.
- A value draingap can't work out is reported as unknown, never guessed.
- Input it can't trust exits 2 with nothing on stdout, under every `--fail-on` value. Each new kind of input needs a test for empty, garbage, cut off and failed-command output.

## Do and don't
- Do add a program to `tests/testdata/progs` for each new program check. `TestRealShutdown` must agree with it.
- Do run a new check on a real project and read what it flags before adding it.
- Don't say a service shuts down gracefully. Say what was checked and what wasn't.
- Don't edit `yaml_*.go` except to update the copied version.

## Definition of done
`make test` and `make lint` pass, README examples match real output (`TestReadme`), and `ROADMAP.md`, `CHANGELOG.md` and docs are updated when relevant.
