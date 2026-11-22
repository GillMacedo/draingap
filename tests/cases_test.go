package tests

import (
	"reflect"
	"strings"
	"testing"
)

// Each program in testdata/progs, checked against a manifest with a 30s
// grace period and a 5s preStop sleep.
var progCases = []struct {
	prog string
	want []string
	exit int
}{
	{"healthy", nil, 0},
	{"notifyall", nil, 0},
	{"signalslice", nil, 0},
	{"waitsafter", nil, 0},
	{"withoutcancel", nil, 0},
	{"sigintonly", []string{"error DG102"}, 1},
	{"nosignal", []string{"error DG101", "error DG204"}, 1},
	{"ignored", []string{"error DG103"}, 1},
	{"canceledctx", []string{"error DG201"}, 1},
	{"derivedctx", []string{"error DG201"}, 1},
	{"fatalserve", []string{"error DG202"}, 1},
	{"iffatal", []string{"error DG202"}, 1},
	{"mainreturns", []string{"error DG202"}, 1},
	{"runreturns", []string{"error DG202"}, 1},
	{"pkgfunc", []string{"error DG205"}, 1},
	{"closeonly", []string{"error DG204"}, 1},
	{"nodeadline", []string{"warn DG203"}, 0},
	{"flagtimeout", []string{"warn DG302"}, 0},
	{"sleepfirst", []string{"error DG301"}, 1},
	{"envtimeout", nil, 0},
	{"deadshutdown", []string{"error DG204", "note DG901"}, 1},
	{"unreadchan", []string{"error DG103", "error DG301"}, 1},
	{"twoservers", []string{"warn DG207"}, 0},
	{"viainterface", nil, 0},
	{"noserver", []string{"note DG106"}, 0},
}

func TestPrograms(t *testing.T) {
	for _, c := range progCases {
		t.Run(c.prog, func(t *testing.T) {
			r := draingap(t, "--pkg", prog(c.prog), manifest("basic.yaml"))
			if got := codes(r.stdout); !reflect.DeepEqual(got, c.want) {
				t.Errorf("codes = %v, want %v\n%s%s", got, c.want, r.stdout, r.stderr)
			}
			if r.code != c.exit {
				t.Errorf("exit = %d, want %d\n%s%s", r.code, c.exit, r.stdout, r.stderr)
			}
			if c.want == nil && !strings.Contains(r.stdout, "no problems found") {
				t.Errorf("a clean program should say no problems found:\n%s", r.stdout)
			}
			if c.want != nil && strings.Contains(r.stdout, "no problems found") {
				t.Errorf("a program with findings says no problems found:\n%s", r.stdout)
			}
		})
	}
}

func TestFailOnWarn(t *testing.T) {
	r := draingap(t, "--pkg", prog("nodeadline"), "--fail-on", "warn", manifest("basic.yaml"))
	if r.code != 1 {
		t.Errorf("--fail-on warn with a warning: exit %d, want 1", r.code)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--fail-on", "note", manifest("prestop-http.yaml"))
	if r.code != 1 {
		t.Errorf("--fail-on note with a note: exit %d, want 1", r.code)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--fail-on", "note", manifest("basic.yaml"))
	if r.code != 0 {
		t.Errorf("--fail-on note on a clean program: exit %d, want 0\n%s", r.code, r.stdout)
	}
}

func TestFailOnNever(t *testing.T) {
	r := draingap(t, "--pkg", "./examples/billing", "--fail-on", "never", "examples/billing/deploy.yaml")
	if r.code != 0 || !strings.Contains(r.stdout, "3 errors") {
		t.Errorf("--fail-on never with errors: exit %d\n%s", r.code, r.stdout)
	}
}

func TestPositions(t *testing.T) {
	r := draingap(t, "--pkg", prog("runreturns"), manifest("basic.yaml"))
	if !strings.Contains(r.stdout, "ERROR DG202 tests/testdata/progs/runreturns/main.go:40") {
		t.Errorf("DG202 should point at the log.Fatal in main:\n%s", r.stdout)
	}
	r = draingap(t, "--pkg", prog("canceledctx"), manifest("basic.yaml"))
	if !strings.Contains(r.stdout, "ERROR DG201 tests/testdata/progs/canceledctx/main.go:48") {
		t.Errorf("DG201 should point at the Shutdown call:\n%s", r.stdout)
	}
}
