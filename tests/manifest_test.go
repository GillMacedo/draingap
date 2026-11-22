package tests

import (
	"bytes"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// deployment builds a one-container Deployment. podExtra is indented
// under the pod spec and ctrExtra under the container.
func deployment(podExtra, ctrExtra string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
` + indent(podExtra, "      ") + `      containers:
        - name: api
          image: registry.example.com/api:1.0.0
` + indent(ctrExtra, "          ") + "---\n"
}

func indent(s, pre string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(pre + l + "\n")
	}
	return b.String()
}

// The healthy program has a 10s Shutdown deadline; flagtimeout has a
// -shutdown-timeout flag with a 25s default; envtimeout reads
// $SHUTDOWN_TIMEOUT.
var manifestCases = []struct {
	name     string
	prog     string
	pod, ctr string
	want     []string
	needs    string // expected "needs" line content
}{
	{"default grace, no preStop", "healthy", "", "", []string{"warn DG304"}, "10s of 30s, 20s spare"},
	{"fits", "healthy", "terminationGracePeriodSeconds: 20", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 20s, 5s spare"},
	{"too long", "healthy", "terminationGracePeriodSeconds: 10", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG301"}, "15s of 10s, 5s over"},
	{"exactly full", "healthy", "terminationGracePeriodSeconds: 15", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"warn DG302"}, "15s of 15s, 0s spare"},
	{"preStop longer than grace", "healthy", "", "lifecycle:\n  preStop:\n    exec:\n      command: [\"sleep\", \"40\"]", []string{"error DG301", "error DG303"}, "50s of 30s, 20s over"},
	{"grace zero", "healthy", "terminationGracePeriodSeconds: 0", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG305"}, ""},
	{"preStop shell sleep", "healthy", "", "lifecycle:\n  preStop:\n    exec:\n      command: [\"/bin/sh\", \"-c\", \"sleep 3\"]", nil, "13s of 30s, 17s spare"},
	{"preStop httpGet", "healthy", "", "lifecycle:\n  preStop:\n    httpGet:\n      path: /drain\n      port: 8080", []string{"note DG403"}, "at least 10s of 30s"},
	{"preStop script lower bound", "healthy", "", "lifecycle:\n  preStop:\n    exec:\n      command: [\"/bin/sh\", \"-c\", \"curl -s localhost:8080/drain; sleep 25\"]", []string{"error DG301", "note DG403"}, "at least 35s of 30s"},
	{"stopSignal SIGINT for a SIGINT-only program", "sigintonly", "", "lifecycle:\n  stopSignal: SIGINT\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s, 15s spare"},
	{"stopSignal SIGUSR1", "healthy", "", "lifecycle:\n  stopSignal: SIGUSR1\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG102"}, "not checked"},
	{"shell without exec", "healthy", "", "command: [\"sh\", \"-c\", \"/app/migrate && /app/api\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"warn DG401"}, "15s of 30s"},
	{"shell single command", "healthy", "", "command: [\"/bin/sh\", \"-c\", \"/app/api --verbose\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"warn DG401"}, "15s of 30s"},
	{"shell with exec", "healthy", "", "command: [\"/bin/bash\", \"-ec\", \"/app/migrate && exec /app/api\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s"},
	{"plain command", "healthy", "", "command: [\"/app/api\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s"},
	{"flag default", "flagtimeout", "", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"warn DG302"}, "30s of 30s, 0s spare"},
	{"flag set lower", "flagtimeout", "", "args: [\"--shutdown-timeout=10s\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s"},
	{"flag set higher, separate value", "flagtimeout", "", "args: [\"-shutdown-timeout\", \"40s\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG301"}, "45s of 30s, 15s over"},
	{"flag last one wins", "flagtimeout", "", "args: [\"-shutdown-timeout=40s\", \"-shutdown-timeout=10s\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s"},
	{"flag through $(VAR)", "flagtimeout", "", "env:\n  - name: T\n    value: 10s\nargs: [\"--shutdown-timeout=$(T)\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "15s of 30s"},
	{"flag in shell script", "flagtimeout", "", "command: [\"sh\", \"-c\", \"exec /app/api -shutdown-timeout=40s\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG301"}, "45s of 30s"},
	{"flag not a duration", "flagtimeout", "", "args: [\"--shutdown-timeout=forever\"]\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"note DG206"}, "not checked"},
	{"env set", "envtimeout", "", "env:\n  - name: SHUTDOWN_TIMEOUT\n    value: 20s\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "25s of 30s"},
	{"env too long", "envtimeout", "", "env:\n  - name: SHUTDOWN_TIMEOUT\n    value: 1m\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"error DG301"}, "65s of 30s"},
	{"env unset uses the fallback", "envtimeout", "", "lifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "10s of 30s"},
	{"env invalid uses the fallback", "envtimeout", "", "env:\n  - name: SHUTDOWN_TIMEOUT\n    value: soon\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", nil, "10s of 30s"},
	{"env from secret", "envtimeout", "", "env:\n  - name: SHUTDOWN_TIMEOUT\n    valueFrom:\n      secretKeyRef:\n        name: s\n        key: k\nlifecycle:\n  preStop:\n    sleep:\n      seconds: 5", []string{"note DG206"}, "not checked"},
}

func TestManifests(t *testing.T) {
	for _, c := range manifestCases {
		t.Run(c.name, func(t *testing.T) {
			p := writeTemp(t, "deploy.yaml", []byte(deployment(c.pod, c.ctr)))
			r := draingap(t, "--pkg", prog(c.prog), p)
			if r.code == 2 {
				t.Fatalf("refused: %s", r.stderr)
			}
			if got := codes(r.stdout); !reflect.DeepEqual(got, c.want) {
				t.Errorf("codes = %v, want %v\n%s", got, c.want, r.stdout)
			}
			if c.needs != "" && !strings.Contains(r.stdout, "needs         "+c.needs) {
				t.Errorf("needs line should contain %q:\n%s", c.needs, r.stdout)
			}
		})
	}
}

func TestKinds(t *testing.T) {
	for _, name := range []string{"statefulset.yaml", "cronjob.yaml", "pod.yaml", "list.json", "with-service.yaml"} {
		t.Run(name, func(t *testing.T) {
			r := draingap(t, "--pkg", prog("healthy"), manifest(name))
			if r.code != 0 || !strings.Contains(r.stdout, "no problems found") {
				t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestContainerChoice(t *testing.T) {
	r := draingap(t, "--pkg", prog("healthy"), manifest("two-containers.yaml"))
	if r.code != 2 || !strings.Contains(r.stderr, "--container") || r.stdout != "" {
		t.Errorf("two containers without --container: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--container", "api", manifest("two-containers.yaml"))
	if r.code != 0 || !strings.Contains(r.stdout, "Deployment/api container api") || strings.Contains(r.stdout, "container proxy") {
		t.Errorf("--container api: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--container", "Deployment/api/api", manifest("two-containers.yaml"))
	if r.code != 0 {
		t.Errorf("--container Deployment/api/api: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--container", "web", manifest("two-containers.yaml"))
	if r.code != 2 || r.stdout != "" {
		t.Errorf("--container web, which doesn't exist: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestJSON(t *testing.T) {
	r := draingap(t, "--pkg", "./examples/billing", "--format", "json", "examples/billing/deploy.yaml")
	if r.code != 1 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	for _, want := range []string{`"code": "DG201"`, `"code": "DG202"`, `"code": "DG102"`, `"code": "DG401"`, `"budgetChecked": false`} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("JSON output lacks %s:\n%s", want, r.stdout)
		}
	}
	r = draingap(t, "--pkg", "./examples/orders", "--format", "json", "examples/orders/deploy.yaml")
	if r.code != 0 || !strings.Contains(r.stdout, `"findings": []`) || !strings.Contains(r.stdout, `"budgetChecked": true`) {
		t.Errorf("exit %d\n%s", r.code, r.stdout)
	}
}

func TestStdinAndStopSignal(t *testing.T) {
	cmd := exec.Command(tool, "--pkg", prog("healthy"), "-")
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(readFile(t, manifest("basic.yaml")))
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "(stdin)") || !strings.Contains(string(out), "no problems found") {
		t.Errorf("manifest on stdin: %v\n%s", err, out)
	}
	cmd = exec.Command(tool, "--pkg", prog("healthy"), "-")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader("")
	out, err = cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || len(out) != 0 {
		t.Errorf("empty stdin: %v\n%s", err, out)
	}

	r := draingap(t, "--pkg", prog("sigintonly"), "--stop-signal", "SIGINT", manifest("basic.yaml"))
	if r.code != 0 || !strings.Contains(r.stdout, "SIGINT (--stop-signal") {
		t.Errorf("--stop-signal SIGINT for a SIGINT-only program: exit %d\n%s", r.code, r.stdout)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--stop-signal", "SIGUSR2", manifest("basic.yaml"))
	if got := codes(r.stdout); !reflect.DeepEqual(got, []string{"error DG102"}) {
		t.Errorf("--stop-signal SIGUSR2: %v\n%s", got, r.stdout)
	}
	r = draingap(t, "--pkg", prog("healthy"), "--stop-signal", "SIGFOO", manifest("basic.yaml"))
	if r.code != 2 || r.stdout != "" {
		t.Errorf("--stop-signal SIGFOO: exit %d", r.code)
	}
}
