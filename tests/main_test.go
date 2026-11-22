// Package tests runs draingap as a built binary against real Go programs
// and manifests.
package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var (
	tool string // path of the built draingap binary
	root string // repository root
)

func TestMain(m *testing.M) {
	os.Exit(setup(m))
}

func setup(m *testing.M) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	root = filepath.Dir(wd)
	dir, err := os.MkdirTemp("", "draingap-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	tool = filepath.Join(dir, "draingap"+exeSuffix())
	cmd := exec.Command("go", "build", "-o", tool, "./plugin/scripts")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building draingap: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

type result struct {
	stdout, stderr string
	code           int
}

// draingap runs the tool from the repository root.
func draingap(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(tool, args...)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running draingap: %v", err)
		}
		code = exit.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

// codes returns the sorted finding codes in text output, such as
// "error DG201".
func codes(out string) []string {
	var cs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "ERROR" || f[0] == "WARN" || f[0] == "NOTE") && strings.HasPrefix(f[1], "DG") {
			cs = append(cs, strings.ToLower(f[0])+" "+f[1])
		}
	}
	sort.Strings(cs)
	return cs
}

func prog(name string) string {
	return "./tests/testdata/progs/" + name
}

func manifest(name string) string {
	return "tests/testdata/manifests/" + name
}

// writeTemp writes data to a new file in a test temp dir and returns
// its path.
func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
