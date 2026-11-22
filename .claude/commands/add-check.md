Add a draingap check for: $ARGUMENTS

1. Write the smallest Go program that shows the problem as `tests/testdata/progs/<name>/main.go`, with the same `-addr` flag and `/slow` handler as `tests/testdata/progs/healthy`. Write a fixed version too if none of the existing programs is one.
2. Add both to `progCases` in `tests/cases_test.go` with the codes they should get. `TestRealShutdown` runs each program, sends SIGTERM during a slow request, and fails unless programs with an error-level program finding really lose the request and the others really drain it. A manifest-only check goes in `manifestCases` in `tests/manifest_test.go` instead.
3. Use the next unused code in its group (DG1xx signal, DG2xx program, DG3xx time budget, DG4xx command). Never reuse a code.
4. Change `plugin/scripts`, add the code to the README table and the skill's list, then run `make test`.
