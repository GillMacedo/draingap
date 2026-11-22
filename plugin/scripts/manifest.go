package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// container is one container draingap checks, with what Kubernetes
// will do when it stops the pod.
type container struct {
	file      string
	workload  string // Kind/name
	name      string
	grace     float64 // terminationGracePeriodSeconds
	graceSet  bool
	preStop   durVal // known, or secs is a lower bound
	preDesc   string
	stopSig   int // 0: the image's STOPSIGNAL or SIGTERM
	command   []string
	args      []string
	hasCmd    bool
	env       map[string]string // literal values
	envUnk    map[string]string // name -> where its value comes from
	envFrom   bool
	shellWarn string // set when the command runs through a shell
}

func (c *container) label() string {
	return c.workload + " container " + c.name
}

var templateRe = regexp.MustCompile(`\{\{-?\s*[-.$a-zA-Z(]`)

// readManifests reads every container of every workload in the files.
func readManifests(files []string) ([]*container, error) {
	var all []*container
	for _, f := range files {
		cs, err := readManifestFile(f)
		if err != nil {
			return nil, err
		}
		all = append(all, cs...)
	}
	if len(all) == 0 {
		return nil, refuse("no Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob or Pod with containers was found in %s", strings.Join(files, ", "))
	}
	return all, nil
}

func readManifestFile(name string) ([]*container, error) {
	var data []byte
	var err error
	if name == "-" {
		data, err = io.ReadAll(os.Stdin)
		name = "stdin"
	} else {
		data, err = os.ReadFile(name)
	}
	if err != nil {
		return nil, refuse("can't read %s: %v", name, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, refuse("%s is empty", name)
	}
	if !isText(data) {
		return nil, refuse("%s is not a text file", name)
	}
	trimmed := bytes.TrimSpace(data)
	if trimmed[0] == '{' || trimmed[0] == '[' {
		// JSON, as kubectl get -o json prints. A cut-off file fails here.
		var v any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, refuse("%s is not complete JSON: %v", name, err)
		}
		if dec.More() {
			return nil, refuse("%s has data after its JSON value", name)
		}
		return fromObjects(name, []any{normalize(v)}, []int{1})
	}
	docs, lines, err := splitDocs(name, data)
	if err != nil {
		return nil, err
	}
	var objs []any
	var objLines []int
	lastWorkload := false
	for i, d := range docs {
		var v any
		if err := Unmarshal([]byte(d), &v); err != nil {
			return nil, refuse("%s: the YAML document starting at line %d doesn't parse, so it may be cut off or not YAML: %v", name, lines[i], err)
		}
		if v == nil {
			continue
		}
		v = normalize(v)
		if hasPodSpec(v) {
			if loc := templateRe.FindStringIndex(d); loc != nil {
				line := lines[i] + strings.Count(d[:loc[0]], "\n")
				return nil, refuse("%s:%d has a template expression, so it isn't rendered: render it first, for example with helm template", name, line)
			}
		}
		objs = append(objs, v)
		objLines = append(objLines, lines[i])
		lastWorkload = hasPodSpec(v)
	}
	if len(objs) == 0 {
		return nil, refuse("%s has no YAML objects, only comments or empty documents", name)
	}
	if lastWorkload && !endsWithMarker(data) {
		return nil, refuse("%s ends inside a workload without a final --- line, so it may be cut off: if the file is complete, add a line with --- at the end (with helm: helm template ... > rendered.yaml && echo --- >> rendered.yaml)", name)
	}
	return fromObjects(name, objs, objLines)
}

