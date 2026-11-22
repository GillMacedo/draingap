package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLive runs draingap on real projects when their checkouts are given:
//
//	DRAINGAP_PODINFO=/path/to/podinfo (github.com/stefanprodan/podinfo)
//	DRAINGAP_BOUTIQUE=/path/to/microservices-demo (GoogleCloudPlatform)
//
// It needs network access the first time, for their modules.
func TestLive(t *testing.T) {
	ran := false
	if dir := os.Getenv("DRAINGAP_PODINFO"); dir != "" {
		ran = true
		m := withMarker(t, filepath.Join(dir, "kustomize", "deployment.yaml"))
		r := draingap(t, "--pkg", filepath.Join(dir, "cmd", "podinfo"), m)
		if r.code != 0 || strings.Contains(r.stdout, "ERROR") || !strings.Contains(r.stdout, "sleep in code 3s") || !strings.Contains(r.stdout, "viper.Unmarshal") {
			t.Errorf("podinfo: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
	}
	if dir := os.Getenv("DRAINGAP_BOUTIQUE"); dir != "" {
		ran = true
		m := withMarker(t, filepath.Join(dir, "kubernetes-manifests", "frontend.yaml"))
		r := draingap(t, "--pkg", filepath.Join(dir, "src", "frontend"), m)
		got := strings.Join(codes(r.stdout), ",")
		if r.code != 1 || got != "error DG101,error DG205" {
			t.Errorf("frontend: exit %d, codes %s\n%s%s", r.code, got, r.stdout, r.stderr)
		}
	}
	if !ran {
		t.Skip("set DRAINGAP_PODINFO or DRAINGAP_BOUTIQUE to run")
	}
}

func withMarker(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return writeTemp(t, "rendered.yaml", append(b, []byte("\n---\n")...))
}
