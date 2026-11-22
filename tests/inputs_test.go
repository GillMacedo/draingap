package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refused checks that draingap printed nothing on stdout, said nothing
// was checked, and exited 2, with every --fail-on level and both formats.
func refused(t *testing.T, why string, args ...string) {
	t.Helper()
	for _, extra := range [][]string{nil, {"--fail-on", "error"}, {"--fail-on", "warn"}, {"--fail-on", "note"}, {"--fail-on", "never"}, {"--format", "json"}} {
		r := draingap(t, append(append([]string(nil), extra...), args...)...)
		if r.code != 2 {
			t.Errorf("%s %v: exit %d, want 2\nstdout: %s\nstderr: %s", why, extra, r.code, r.stdout, r.stderr)
		}
		if r.stdout != "" {
			t.Errorf("%s %v: printed a report for input it can't trust:\n%s", why, extra, r.stdout)
		}
		if !strings.Contains(r.stderr, "nothing was checked") {
			t.Errorf("%s %v: stderr doesn't say nothing was checked:\n%s", why, extra, r.stderr)
		}
	}
}

const helmError = `Error: INSTALLATION FAILED: template: api/templates/deployment.yaml:31:28: executing "api/templates/deployment.yaml" at <.Values.shutdown.grace>: nil pointer evaluating interface {}.grace
`

const kubectlError = `error: the server doesn't have a resource type "deployments"
`

const goBuildError = `# example.com/api
./main.go:12:2: undefined: srv
./main.go:20:15: cannot use ctx (variable of type context.Context) as string value in argument to log.Print
`

// Bad manifests, each with the real healthy program.
func TestUntrustedManifests(t *testing.T) {
	cases := map[string]string{
		"empty":                  "",
		"whitespace":             " \n\n\t\n",
		"comments only":          "# Source: api/templates/deployment.yaml\n---\n# nothing\n",
		"plain text":             "hello world\n",
		"helm error":             helmError,
		"kubectl error":          kubectlError,
		"go build error":         goBuildError,
		"binary":                 "\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00",
		"service only":           "apiVersion: v1\nkind: Service\nmetadata:\n  name: api\nspec:\n  ports:\n    - port: 80\n---\n",
		"no kind":                "metadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n---\n",
		"unrendered":             deployment("terminationGracePeriodSeconds: {{ .Values.grace }}", ""),
		"unrendered if":          strings.Replace(deployment("", ""), "      containers:\n", "      {{- if .Values.grace }}\n      terminationGracePeriodSeconds: 30\n      {{- end }}\n      containers:\n", 1),
		"grace as string":        deployment("terminationGracePeriodSeconds: \"30\"", ""),
		"grace negative":         deployment("terminationGracePeriodSeconds: -1", ""),
		"grace fraction":         deployment("terminationGracePeriodSeconds: 2.5", ""),
		"containers not a list":  strings.Replace(deployment("", ""), "      containers:\n        - name: api\n          image: registry.example.com/api:1.0.0\n", "      containers: api\n", 1),
		"unknown stop signal":    deployment("", "lifecycle:\n  stopSignal: SIGFOO"),
		"env value not a string": deployment("", "env:\n  - name: SHUTDOWN_TIMEOUT\n    value: 30"),
		"command not a list":     deployment("", "command: /app/api"),
		"json cut off":           `{"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "apps/v1", "kind": "Deployment"`,
		"json then garbage":      `{"apiVersion": "v1", "kind": "List", "items": []} trailing`,
		"json not an object":     `["a", "b"]`,
		"json empty list":        `{"apiVersion": "v1", "kind": "List", "items": []}`,
		"no final marker":        strings.TrimSuffix(deployment("", ""), "---\n"),
		"yaml error":             "apiVersion: apps/v1\nkind: Deployment\n  metadata: : :\n---\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTemp(t, "manifest.yaml", []byte(body))
			refused(t, name, "--pkg", prog("healthy"), p)
		})
	}
	t.Run("missing file", func(t *testing.T) {
		refused(t, "missing file", "--pkg", prog("healthy"), filepath.Join(t.TempDir(), "nope.yaml"))
	})
	t.Run("directory", func(t *testing.T) {
		refused(t, "directory", "--pkg", prog("healthy"), t.TempDir())
	})
	t.Run("a good file and an empty one", func(t *testing.T) {
		p := writeTemp(t, "empty.yaml", nil)
		refused(t, "good plus empty", "--pkg", prog("healthy"), manifest("basic.yaml"), p)
	})
}

