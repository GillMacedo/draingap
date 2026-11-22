package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSleepSecs(t *testing.T) {
	cases := map[string]float64{"sleep 5": 5, "/bin/sleep 2.5": 2.5, "sleep 1m": 60, " sleep 10 ": 10}
	for in, want := range cases {
		if got, ok := sleepSecs(in); !ok || got != want {
			t.Errorf("sleepSecs(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"sleep", "sleep x", "sleep 5; true", "nginx -s quit", "sleep 5 && x"} {
		if _, ok := sleepSecs(in); ok {
			t.Errorf("sleepSecs(%q) should not parse", in)
		}
	}
}

func TestShellScript(t *testing.T) {
	cases := []struct {
		argv []string
		want string
		ok   bool
	}{
		{[]string{"sh", "-c", "exec /app"}, "exec /app", true},
		{[]string{"/bin/bash", "-ec", "a && b"}, "a && b", true},
		{[]string{"/bin/sh", "-e", "-c", "x"}, "x", true},
		{[]string{"busybox", "sh", "-c", "x"}, "x", true},
		{[]string{"/app", "-c", "config.yaml"}, "", false},
		{[]string{"sh", "script.sh"}, "", false},
		{[]string{"sh", "--login", "-c"}, "", false},
	}
	for _, c := range cases {
		got, ok := shellScript(c.argv)
		if got != c.want || ok != c.ok {
			t.Errorf("shellScript(%q) = %q, %v; want %q, %v", c.argv, got, ok, c.want, c.ok)
		}
	}
}

func TestExpandK8s(t *testing.T) {
	env := map[string]string{"A": "1s", "B": "x"}
	cases := map[string]string{
		"$(A)":       "1s",
		"--t=$(A)":   "--t=1s",
		"$$(A)":      "$(A)",
		"$(MISSING)": "$(MISSING)",
		"$(A)$(B)":   "1sx",
		"$":          "$",
	}
	for in, want := range cases {
		if got := expandK8s(in, env); got != want {
			t.Errorf("expandK8s(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlagValue(t *testing.T) {
	c := &container{args: []string{"-addr=:80", "--shutdown-timeout", "40s", "--", "-shutdown-timeout=1s"}, env: map[string]string{}}
	if v, ok := c.flagValue("shutdown-timeout"); !ok || v != "40s" {
		t.Errorf("flagValue = %q, %v; want 40s (arguments after -- are not flags)", v, ok)
	}
	if _, ok := c.flagValue("missing"); ok {
		t.Error("flagValue found a flag that isn't there")
	}
	// A flag that only shares a prefix doesn't count.
	c = &container{args: []string{"--shutdown-timeout-extra=9s"}, env: map[string]string{}}
	if _, ok := c.flagValue("shutdown-timeout"); ok {
		t.Error("flagValue matched a longer flag name")
	}
}

func TestSplitDocs(t *testing.T) {
	docs, lines, _ := splitDocs("x", []byte("a: 1\n---\nb: 2\n--- \n...\n"))
	if len(docs) != 4 || lines[1] != 3 || strings.TrimSpace(docs[1]) != "b: 2" {
		t.Errorf("splitDocs = %q, %v", docs, lines)
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--version"}, &out, &errb); code != 0 || out.String() != "draingap 0.1.0\n" {
		t.Errorf("--version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run(nil, &out, &errb); code != 2 || out.Len() != 0 {
		t.Errorf("no arguments: exit %d, stdout %q", code, out.String())
	}
}
