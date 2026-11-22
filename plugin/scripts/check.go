package main

import (
	"fmt"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

type level int

const (
	levelNote level = iota
	levelWarn
	levelError
)

func (l level) String() string {
	return [...]string{"note", "warn", "error"}[l]
}

type finding struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	Where   string `json:"where,omitempty"`
	Target  string `json:"container,omitempty"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
	lvl     level
}

// budget is the shutdown timeline of one container.
type budget struct {
	Container  string   `json:"container"`
	File       string   `json:"file"`
	StopSignal string   `json:"stopSignal"`
	Grace      float64  `json:"gracePeriodSeconds"`
	GraceFrom  string   `json:"gracePeriodFrom"`
	PreStop    *float64 `json:"preStopSeconds"`
	PreStopIs  string   `json:"preStop"`
	Sleep      *float64 `json:"sleepInCodeSeconds"`
	SleepIs    string   `json:"sleepInCode,omitempty"`
	Deadline   *float64 `json:"shutdownDeadlineSeconds"`
	DeadlineIs string   `json:"shutdownDeadline"`
	Needed     *float64 `json:"neededSeconds"`
	Checked    bool     `json:"budgetChecked"`
	NotChecked string   `json:"notCheckedBecause,omitempty"`
	Entrypoint string   `json:"entrypoint"`
}

type report struct {
	Program  string    `json:"program"`
	Budgets  []budget  `json:"containers"`
	Findings []finding `json:"findings"`
}

func secs(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64) + "s"
}

func ptr(f float64) *float64 { return &f }

func check(prog *program, fx facts, cs []*container, imageSig int) report {
	r := report{Program: prog.mainPkg}
	add := func(l level, code, where, target, msg, fix string) {
		r.Findings = append(r.Findings, finding{Level: l.String(), lvl: l, Code: code, Where: where, Target: target, Message: msg, Fix: fix})
	}

	// Program-wide findings, the same for every container.
	var serverServes []serveCall
	for _, s := range fx.serves {
		if s.pkgFunc {
			add(levelError, "DG205", prog.where(s.pos), "",
				s.name+" starts a server with no *http.Server value, so nothing can call Shutdown on it and every request in flight is cut when the program stops",
				"create an http.Server, run its ListenAndServe in a goroutine, and call Shutdown after the stop signal")
			continue
		}
		serverServes = append(serverServes, s)
	}
	if len(serverServes) > 0 && len(fx.shutdowns) == 0 {
		msg := "the program starts an http.Server and never calls Shutdown, so requests in flight are cut when it stops"
		if len(fx.closes) > 0 {
			msg = "the program stops its http.Server with Close, which drops active connections at once, and never calls Shutdown"
		}
		add(levelError, "DG204", prog.where(serverServes[0].pos), "", msg,
			"after the stop signal, call srv.Shutdown(ctx) with a context.WithTimeout deadline and wait for it to return")
	}
	if len(fx.shutdowns) > 0 {
		for _, s := range serverServes {
			if s.exitsWhy != "" {
				where := prog.where(s.exitsPos)
				if where == "" {
					where = prog.where(s.pos)
				}
				add(levelError, "DG202", where, "",
					"the program exits while Shutdown is still draining: "+s.exitsWhy,
					"treat http.ErrServerClosed as a normal return, then wait for the goroutine that calls Shutdown to finish (for example on a channel) before main returns")
			}
		}
	}
	for _, pos := range fx.unshut {
		add(levelWarn, "DG207", prog.where(pos), "",
			"this server is started but never shut down, while another server is, so its requests in flight are cut when the program exits",
			"call Shutdown on this server too, with the same deadline")
	}
	for _, w := range dedupe(sortedPositions(prog, fx.unreached)) {
		add(levelNote, "DG901", w, "", "this call is in a function that nothing reachable from main calls, so it was left out", "")
	}
	if len(fx.serves) == 0 && len(fx.shutdowns) == 0 {
		add(levelNote, "DG106", "", "",
			"no net/http server is started or shut down in the module's own code; servers started inside frameworks or other libraries (gin, echo, grpc and others) aren't followed, so only the stop signal and the manifest were checked", "")
	}
	for _, s := range fx.shutdowns {
		switch s.ctx.kind {
		case ctxCanceled:
			add(levelError, "DG201", prog.where(s.pos), "",
				"Shutdown gets a context derived from "+s.ctx.why+", which is already canceled once the signal arrives, so Shutdown closes the listeners and returns at once without waiting for requests in flight",
				"build the shutdown context from context.Background() (or context.WithoutCancel(ctx)) with context.WithTimeout")
		case ctxNone:
			add(levelWarn, "DG203", prog.where(s.pos), "",
				"Shutdown gets "+s.ctx.why+", which has no deadline, so a slow or stuck connection keeps the program running until Kubernetes sends SIGKILL, and nothing after Shutdown runs",
				"pass a context.WithTimeout deadline that fits the grace period")
		}
	}

	for _, c := range cs {
		r.Budgets = append(r.Budgets, checkContainer(prog, fx, c, imageSig, add))
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.lvl != b.lvl {
			return a.lvl > b.lvl
		}
		return a.Code < b.Code
	})
	return r
}

func checkContainer(prog *program, fx facts, c *container, imageSig int, add func(level, string, string, string, string, string)) budget {
	t := c.label()
	sig := c.stopSig
	sigFrom := "lifecycle.stopSignal"
	switch {
	case sig != 0:
	case imageSig != 0:
		sig, sigFrom = imageSig, "--stop-signal, the image's STOPSIGNAL"
	default:
		sig, sigFrom = 15, "the default, unless the image sets STOPSIGNAL"
	}
	b := budget{Container: t, File: c.file, StopSignal: signalName(sig) + " (" + sigFrom + ")", Grace: c.grace, PreStopIs: c.preDesc}
	b.GraceFrom = "terminationGracePeriodSeconds"
	if !c.graceSet {
		b.GraceFrom = "terminationGracePeriodSeconds not set, so the default"
	}
	switch {
	case c.hasCmd:
		b.Entrypoint = "command in the manifest"
	default:
		b.Entrypoint = "the image's ENTRYPOINT, not checked"
	}

	// Signals.
	handled, unknown, ignored, unread := false, "", false, ""
	var others []string
	for _, s := range fx.signals {
		covers := s.all
		for _, n := range s.sigs {
			if n == sig {
				covers = true
			}
		}
		if s.fn == "Ignore" {
			if covers {
				ignored = true
			}
			continue
		}
		if s.unknown != "" {
			unknown = s.unknown + " at " + prog.where(s.pos)
			continue
		}
		if covers && s.fn == "Notify" && containsPos(fx.unread, s.pos) {
			unread = prog.where(s.pos)
			continue
		}
		if covers {
			handled = true
		} else {
			for _, n := range s.sigs {
				others = append(others, signalName(n))
			}
		}
	}
	signalOK := false
	switch {
	case ignored:
		add(levelError, "DG103", firstSignal(prog, fx, "Ignore"), t,
			"the program ignores "+signalName(sig)+", the signal Kubernetes sends to stop it, so it runs until SIGKILL at the end of the grace period",
			"don't call signal.Ignore for "+signalName(sig))
	case handled:
		signalOK = true
	case unread != "":
		add(levelError, "DG103", unread, t,
			"signal.Notify catches "+signalName(sig)+" on a channel nothing receives from, so the signal is swallowed and the program runs until SIGKILL at the end of the grace period",
			"receive from the channel, then call srv.Shutdown")
	case unknown != "":
		add(levelNote, "DG104", "", t, "not checked whether the program handles "+signalName(sig)+": "+unknown, "")
	case len(prog.depSignals) > 0:
		add(levelNote, "DG105", "", t,
			"no os/signal call in the module handles "+signalName(sig)+", but "+strings.Join(prog.depSignals, ", ")+" calls os/signal, so a framework may handle it; not checked", "")
	case len(others) > 0:
		add(levelError, "DG102", firstSignal(prog, fx, "Notify", "NotifyContext"), t,
			"the program listens for "+strings.Join(dedupe(others), ", ")+" but not "+signalName(sig)+", so the signal Kubernetes sends kills it at once with requests in flight",
			"add syscall."+signalName(sig)+" to the signal.Notify or signal.NotifyContext call")
	default:
		add(levelError, "DG101", "", t,
			"nothing in the program calls signal.Notify or signal.NotifyContext, so "+signalName(sig)+" from Kubernetes kills it at once with requests in flight",
			"use signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt) and call srv.Shutdown when it is done")
	}

	if c.shellWarn != "" {
		add(levelWarn, "DG401", "", t, c.shellWarn,
			"start the program with exec (sh -c '... && exec /app'), or set command to the program itself")
	}

	if c.grace == 0 {
		add(levelError, "DG305", "", t, "terminationGracePeriodSeconds is 0, so Kubernetes kills the container with no time to shut down",
			"set terminationGracePeriodSeconds to the time the program needs, plus any preStop delay")
	}

	pre := c.preStop
	if pre.known {
		b.PreStop = ptr(pre.secs)
	}
	if pre.secs >= c.grace && c.grace > 0 {
		add(levelError, "DG303", "", t,
			fmt.Sprintf("the preStop hook takes %s%s and the grace period is %s; the stop signal is sent only after preStop ends, so the program gets at most the 2s extension Kubernetes adds", atLeast(pre), secs(pre.secs), secs(c.grace)),
			"make terminationGracePeriodSeconds longer than the preStop delay plus the program's shutdown deadline")
	}
	if !pre.known {
		b.PreStopIs = "not known: " + pre.why
		add(levelNote, "DG403", "", t, "the preStop duration isn't known: "+pre.why, "")
	}

	if !signalOK {
		b.NotChecked = "the program doesn't handle " + signalName(sig) + ", or draingap couldn't tell"
	}
	if len(fx.shutdowns) == 0 {
		b.DeadlineIs = "no Shutdown call"
		b.SleepIs = "not counted, no Shutdown call"
		if b.NotChecked == "" {
			b.NotChecked = "the program never calls (*http.Server).Shutdown"
		}
		return b
	}
	// The longest deadline among Shutdown calls decides the timeline.
	var dl durVal
	var dlFrom string
	var sl durVal
	sl.known = true
	any := false
	for _, s := range fx.shutdowns {
		var d durVal
		from := ""
		switch s.ctx.kind {
		case ctxDeadline:
			d, from = c.resolve(s.ctx.dur)
			if from == "" && d.known {
				from = "context.WithTimeout at " + prog.where(s.pos)
			}
		case ctxCanceled:
			d = durVal{why: "returns at once, its context is already canceled"}
		case ctxNone:
			d, from = durVal{why: "no deadline"}, "none"
		default:
			d = durVal{why: s.ctx.why}
		}
		if !any || (d.known && dl.known && d.secs > dl.secs) || (!d.known && dl.known) {
			dl, dlFrom = d, from
		}
		if s.sleep.known && sl.known && s.sleep.secs > sl.secs {
			sl = s.sleep
		} else if !s.sleep.known {
			sl = s.sleep
		}
		any = true
	}
	if sl.known {
		b.Sleep = ptr(sl.secs)
	} else {
		add(levelNote, "DG403", "", t, "the sleep before Shutdown isn't known: "+sl.why, "")
	}
	if dl.known {
		b.Deadline = ptr(dl.secs)
		b.DeadlineIs = dlFrom
	} else {
		b.DeadlineIs = dl.why
		if dl.why != "no deadline" && !strings.HasPrefix(dl.why, "returns at once") {
			b.DeadlineIs = "not known: " + dl.why
		}
		if b.NotChecked == "" {
			b.NotChecked = "the Shutdown deadline isn't known"
		}
		if dl.why == "no deadline" {
			b.NotChecked = "Shutdown has no deadline"
		} else if s0 := dl.why; !strings.HasPrefix(s0, "returns at once") {
			add(levelNote, "DG206", "", t, "the Shutdown deadline isn't known, so the time budget wasn't checked: "+dl.why, "")
		}
	}

	if signalOK && pre.known && pre.secs == 0 && sl.known && sl.secs == 0 && c.grace > 0 {
		add(levelWarn, "DG304", "", t,
			"Shutdown runs as soon as the signal arrives and there is no preStop delay, so the listener closes while Services may still send new connections to the pod, and those get connection refused",
			"add a preStop sleep of a few seconds (lifecycle.preStop.sleep.seconds, or exec sleep if the image has it) and raise the grace period to match")
	}

	if !dl.known {
		return b
	}
	need := pre.secs + sl.secs + dl.secs
	b.Needed = ptr(need)
	b.Checked = pre.known && sl.known && b.NotChecked == ""
	parts := fmt.Sprintf("preStop %s%s + sleep in code %s + Shutdown deadline %s (%s) = %s%s", atLeast(pre), secs(pre.secs), secs(sl.secs), secs(dl.secs), dlFrom, atLeast(pre), secs(need))
	switch {
	case need > c.grace && c.grace > 0:
		add(levelError, "DG301", "", t,
			"the shutdown can't finish inside the grace period: "+parts+", but Kubernetes sends SIGKILL "+secs(c.grace)+" after it starts stopping the pod ("+b.GraceFrom+")",
			"raise terminationGracePeriodSeconds above "+secs(need)+", or shorten the preStop delay or the Shutdown deadline")
	case need == c.grace && c.grace > 0 && b.Checked:
		add(levelWarn, "DG302", "", t,
			"the shutdown uses the whole grace period: "+parts+", so anything after Shutdown returns (closing databases, flushing logs or traces) races SIGKILL",
			"leave a few seconds between the shutdown deadline and terminationGracePeriodSeconds")
	}
	return b
}

func containsPos(ps []token.Pos, p token.Pos) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func atLeast(d durVal) string {
	if d.known {
		return ""
	}
	return "at least "
}

func firstSignal(prog *program, fx facts, fns ...string) string {
	for _, s := range fx.signals {
		for _, f := range fns {
			if s.fn == f {
				return prog.where(s.pos)
			}
		}
	}
	return ""
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