func isText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// splitDocs splits a YAML stream on --- lines, keeping start lines.
func splitDocs(name string, data []byte) ([]string, []int, error) {
	var docs []string
	var starts []int
	var cur strings.Builder
	start := 1
	for i, line := range strings.Split(string(data), "\n") {
		t := strings.TrimRight(line, " \t\r")
		if t == "---" || strings.HasPrefix(t, "--- ") || t == "..." {
			docs = append(docs, cur.String())
			starts = append(starts, start)
			cur.Reset()
			start = i + 2
			if strings.HasPrefix(t, "--- ") {
				// "--- value" starts a document with content on the same line.
				cur.WriteString(strings.TrimPrefix(t, "--- "))
				cur.WriteString("\n")
				start = i + 1
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	docs = append(docs, cur.String())
	starts = append(starts, start)
	return docs, starts, nil
}

func endsWithMarker(data []byte) bool {
	lines := strings.Split(strings.TrimRight(string(data), " \t\r\n"), "\n")
	last := strings.TrimRight(lines[len(lines)-1], " \t\r")
	return last == "---" || last == "..."
}

// normalize turns YAML maps into map[string]any.
func normalize(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case map[string]any:
		for k, val := range x {
			x[k] = normalize(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	}
	return v
}

// podSpecOf returns the pod spec of a workload object, if it has one.
func podSpecOf(obj map[string]any) (map[string]any, bool) {
	kind, _ := obj["kind"].(string)
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return nil, false
	}
	switch kind {
	case "Pod":
		return spec, true
	case "CronJob":
		jt, _ := spec["jobTemplate"].(map[string]any)
		js, _ := jt["spec"].(map[string]any)
		tmpl, _ := js["template"].(map[string]any)
		ps, ok := tmpl["spec"].(map[string]any)
		return ps, ok
	}
	tmpl, _ := spec["template"].(map[string]any)
	ps, ok := tmpl["spec"].(map[string]any)
	if ok {
		if _, has := ps["containers"]; has {
			return ps, true
		}
	}
	return nil, false
}

func hasPodSpec(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if items, ok := obj["items"].([]any); ok {
		for _, it := range items {
			if hasPodSpec(it) {
				return true
			}
		}
	}
	_, ok = podSpecOf(obj)
	return ok
}

func fromObjects(file string, objs []any, lines []int) ([]*container, error) {
	var out []*container
	for i, v := range objs {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, refuse("%s: the document at line %d is %s, not a Kubernetes object, so the file may be a command's error output", file, lines[i], describe(v))
		}
		kind, _ := obj["kind"].(string)
		if kind == "" || obj["apiVersion"] == nil {
			return nil, refuse("%s: the document at line %d has no apiVersion or kind, so it isn't a Kubernetes object (it may be a command's error output)", file, lines[i])
		}
		if strings.HasSuffix(kind, "List") {
			items, ok := obj["items"].([]any)
			if !ok {
				return nil, refuse("%s: %s at line %d has no items list", file, kind, lines[i])
			}
			sub := make([]int, len(items))
			for j := range sub {
				sub[j] = lines[i]
			}
			cs, err := fromObjects(file, items, sub)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
			continue
		}
		ps, ok := podSpecOf(obj)
		if !ok {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			if gen, _ := meta["generateName"].(string); gen != "" {
				name = gen
			} else {
				return nil, refuse("%s: %s at line %d has no metadata.name", file, kind, lines[i])
			}
		}
		cs, err := containersOf(file, kind+"/"+name, ps)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}

func describe(v any) string {
	switch x := v.(type) {
	case string:
		s := x
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		return "the text " + strconv.Quote(s)
	case []any:
		return "a list"
	}
	return fmt.Sprintf("a %T value", v)
}

func containersOf(file, workload string, ps map[string]any) ([]*container, error) {
	where := file + ": " + workload
	grace := 30.0
	graceSet := false
	if g, ok := ps["terminationGracePeriodSeconds"]; ok && g != nil {
		n, ok := intValue(g)
		if !ok || n < 0 {
			return nil, refuse("%s: terminationGracePeriodSeconds is %v, which the API server rejects: it must be a whole number of seconds, 0 or more", where, g)
		}
		grace, graceSet = float64(n), true
	}
	list, ok := ps["containers"].([]any)
	if !ok || len(list) == 0 {
		return nil, refuse("%s: spec.containers is missing or not a list", where)
	}
	var out []*container
	for _, item := range list {
		cm, ok := item.(map[string]any)
		if !ok {
			return nil, refuse("%s: a container is not a mapping", where)
		}
		c := &container{file: file, workload: workload, grace: grace, graceSet: graceSet,
			env: map[string]string{}, envUnk: map[string]string{}}
		c.name, _ = cm["name"].(string)
		if c.name == "" {
			return nil, refuse("%s: a container has no name", where)
		}
		cw := where + " container " + c.name
		var err error
		if c.command, err = stringList(cm["command"]); err != nil {
			return nil, refuse("%s: command %v", cw, err)
		}
		c.hasCmd = len(c.command) > 0
		if c.args, err = stringList(cm["args"]); err != nil {
			return nil, refuse("%s: args %v", cw, err)
		}
		if err := c.readEnv(cm, cw); err != nil {
			return nil, err
		}
		lc, _ := cm["lifecycle"].(map[string]any)
		if s, ok := lc["stopSignal"]; ok && s != nil {
			name, _ := s.(string)
			n, ok := signalNumbers[strings.ToUpper(name)]
			if !ok {
				return nil, refuse("%s: lifecycle.stopSignal %v is not a signal draingap knows", cw, s)
			}
			c.stopSig = n
		}
		c.preStop, c.preDesc = readPreStop(lc["preStop"])
		c.shellWarn = shellWrap(c)
		out = append(out, c)
	}
	return out, nil
}

func (c *container) readEnv(cm map[string]any, cw string) error {
	if ef, ok := cm["envFrom"].([]any); ok && len(ef) > 0 {
		c.envFrom = true
	}
	raw, ok := cm["env"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return refuse("%s: env is not a list", cw)
	}
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			return refuse("%s: an env entry is not a mapping", cw)
		}
		name, _ := m["name"].(string)
		if name == "" {
			return refuse("%s: an env entry has no name", cw)
		}
		if vf, ok := m["valueFrom"].(map[string]any); ok {
			src := "valueFrom"
			for k := range vf {
				src = k
			}
			c.envUnk[name] = src
			delete(c.env, name)
			continue
		}
		val := ""
		if v, ok := m["value"]; ok && v != nil {
			s, ok := v.(string)
			if !ok {
				return refuse("%s: env %s has a value that isn't a string, which the API server rejects", cw, name)
			}
			val = s
		}
		c.env[name] = expandK8s(val, c.env)
		delete(c.envUnk, name)
	}
	return nil
}

// expandK8s expands $(VAR) the way the kubelet does, using variables
// defined earlier. $$ escapes a dollar sign.
func expandK8s(s string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) {
			if s[i+1] == '$' {
				b.WriteByte('$')
				i++
				continue
			}
			if s[i+1] == '(' {
				if end := strings.IndexByte(s[i+2:], ')'); end >= 0 {
					name := s[i+2 : i+2+end]
					if v, ok := env[name]; ok {
						b.WriteString(v)
						i += end + 2
						continue
					}
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func intValue(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		return int64(x), x < 1<<62
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func stringList(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("is not a list")
	}
	var out []string
	for _, it := range list {
		switch x := it.(type) {
		case string:
			out = append(out, x)
		default:
			return nil, fmt.Errorf("has an item that isn't a string, which the API server rejects")
		}
	}
	return out, nil
}

var sleepRe = regexp.MustCompile(`^(?:/bin/|/usr/bin/)?sleep\s+([0-9]+(?:\.[0-9]+)?)([smh]?)$`)

func sleepSecs(s string) (float64, bool) {
	m := sleepRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "m":
		n *= 60
	case "h":
		n *= 3600
	}
	return n, true
}

// readPreStop works out how long the preStop hook takes. When it can't,
// secs is a lower bound.
func readPreStop(v any) (durVal, string) {
	ps, ok := v.(map[string]any)
	if !ok || len(ps) == 0 {
		return durVal{known: true}, "none"
	}
	if sl, ok := ps["sleep"].(map[string]any); ok {
		if n, ok := intValue(sl["seconds"]); ok && n >= 0 {
			return durVal{known: true, secs: float64(n)}, fmt.Sprintf("sleep action %ds", n)
		}
		return durVal{why: "the preStop sleep action has no whole number of seconds"}, "sleep action, unreadable"
	}
	if ex, ok := ps["exec"].(map[string]any); ok {
		cmd, err := stringList(ex["command"])
		if err != nil || len(cmd) == 0 {
			return durVal{why: "the preStop exec command is unreadable"}, "exec, unreadable"
		}
		if n, ok := sleepSecs(strings.Join(cmd, " ")); ok {
			return durVal{known: true, secs: n}, "exec " + strings.Join(cmd, " ") + ", needs a sleep binary in the image"
		}
		if script, ok := shellScript(cmd); ok {
			if n, ok := sleepSecs(script); ok {
				return durVal{known: true, secs: n}, "exec " + path.Base(cmd[0]) + " -c " + strconv.Quote(script) + ", needs a shell in the image"
			}
			// A sleep among other commands gives a lower bound.
			min := 0.0
			for _, part := range splitCommands(script) {
				if n, ok := sleepSecs(part); ok {
					min += n
				}
			}
			return durVal{secs: min, why: "the preStop script " + strconv.Quote(script) + " has commands whose duration isn't known"}, "exec script"
		}
		return durVal{why: "the preStop command " + strconv.Quote(strings.Join(cmd, " ")) + " takes a time draingap can't know"}, "exec " + strings.Join(cmd, " ")
	}
	if _, ok := ps["httpGet"]; ok {
		return durVal{why: "the preStop httpGet hook takes as long as its handler"}, "httpGet"
	}
	if _, ok := ps["tcpSocket"]; ok {
		return durVal{known: true}, "tcpSocket (does nothing)"
	}
	keys := make([]string, 0, len(ps))
	for k := range ps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return durVal{why: "the preStop hook has an unknown handler: " + strings.Join(keys, ", ")}, "unknown"
}

var shells = map[string]bool{"sh": true, "bash": true, "ash": true, "dash": true, "zsh": true, "ksh": true, "busybox": true}

// shellScript returns the script of a sh -c command line.
func shellScript(argv []string) (string, bool) {
	if len(argv) < 3 || !shells[path.Base(argv[0])] {
		return "", false
	}
	i := 1
	if path.Base(argv[0]) == "busybox" {
		if argv[1] != "sh" && argv[1] != "ash" {
			return "", false
		}
		i = 2
	}
	for ; i < len(argv)-1; i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return "", false
		}
		if strings.Contains(a, "c") && !strings.HasPrefix(a, "--") {
			return argv[i+1], true
		}
	}
	return "", false
}

var splitRe = regexp.MustCompile(`&&|\|\||[;\n&|]`)

func splitCommands(script string) []string {
	var out []string
	for _, p := range splitRe.Split(script, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// shellWrap explains why the stop signal may not reach the program when
// the container runs it through a shell.
func shellWrap(c *container) string {
	argv := append(append([]string(nil), c.command...), c.args...)
	if !c.hasCmd {
		return ""
	}
	script, ok := shellScript(argv)
	if !ok {
		return ""
	}
	parts := splitCommands(script)
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if strings.HasPrefix(last, "exec ") {
		return ""
	}
	return fmt.Sprintf("the container runs %s -c %q, so the shell is PID 1 and gets the stop signal; whether the program gets it depends on the image's shell (bash 5.2 replaces itself with the last command, dash 0.5.12 does not)", path.Base(argv[0]), script)
}

// argTokens returns the words of the command line, with the script of a
// shell wrapper split on spaces.
func (c *container) argTokens() []string {
	argv := append(append([]string(nil), c.command...), c.args...)
	if script, ok := shellScript(argv); ok {
		return strings.Fields(script)
	}
	var out []string
	for _, a := range argv {
		out = append(out, expandK8s(a, c.env))
	}
	return out
}

// flagValue finds -name=value, --name=value, -name value or --name value
// in the container's command line. The last one wins, as with the flag
// package.
func (c *container) flagValue(name string) (string, bool) {
	toks := c.argTokens()
	val, found := "", false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "--" {
			break
		}
		for _, p := range []string{"--" + name, "-" + name} {
			if t == p && i+1 < len(toks) {
				val, found = toks[i+1], true
				i++
			} else if strings.HasPrefix(t, p+"=") {
				val, found = strings.TrimPrefix(t, p+"="), true
			}
		}
	}
	return val, found
}

// resolve fills in a flag or env duration from the container.
func (c *container) resolve(d durVal) (durVal, string) {
	if d.known {
		return d, ""
	}
	if d.flag != "" {
		if v, ok := c.flagValue(d.flag); ok {
			if strings.Contains(v, "$") {
				return durVal{why: "flag -" + d.flag + " is set to " + v + ", which draingap can't expand"}, ""
			}
			dur, err := time.ParseDuration(v)
			if err != nil {
				return durVal{why: "flag -" + d.flag + " is set to " + strconv.Quote(v) + ", which isn't a Go duration, so the program won't start"}, ""
			}
			return durVal{known: true, secs: dur.Seconds()}, "flag -" + d.flag + "=" + v + " in the container args"
		}
		if d.flagDok {
			src := "default of flag -" + d.flag
			switch {
			case !c.hasCmd && len(c.args) == 0:
				src += ", unless the image's ENTRYPOINT or CMD sets it"
			case !c.hasCmd:
				src += ", unless the image's ENTRYPOINT sets it"
			}
			return durVal{known: true, secs: d.flagDef}, src
		}
		return d, ""
	}
	if d.env != "" {
		if v, ok := c.env[d.env]; ok {
			dur, err := time.ParseDuration(v)
			if err != nil {
				if d.envDok {
					return durVal{known: true, secs: d.envDef}, "$" + d.env + " is " + strconv.Quote(v) + ", which isn't a Go duration, so the program's fallback"
				}
				return durVal{why: "$" + d.env + " is " + strconv.Quote(v) + ", which isn't a Go duration"}, ""
			}
			return durVal{known: true, secs: dur.Seconds()}, "$" + d.env + "=" + v
		}
		if src, ok := c.envUnk[d.env]; ok {
			return durVal{why: "$" + d.env + " comes from " + src}, ""
		}
		if c.envFrom {
			return durVal{why: "$" + d.env + " may come from envFrom"}, ""
		}
		if d.envDok {
			return durVal{known: true, secs: d.envDef}, "$" + d.env + " isn't set, so the program's fallback"
		}
		return durVal{why: "$" + d.env + " isn't set in the container, and what the program does then isn't known"}, ""
	}
	return d, ""
}
