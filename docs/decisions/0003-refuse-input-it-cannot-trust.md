# 0003: Refuse input it can't trust, with exit 2

**Decision:** draingap exits 2 and prints nothing on stdout when a manifest is empty, not YAML or JSON, a command's error output, unrendered, cut off, or has no workload, and when the program doesn't build for linux, isn't a main package or has no `func main`. `--fail-on` doesn't change this, including `--fail-on never`.

**Why:** a check that prints "no problems found" for a failed `helm template` or a half-written file is worse than no check. A YAML file cut at a line boundary is often still valid YAML, so a file that ends inside a workload must end with a `---` line to be read; the skill and README show how to add it when rendering. JSON that is cut anywhere doesn't decode. Go files are compiled by `go list -export` first, so a cut file is refused unless it still builds as a program, which the tests check by cutting a real `main.go` every 24 bytes.

**Not covered:** a complete, valid manifest for the wrong environment, or source from a different commit than the image. draingap prints the file and workload it read so a person can see that.