// Every cut-off copy of a real manifest must be refused, whatever byte
// it stops at.
func TestCutManifests(t *testing.T) {
	for _, name := range []string{"basic.yaml", "with-service.yaml", "list.json", "../../../examples/orders/deploy.yaml"} {
		full := readFile(t, filepath.Join("tests/testdata/manifests", name))
		whole := bytes.TrimRight(full, " \n")
		dir := t.TempDir()
		checked := 0
		for i := 0; i < len(full); i++ {
			cut := full[:i]
			if bytes.Equal(bytes.TrimRight(cut, " \n"), whole) {
				continue // only trailing whitespace is missing
			}
			p := filepath.Join(dir, "cut.yaml")
			if err := os.WriteFile(p, cut, 0o644); err != nil {
				t.Fatal(err)
			}
			r := draingap(t, "--pkg", prog("healthy"), "--fail-on", "note", p)
			checked++
			if r.code != 2 || r.stdout != "" {
				t.Errorf("%s cut at byte %d of %d: exit %d, want 2\n%q\nstdout: %s", name, i, len(full), r.code, cut, r.stdout)
				break
			}
		}
		if checked < 100 {
			t.Errorf("%s: only %d cuts checked", name, checked)
		}
	}
}

const healthyMain = "tests/testdata/progs/healthy/main.go"

// module writes a temporary Go module with the given main.go.
func module(t *testing.T, mainGo string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/api\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mainGo != "" {
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Bad programs, each with the real basic manifest.
func TestUntrustedPrograms(t *testing.T) {
	real := string(readFile(t, healthyMain))
	cases := map[string]string{
		"empty file":     "",
		"garbage":        "this is not Go\n",
		"go build error": goBuildError,
		"helm error":     helmError,
		"library":        "package api\n\nfunc Hello() string { return \"hi\" }\n",
		"compile error":  strings.Replace(real, "srv.Shutdown(sctx)", "srv.Shutdown(sctx, 1)", 1),
		"no main func":   "package main\n\nimport \"net/http\"\n\nvar _ = http.ListenAndServe\n",
		"cgo":            "package main\n\nimport \"C\"\n\nfunc main() {}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			refused(t, name, "--pkg", module(t, src), manifest("basic.yaml"))
		})
	}
	t.Run("empty dir", func(t *testing.T) {
		refused(t, "empty dir", "--pkg", module(t, ""), manifest("basic.yaml"))
	})
	t.Run("missing dir", func(t *testing.T) {
		refused(t, "missing dir", "--pkg", filepath.Join(t.TempDir(), "nope"), manifest("basic.yaml"))
	})
	t.Run("not a module", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(real), 0o644); err != nil {
			t.Fatal(err)
		}
		refused(t, "not a module", "--pkg", dir, manifest("basic.yaml"))
	})
	t.Run("file instead of dir", func(t *testing.T) {
		refused(t, "file", "--pkg", healthyMain, manifest("basic.yaml"))
	})
}

// Every cut-off copy of a real program must be refused.
func TestCutPrograms(t *testing.T) {
	full := readFile(t, healthyMain)
	whole := bytes.TrimSpace(full)
	dir := module(t, "")
	step := len(full) / 40
	for i := 0; i < len(full); i += step {
		cut := full[:i]
		if bytes.Equal(bytes.TrimSpace(cut), whole) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), cut, 0o644); err != nil {
			t.Fatal(err)
		}
		r := draingap(t, "--pkg", dir, "--fail-on", "note", manifest("basic.yaml"))
		if r.code != 2 || r.stdout != "" {
			t.Errorf("main.go cut at byte %d of %d: exit %d, want 2\nstdout: %s", i, len(full), r.code, r.stdout)
		}
	}
}

func TestBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--pkg", prog("healthy")},
		{manifest("basic.yaml")},
		{"--pkg", prog("healthy"), "--fail-on", "always", manifest("basic.yaml")},
		{"--pkg", prog("healthy"), "--format", "xml", manifest("basic.yaml")},
		{"--pkg", prog("healthy"), "--bogus", manifest("basic.yaml")},
	} {
		r := draingap(t, args...)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%v: exit %d, want 2\n%s", args, r.code, r.stdout)
		}
	}
}
