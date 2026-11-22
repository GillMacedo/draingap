# draingap: check that a Go service drains its requests before Kubernetes kills it

[![CI](https://github.com/GillMacedo/draingap/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/draingap/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for Go HTTP services on Kubernetes that drop requests during rollouts, scale-downs and node drains. It reads the program's source and its rendered manifests together, and reports why requests in flight get cut: SIGTERM not handled, `http.Server.Shutdown` given a context that is already canceled, `main` exiting while Shutdown drains, and a preStop delay plus Shutdown deadline that don't fit in `terminationGracePeriodSeconds`.**

When Kubernetes stops a pod it runs the preStop hook, sends SIGTERM (or the container's stop signal), and sends SIGKILL when the grace period ends, 30 seconds by default, counted from the start of preStop. A Go program that doesn't handle SIGTERM dies at once. One that handles only `os.Interrupt` (SIGINT) dies at once too. One that passes the `signal.NotifyContext` context to `Shutdown` gets a context that is already done, so `Shutdown` closes the listeners and returns without waiting. One that writes `log.Fatal(srv.ListenAndServe())` exits the moment Shutdown starts, because `ListenAndServe` returns `http.ErrServerClosed` right away; the net/http docs warn about this. And a 30s Shutdown deadline behind a 10s preStop sleep can't finish inside a 30s grace period. All of these compile, pass tests, and show up only as 502s and connection resets during deploys.

Linters look at one side. `kube-linter` and `kube-score` read only the manifests, and Go linters read only the program. draingap reads both: it follows the Shutdown deadline from `context.WithTimeout` to a constant, a `flag.Duration` default, or `os.Getenv`, then looks up that flag or variable in the container's `args` and `env` to get the value the pod really runs with.

## What you can ask Claude

- "Our Go API returns 502s during every rollout. Does it shut down gracefully?"
- "Is terminationGracePeriodSeconds long enough for this service's preStop hook and shutdown timeout?"
- "Review this PR that changes the shutdown code in cmd/server/main.go."
- "Why do our pods always take the full 30 seconds to terminate?"
- "Check the Helm chart's preStop and grace period against the Go code before we raise the shutdown timeout."

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/draingap
    claude plugin install draingap@draingap-marketplace

Or run it with Go 1.22 or newer. It has no dependencies outside the standard library:

    go run github.com/GillMacedo/draingap/plugin/scripts@v0.1.0 --pkg ./cmd/server rendered.yaml

## Example

`examples/orders` is a small service that shuts down correctly, and `examples/orders/deploy.yaml` gives it a 5s preStop sleep and a 30s grace period. The healthy case stays quiet and prints the timeline:

    $ go run ./plugin/scripts --pkg ./examples/orders examples/orders/deploy.yaml
    Deployment/orders container orders (examples/orders/deploy.yaml)
      stop signal   SIGTERM (the default, unless the image sets STOPSIGNAL)
      grace period  30s (terminationGracePeriodSeconds)
      preStop       5s (sleep action 5s)
      sleep in code 0s
      Shutdown      20s (default of flag -shutdown-timeout, unless the image's ENTRYPOINT sets it)
      needs         25s of 30s, 5s spare
      entrypoint    the image's ENTRYPOINT, not checked

    draingap: github.com/GillMacedo/draingap/examples/orders, 1 container: no problems found

`deploy-tight.yaml` runs the same program with `-shutdown-timeout=30s`, a 10s preStop and the default grace period. `--fail-on never` keeps the exit code at 0 for this page; without it, the run exits 1:

    $ go run ./plugin/scripts --fail-on never --pkg ./examples/orders examples/orders/deploy-tight.yaml
    Deployment/orders container orders (examples/orders/deploy-tight.yaml)
      stop signal   SIGTERM (the default, unless the image sets STOPSIGNAL)
      grace period  30s (terminationGracePeriodSeconds not set, so the default)
      preStop       10s (exec sleep 10, needs a sleep binary in the image)
      sleep in code 0s
      Shutdown      30s (flag -shutdown-timeout=30s in the container args)
      needs         40s of 30s, 10s over
      entrypoint    the image's ENTRYPOINT, not checked

    ERROR DG301 [Deployment/orders container orders]
      the shutdown can't finish inside the grace period: preStop 10s + sleep in code 0s + Shutdown deadline 30s (flag -shutdown-timeout=30s in the container args) = 40s, but Kubernetes sends SIGKILL 30s after it starts stopping the pod (terminationGracePeriodSeconds not set, so the default)
      fix: raise terminationGracePeriodSeconds above 40s, or shorten the preStop delay or the Shutdown deadline

    draingap: github.com/GillMacedo/draingap/examples/orders, 1 container: 1 error, 0 warnings, 0 notes

`examples/billing` has three bugs in eight lines of shutdown code, and its Deployment starts it through `sh -c`:

    $ go run ./plugin/scripts --fail-on never --pkg ./examples/billing examples/billing/deploy.yaml
    Deployment/billing container billing (examples/billing/deploy.yaml)
      stop signal   SIGTERM (the default, unless the image sets STOPSIGNAL)
      grace period  20s (terminationGracePeriodSeconds)
      preStop       0s (none)
      sleep in code 0s
      Shutdown      returns at once, its context is already canceled
      needs         not checked: the program doesn't handle SIGTERM, or draingap couldn't tell
      entrypoint    command in the manifest

    ERROR DG102 examples/billing/main.go:29 [Deployment/billing container billing]
      the program listens for SIGINT but not SIGTERM, so the signal Kubernetes sends kills it at once with requests in flight
      fix: add syscall.SIGTERM to the signal.Notify or signal.NotifyContext call
    ERROR DG201 examples/billing/main.go:37
      Shutdown gets a context derived from signal.NotifyContext, which is already canceled once the signal arrives, so Shutdown closes the listeners and returns at once without waiting for requests in flight
      fix: build the shutdown context from context.Background() (or context.WithoutCancel(ctx)) with context.WithTimeout
    ERROR DG202 examples/billing/main.go:42
      the program exits while Shutdown is still draining: log.Fatal exits with the ErrServerClosed that Serve returns when Shutdown starts
      fix: treat http.ErrServerClosed as a normal return, then wait for the goroutine that calls Shutdown to finish (for example on a channel) before main returns
    WARN DG401 [Deployment/billing container billing]
      the container runs sh -c "/app/migrate up && /app/billing -addr=:8080", so the shell is PID 1 and gets the stop signal; whether the program gets it depends on the image's shell (bash 5.2 replaces itself with the last command, dash 0.5.12 does not)
      fix: start the program with exec (sh -c '... && exec /app'), or set command to the program itself

    draingap: github.com/GillMacedo/draingap/examples/billing, 1 container: 3 errors, 1 warning, 0 notes

The tests run both programs for real: they start a request that takes 1.5s, send SIGTERM while it runs, and check that `orders` finishes it and `billing` cuts it. Add `--format json` for machine-readable output. `examples/README.md` says how the examples were made.

## Use it on your service

Render the manifests first. draingap reads rendered YAML or JSON, never a chart or kustomization directory:

    helm template api ./chart -f values-prod.yaml > rendered.yaml && echo --- >> rendered.yaml
    go run github.com/GillMacedo/draingap/plugin/scripts@v0.1.0 --pkg ./cmd/api --container api rendered.yaml

The final `---` tells draingap the file is complete, and `&&` writes it only when `helm` succeeds; see [When it refuses](#when-it-refuses). `kubectl get deploy api -o json` output works too. `--container` names the containers that run this program, as `name` or `Kind/workload/name`, and can be left out when there is only one. If the image's Dockerfile sets `STOPSIGNAL`, pass it with `--stop-signal`. Pass `--tags` with your release build tags.

In CI, run it after rendering, so a PR that changes the shutdown code, the grace period, the preStop hook or the container command fails when they stop fitting together:

    - run: helm template api ./chart > rendered.yaml && echo --- >> rendered.yaml
    - run: go run github.com/GillMacedo/draingap/plugin/scripts@v0.1.0 --pkg ./cmd/api rendered.yaml

## What it checks

| Code | Level | Meaning |
|---|---|---|
| DG101 | error | Nothing calls `signal.Notify` or `signal.NotifyContext`, so the stop signal kills the program at once |
| DG102 | error | The program listens for other signals (usually `os.Interrupt`, which is SIGINT) but not the stop signal |
| DG103 | error | The program ignores the stop signal with `signal.Ignore`, or catches it on a channel nothing receives from, so it runs until SIGKILL |
| DG104 | note | A signal list draingap couldn't read, so the signal check wasn't done |
| DG105 | note | No `os/signal` call in the module, but a dependency has one, so a framework may handle the signal |
| DG106 | note | No `net/http` server in the module's own code (gin, echo, gRPC and other frameworks aren't followed), so only the signal and the manifest were checked |
| DG201 | error | `Shutdown` gets a context from `signal.NotifyContext`, or derived from it, which is already canceled, so it doesn't wait |
| DG202 | error | The program exits while Shutdown drains: `log.Fatal`, `os.Exit` or `panic` on the `ErrServerClosed` that Serve returns, or `main` returning right after Serve with nothing that waits, also through a `run()` function |
| DG203 | warn | `Shutdown(context.Background())`: no deadline, so a stuck connection holds the program until SIGKILL and nothing after Shutdown runs |
| DG204 | error | An `http.Server` is started and nothing calls `Shutdown` (only `Close`, or nothing) |
| DG205 | error | `http.ListenAndServe` and friends, which have no server value to shut down |
| DG206 | note | The Shutdown deadline couldn't be worked out, so the time budget wasn't checked |
| DG207 | warn | One server is shut down and another, such as a metrics server, never is |
| DG301 | error | preStop + sleep in code before Shutdown + Shutdown deadline is longer than the grace period |
| DG302 | warn | They add up to exactly the grace period, so cleanup after Shutdown races SIGKILL |
| DG303 | error | The preStop hook alone takes the whole grace period |
| DG304 | warn | No preStop delay and no sleep before Shutdown, so the listener closes while traffic may still be routed to the pod |
| DG305 | error | `terminationGracePeriodSeconds: 0` |
| DG401 | warn | The container runs the program through `sh -c` and the script doesn't end in `exec`, so the shell may be PID 1 and get the signal in its place |
| DG403 | note | A preStop hook or sleep whose duration isn't known (`httpGet`, a script); known parts count as a lower bound |
| DG901 | note | A Shutdown, Serve or `os/signal` call in a function nothing reachable from `main` calls, which was left out |

Where the numbers come from:

- **Stop signal:** `lifecycle.stopSignal`, else `--stop-signal`, else SIGTERM.
- **Grace period:** `terminationGracePeriodSeconds` in the pod spec, else 30s.
- **preStop:** the `sleep` action, `exec` of `sleep N`, or `sh -c 'sleep N'`. Other hooks are unknown, with any `sleep` in them as a lower bound.
- **Sleep in code:** `time.Sleep` and `<-time.After` before the Shutdown call in the same function.
- **Shutdown deadline:** the `context.WithTimeout` behind the context passed to `(*http.Server).Shutdown`, followed through variables, struct fields, function parameters (all callers must agree) and interface methods. A constant is used as is. A `flag` or `pflag` duration uses the value in the container's `command` or `args` (also inside `sh -c`, and after `$(VAR)` expansion), else its default. `time.ParseDuration(os.Getenv("X"))` uses the container's `env`, with a constant fallback in the code if there is one. A field with a struct tag, or of a struct passed by pointer to a call such as `viper.Unmarshal(&cfg)`, is unknown, because a decoder may set it.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing at or above `--fail-on` (default `error`), or `--fail-on never` |
| 1 | At least one finding at or above `--fail-on` (`error`, `warn` or `note`) |
| 2 | The input can't be trusted, or the flags are wrong. Stdout is empty, whatever `--fail-on` says |

## When it refuses

draingap exits 2 and prints only the reason on stderr when it can't trust what it read. For a manifest with its last 30 bytes cut off:

    draingap: nothing was checked, because the input can't be trusted: deploy.yaml ends inside a workload without a final --- line, so it may be cut off: if the file is complete, add a line with --- at the end (with helm: helm template ... > rendered.yaml && echo --- >> rendered.yaml)

A manifest is refused when it is missing, empty, white space or comments only, binary, not YAML or JSON, cut off (a YAML file that ends inside a workload with no final `---`, or JSON that doesn't close), the error output of `helm`, `kubectl` or `go build`, an unrendered template, has no workload with containers, or has values the API server rejects (a grace period that is a string, negative or a fraction; a command that isn't a list; an env value that isn't a string; an unknown stop signal). The program is refused when the directory is missing, empty, outside a module, not a `main` package, has no `func main`, uses cgo, or doesn't build for linux, including a Go file that is empty, garbage, cut off, or a build log. These checks run before anything is reported, under every `--fail-on` value and both formats.

## Limits

- **net/http only.** gRPC's `GracefulStop`, and servers that frameworks start and stop for you (gin's `Run`, echo's `Start`, fx, kratos), aren't followed. DG106 says so when no `net/http` server is visible.
- **The shutdown path, not the handlers.** A handler that ignores its request context, a background worker, a message consumer or a database pool closed too early can still lose work.
- **Static.** draingap doesn't run the program. Values set at run time in ways it can't follow (viper, config files, a computed duration) are reported as unknown, never guessed. When several Shutdown calls use different deadlines, the longest counts.
- **The image isn't read.** Its `ENTRYPOINT`, `CMD` and `STOPSIGNAL`, and whether it has the `sleep` binary or shell an `exec` preStop needs (distroless and scratch images don't), are listed as not checked. Use the `sleep` action when the image has no shell.
- **Linux.** The program is read as `GOOS=linux` with `CGO_ENABLED=0` (unless you set `CGO_ENABLED`), the way a container image is usually built.
- **Traffic routing.** DG304 says a delay is missing; it doesn't measure how long your ingress or load balancer takes to stop routing to a terminating pod.

## FAQ

**Why does my Go service return 502 or connection refused during Kubernetes rolling updates?** Usually one of: the program doesn't handle SIGTERM and dies at once; it closes its listener the moment SIGTERM arrives while traffic is still being routed to the pod (add a preStop sleep); `Shutdown` doesn't wait because its context is already canceled; or `main` exits before Shutdown finishes. draingap checks all four.

**Does Go handle SIGTERM by default?** No. Without `signal.Notify` or `signal.NotifyContext` for `syscall.SIGTERM`, a Go program exits immediately on SIGTERM. `os.Interrupt` is SIGINT, which Kubernetes doesn't send unless `STOPSIGNAL` or `lifecycle.stopSignal` says so.

**Why does Shutdown return immediately?** The context passed to `Shutdown` is already done. That happens when it comes from `signal.NotifyContext`, which is canceled when the signal arrives, or is derived from it with `context.WithTimeout(ctx, ...)`. Build the shutdown context from `context.Background()` or `context.WithoutCancel(ctx)`.

**How long should terminationGracePeriodSeconds be?** Longer than the preStop delay plus any sleep before Shutdown plus the Shutdown deadline, with a few seconds left for cleanup after Shutdown. The grace period starts when preStop starts, so preStop time counts against it.

**Does `sh -c` break graceful shutdown?** It can. The shell becomes PID 1 and gets the signal. On the machine these tests ran on, bash 5.2 replaced itself with the last command of a `-c` script and dash 0.5.12 didn't, so with dash the program never got the signal. End the script with `exec your-program`.

**Does draingap run my code or connect to my cluster?** No. It runs `go list -deps -export`, which compiles packages into the build cache and may download modules, and it reads files.

**Is it an MCP server?** No. It's a skill with one Go program, so `go run` or `go build` is all it needs.

## Tested on

- **Real shutdowns.** `tests/testdata/progs` has 25 small programs, one per pattern, healthy and broken. `TestRealShutdown` builds and runs each one, starts a 1.5s request, sends SIGTERM once the handler is running, and checks what happened: every program with an error-level program finding (DG1xx, DG2xx) really lost the request or kept running after SIGTERM, and every other program really finished the request and exited. The same test runs `examples/orders` and `examples/billing`.
- **Manifests.** 27 manifest cases for grace periods, preStop forms, stop signals, shell wrappers, flags in `args` and in scripts, `$(VAR)`, env values, fallbacks and Secrets; StatefulSet, CronJob, Pod, a JSON List and a multi-document file; container selection.
- **Input it can't trust.** 28 bad manifests and 12 bad programs, each with the other side real, under every `--fail-on` value and both formats. Every byte-prefix of four real manifests, and a cut every 24 bytes of a real `main.go`, must exit 2 with nothing on stdout.
- **Real projects, by hand,** with Go 1.27.4 on linux/arm64:
  - stefanprodan/podinfo at f72ffd4 with `kustomize/deployment.yaml`: no errors. It found the SIGTERM handler through a signal list in another package and the 3s sleep before Shutdown, and reported the deadline as unknown because `viper.Unmarshal` fills it. The first version flagged DG101 here by mistake (objects across packages didn't match) and read a 5s deadline from a mock; both are fixed and covered by tests.
  - GoogleCloudPlatform/microservices-demo `src/frontend` at c7cb6d5 with `kubernetes-manifests/frontend.yaml`: DG205 for `http.ListenAndServe` at `main.go:171` and DG101, since nothing handles SIGTERM.
- **Real projects, again:** `tests/live_test.go` reruns both when `DRAINGAP_PODINFO` and `DRAINGAP_BOUTIQUE` point at checkouts.
- **Go versions.** The whole suite passes on Go 1.22.12 and 1.27.4, with `-race`.
- **The installed plugin.** A test copies `plugin/` out of the repository and builds it with the skill's own command.

Not tested: a real Kubernetes cluster (the timeline follows the Kubernetes docs on pod termination and container hooks); PID 1 signal behavior inside a container (the container these tests ran in can't create PID namespaces); busybox and Alpine shells; macOS and Windows; cgo programs; a fresh Claude Code session using the skill. The `go run ...@v0.1.0` lines work only once the repository is published.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/draingap/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `draingap` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
