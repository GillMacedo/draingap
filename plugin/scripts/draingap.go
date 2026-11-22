// Command draingap checks whether a Go HTTP service drains its requests
// before Kubernetes kills it: that the program handles the stop signal,
// that its Shutdown waits, and that the preStop delay and Shutdown
// deadline fit inside terminationGracePeriodSeconds.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("draingap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pkg := fs.String("pkg", "", "directory of the program's main package (required)")
	containers := fs.String("container", "", "containers that run the program, comma-separated: name or Kind/workload/name")
	failOn := fs.String("fail-on", "error", "exit 1 on findings at or above this level: error, warn, note, or never (input it can't trust still exits 2)")
	format := fs.String("format", "text", "output format: text or json")
	tags := fs.String("tags", "", "build tags to read the program with")
	stopSignal := fs.String("stop-signal", "", "the image's STOPSIGNAL, if it sets one (for example SIGINT); lifecycle.stopSignal in the manifest still wins")
	showVersion := fs.Bool("version", false, "print the version")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: draingap --pkg DIR [--container NAME] [--fail-on error|warn|note|never] [--format text|json] MANIFEST...")
		fmt.Fprintln(stderr, "MANIFEST is rendered YAML or JSON (- for stdin). Exit 0: nothing at --fail-on; 1: findings; 2: input can't be trusted, nothing checked.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "draingap", version)
		return 0
	}
	threshold := map[string]level{"error": levelError, "warn": levelWarn, "note": levelNote, "never": levelError + 1}
	lvl, ok := threshold[*failOn]
	if !ok {
		fmt.Fprintf(stderr, "draingap: --fail-on must be error, warn, note or never, not %q\n", *failOn)
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "draingap: --format must be text or json, not %q\n", *format)
		return 2
	}
	imageSig := 0
	if *stopSignal != "" {
		n, ok := signalNumbers[strings.ToUpper(*stopSignal)]
		if !ok {
			fmt.Fprintf(stderr, "draingap: --stop-signal %q is not a signal draingap knows\n", *stopSignal)
			return 2
		}
		imageSig = n
	}
	if *pkg == "" || fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	r, err := runCheck(*pkg, *tags, *containers, imageSig, fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "draingap: nothing was checked, because the input can't be trusted: %v\n", err)
		return 2
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if r.Findings == nil {
			r.Findings = []finding{}
		}
		enc.Encode(r)
	} else {
		printText(stdout, r)
	}
	for _, f := range r.Findings {
		if f.lvl >= lvl {
			return 1
		}
	}
	return 0
}

func runCheck(pkg, tags, want string, imageSig int, files []string) (report, error) {
	cs, err := readManifests(files)
	if err != nil {
		return report{}, err
	}
	cs, err = pick(cs, want)
	if err != nil {
		return report{}, err
	}
	prog, err := loadProgram(pkg, tags)
	if err != nil {
		return report{}, err
	}
	return check(prog, analyze(prog), cs, imageSig), nil
}

// pick selects the containers that run the program.
func pick(cs []*container, want string) ([]*container, error) {
	var names []string
	for _, c := range cs {
		names = append(names, c.workload+"/"+c.name)
	}
	sort.Strings(names)
	if want == "" {
		if len(cs) == 1 {
			return cs, nil
		}
		return nil, refuse("the manifests have %d containers (%s): say which run this program with --container", len(cs), strings.Join(names, ", "))
	}
	var out []*container
	for _, w := range strings.Split(want, ",") {
		w = strings.TrimSpace(w)
		found := false
		for _, c := range cs {
			if c.name == w || c.workload+"/"+c.name == w {
				out = append(out, c)
				found = true
			}
		}
		if !found {
			return nil, refuse("no container %q in the manifests; they have %s", w, strings.Join(names, ", "))
		}
	}
	return out, nil
}

func printText(w io.Writer, r report) {
	for _, b := range r.Budgets {
		fmt.Fprintf(w, "%s (%s)\n", b.Container, b.File)
		fmt.Fprintf(w, "  stop signal   %s\n", b.StopSignal)
		fmt.Fprintf(w, "  grace period  %s (%s)\n", secs(b.Grace), b.GraceFrom)
		fmt.Fprintf(w, "  preStop       %s\n", optSecs(b.PreStop, b.PreStopIs))
		fmt.Fprintf(w, "  sleep in code %s\n", optSecs(b.Sleep, b.SleepIs))
		fmt.Fprintf(w, "  Shutdown      %s\n", optSecs(b.Deadline, b.DeadlineIs))
		switch {
		case b.NotChecked != "":
			fmt.Fprintf(w, "  needs         not checked: %s\n", b.NotChecked)
		case b.Needed != nil && b.Checked && *b.Needed <= b.Grace:
			fmt.Fprintf(w, "  needs         %s of %s, %s spare\n", secs(*b.Needed), secs(b.Grace), secs(b.Grace-*b.Needed))
		case b.Needed != nil && b.Checked:
			fmt.Fprintf(w, "  needs         %s of %s, %s over\n", secs(*b.Needed), secs(b.Grace), secs(*b.Needed-b.Grace))
		case b.Needed != nil:
			fmt.Fprintf(w, "  needs         at least %s of %s\n", secs(*b.Needed), secs(b.Grace))
		default:
			fmt.Fprintf(w, "  needs         not checked\n")
		}
		fmt.Fprintf(w, "  entrypoint    %s\n", b.Entrypoint)
		fmt.Fprintln(w)
	}
	counts := map[string]int{}
	for _, f := range r.Findings {
		counts[f.Level]++
		head := strings.ToUpper(f.Level) + " " + f.Code
		if f.Where != "" {
			head += " " + f.Where
		}
		if f.Target != "" {
			head += " [" + f.Target + "]"
		}
		fmt.Fprintln(w, head)
		fmt.Fprintf(w, "  %s\n", f.Message)
		if f.Fix != "" {
			fmt.Fprintf(w, "  fix: %s\n", f.Fix)
		}
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(w)
	}
	n := len(r.Budgets)
	plural := "s"
	if n == 1 {
		plural = ""
	}
	switch {
	case counts["error"]+counts["warn"] == 0 && counts["note"] == 0:
		fmt.Fprintf(w, "draingap: %s, %d container%s: no problems found\n", r.Program, n, plural)
	case counts["error"]+counts["warn"] == 0:
		fmt.Fprintf(w, "draingap: %s, %d container%s: no errors or warnings, but %s what wasn't checked\n", r.Program, n, plural, notes(counts["note"]))
	default:
		fmt.Fprintf(w, "draingap: %s, %d container%s: %d error%s, %d warning%s, %d note%s\n", r.Program, n, plural, counts["error"], pl(counts["error"]), counts["warn"], pl(counts["warn"]), counts["note"], pl(counts["note"]))
	}
}

func notes(n int) string {
	if n == 1 {
		return "1 note says"
	}
	return fmt.Sprintf("%d notes say", n)
}

func pl(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func optSecs(v *float64, desc string) string {
	if v == nil {
		if desc == "" {
			return "not known"
		}
		return desc
	}
	if desc == "" {
		return secs(*v)
	}
	return secs(*v) + " (" + desc + ")"
}
