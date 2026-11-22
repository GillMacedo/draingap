# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/draingap/security/advisories/new), not in a public issue.

`draingap` runs `go list -deps -export` in the `--pkg` directory and reads Go source files and the manifest files you name. `go list -export` compiles packages into the Go build cache and, like `go build`, may download modules that aren't in the module cache or `vendor/`; with `CGO_ENABLED=1` it runs the C compiler. draingap doesn't run your program, write to your files, or connect to a cluster. Workload names, container names, env values used for deadlines and file paths are printed in findings, so don't share its output if those hold anything private.

A missed or wrong finding is a normal bug. If `draingap` changes a file, runs code from the module, or reports a clean result for a program or manifest it couldn't read, report it privately. `draingap` is a diagnostic aid and doesn't enforce anything.
