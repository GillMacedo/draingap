package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReadme runs every "$ " line in the README's indented code blocks
// from the repository root and compares its output with the lines that
// follow, the way a reader would see them.
func TestReadme(t *testing.T) {
	lines := strings.Split(string(readFile(t, "README.md")), "\n")
	found := 0
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "    $ ") {
			continue
		}
		cmdline := strings.TrimPrefix(lines[i], "    $ ")
		var want []string
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if l == "" && j+1 < len(lines) && strings.HasPrefix(lines[j+1], "    ") && !strings.HasPrefix(lines[j+1], "    $ ") {
				want = append(want, "")
				continue
			}
			if !strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "    $ ") {
				break
			}
			want = append(want, strings.TrimPrefix(l, "    "))
		}
		found++
		const prefix = "go run ./plugin/scripts "
		if !strings.HasPrefix(cmdline, prefix) {
			t.Errorf("README command this test doesn't know how to run: %s", cmdline)
			continue
		}
		cmd := exec.Command(tool, strings.Fields(strings.TrimPrefix(cmdline, prefix))...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v (README commands must exit 0)", cmdline, err)
		}
		if got := strings.TrimRight(string(out), "\n"); got != strings.TrimRight(strings.Join(want, "\n"), "\n") {
			t.Errorf("%s: README output differs.\n--- got\n%s\n--- README\n%s", cmdline, got, strings.Join(want, "\n"))
		}
	}
	if found < 3 {
		t.Errorf("found %d commands in the README, expected at least 3", found)
	}
}

// TestReadmeRefusal checks the refusal message the README quotes.
func TestReadmeRefusal(t *testing.T) {
	full := readFile(t, "examples/orders/deploy.yaml")
	p := writeTemp(t, "deploy.yaml", full[:len(full)-30])
	r := draingap(t, "--pkg", "./examples/orders", p)
	got := strings.TrimSpace(strings.ReplaceAll(r.stderr, p, "deploy.yaml"))
	if r.code != 2 || !strings.Contains(string(readFile(t, "README.md")), "    "+got+"\n") {
		t.Errorf("exit %d; the README doesn't quote this message:\n%s", r.code, got)
	}
}
