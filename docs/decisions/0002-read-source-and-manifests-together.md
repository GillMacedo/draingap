# 0002: Read the source and the manifests together, statically

**Decision:** draingap type-checks the program and reads rendered manifests, and joins them: the Shutdown deadline found in code is looked up in the container's `args` and `env`, and the signal check uses the container's stop signal. It doesn't run the program.

**Why:** each half alone misses the common failures. A manifest linter can't see that the program handles only SIGINT or passes a canceled context to Shutdown; a Go linter can't see that a 30s deadline sits behind a 10s preStop in a 30s grace period. Running the program would need its databases and secrets, and a cluster to measure routing, which a check in CI doesn't have.

**Cost:** values set by viper, config files or computed durations can't be followed. They are reported as unknown, and the budget is left unchecked rather than guessed. The real-SIGTERM tests in `tests/drain_test.go` keep the program checks honest about what Go does.
