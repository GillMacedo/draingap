# 0001: A skill with one Go program

**Decision:** draingap ships as a Claude Code skill whose script is one standard-library Go `package main`, with YAML parsing copied in. It isn't an MCP server.

**Why:** the check needs `go/types` and the go command, which Go users already have. An agent gets everything it needs by running the program and reading its text or JSON output, so a server process adds nothing. One package with no dependencies builds from the installed plugin directory with `go build` and works offline.

**Cost:** the copied YAML parser is about 10,000 lines that have to be updated by hand.
