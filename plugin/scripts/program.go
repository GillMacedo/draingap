package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Linux signal numbers, which are what a container receives.
var signalNumbers = map[string]int{
	"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGKILL": 9, "SIGUSR1": 10, "SIGUSR2": 12,
	"SIGTERM": 15, "SIGWINCH": 28,
}

func signalName(n int) string {
	for name, v := range signalNumbers {
		if v == n {
			return name
		}
	}
	return "signal " + strconv.Itoa(n)
}

// signalReg is one call to signal.Notify, NotifyContext or Ignore.
type signalReg struct {
	pos     token.Pos
	fn      string // Notify, NotifyContext or Ignore
	all     bool   // no signals listed: every signal
	sigs    []int
	unknown string // why the signal list couldn't be read
}

// durVal is a duration draingap tried to work out from the source.
type durVal struct {
	known   bool
	secs    float64
	flag    string // set by this command-line flag
	flagDef float64
	flagDok bool   // flagDef is known
	env     string // parsed from this environment variable
	envDef  float64
	envDok  bool   // envDef is the value used when env is unset or invalid
	why     string // why it isn't known
}

// ctxKind says what deadline the context given to Shutdown has.
type ctxKind int

const (
	ctxUnknown  ctxKind = iota
	ctxNone             // no deadline: context.Background and the like
	ctxDeadline         // context.WithTimeout
	ctxCanceled         // derived from signal.NotifyContext, so done already
)

type ctxInfo struct {
	kind ctxKind
	dur  durVal // for ctxDeadline
	why  string // for ctxUnknown and ctxCanceled
}

type shutdownCall struct {
	server okey
	pos    token.Pos
	ctx    ctxInfo
	sleep  durVal // time.Sleep before Shutdown in the same function
}

type serveCall struct {
	pos       token.Pos
	server    okey // the *http.Server variable or field, if known
	name      string
	pkgFunc   bool   // http.ListenAndServe and friends, with no Server value
	exitsWhy  string // set when the program exits as soon as Serve returns
	exitsPos  token.Pos
	backgound bool
}

// facts is what the analysis found in the program.
type facts struct {
	signals   []signalReg
	shutdowns []shutdownCall
	serves    []serveCall
	closes    []token.Pos // (*http.Server).Close
	// unreached lists Shutdown, Serve and os/signal calls in functions
	// nothing reachable from main calls.
	unreached []token.Pos
	// unread lists signal.Notify calls whose channel nothing receives from.
	unread []token.Pos
	// unshut lists servers started with Serve that are never shut down
	// while another one is.
	unshut []token.Pos
}

// assign is one value given to a variable or field.
type assign struct {
	rhs   ast.Expr
	index int // which result of a multi-value call
	multi bool
	lp    *loadedPkg
	pos   token.Pos
	end   token.Pos     // end of the statement
	block ast.Node      // innermost enclosing block
	fn    *ast.FuncDecl // enclosing function; nil at package level
	via   string        // for &x passed to a call: the call
}

// pickAssign returns the assignment that gives a variable its value at
// use. With one assignment, that is the one. With several, it is the
// last that ends before use, when they all sit in the same block, so
// they run in order: ctx := context.Background() followed by
// ctx, cancel := context.WithTimeout(ctx, d).
func (a *analyzer) pickAssign(obj okey, use token.Pos) (assign, bool) {
	var as []assign
	for _, x := range a.assigns[obj] {
		// Code nothing reachable calls, such as test helpers and mocks,
		// doesn't set the value the program runs with.
		if x.fn == nil || a.reach[x.fn] {
			as = append(as, x)
		}
	}
	if len(as) == 1 {
		return as[0], as[0].rhs != nil
	}
	if len(as) == 0 {
		return assign{}, false
	}
	var best *assign
	for i := range as {
		x := &as[i]
		if x.rhs == nil || x.block == nil || x.block != as[0].block {
			return assign{}, false
		}
		if x.end <= use && (best == nil || x.pos > best.pos) {
			best = x
		}
	}
	if best == nil {
		return assign{}, false
	}
	return *best, true
}

type flagReg struct {
	name string
	def  ast.Expr
	lp   *loadedPkg
}

type analyzer struct {
	prog    *program
	assigns map[okey][]assign
	flags   map[okey]flagReg // DurationVar targets
	funcs   map[okey]*ast.FuncDecl
	funcPkg map[*ast.FuncDecl]*loadedPkg
	calls   map[okey][]callSite // who calls each function
	params  map[okey]paramInfo
	reach   map[*ast.FuncDecl]bool
	// ifaceCalls lists calls through interface methods, by method name.
	ifaceCalls map[string][]callSite
	f          facts
}

type callSite struct {
	call *ast.CallExpr
	path []ast.Node
	lp   *loadedPkg
}

func analyze(prog *program) facts {
	a := &analyzer{
		prog:       prog,
		assigns:    map[okey][]assign{},
		flags:      map[okey]flagReg{},
		funcs:      map[okey]*ast.FuncDecl{},
		funcPkg:    map[*ast.FuncDecl]*loadedPkg{},
		calls:      map[okey][]callSite{},
		params:     map[okey]paramInfo{},
		ifaceCalls: map[string][]callSite{},
	}
	for _, lp := range prog.own {
		for _, f := range lp.files {
			a.index(lp, f)
		}
	}
	a.markReachable()
	for _, lp := range prog.own {
		for _, f := range lp.files {
			a.scan(lp, f)
		}
	}
	a.checkServers()
	return a.f
}

// markReachable finds the functions that main and init functions can
// reach through static calls and function values. A call through an
// interface method reaches every method of that name in the module.
func (a *analyzer) markReachable() {
	a.reach = map[*ast.FuncDecl]bool{}
	byName := map[string][]*ast.FuncDecl{}
	var work []*ast.FuncDecl
	for obj, fd := range a.funcs {
		if fd.Recv != nil {
			byName[fd.Name.Name] = append(byName[fd.Name.Name], fd)
		}
		lp := a.funcPkg[fd]
		if fd.Recv == nil && (fd.Name.Name == "init" || fd.Name.Name == "main" && lp.path == a.prog.mainPkg) {
			work = append(work, fd)
		}
		_ = obj
	}
	// Package-level variable initializers run too.
	for _, lp := range a.prog.own {
		for _, f := range lp.files {
			for _, d := range f.Decls {
				if gd, ok := d.(*ast.GenDecl); ok {
					work = append(work, a.refsIn(lp, gd, byName)...)
				}
			}
		}
	}
	for len(work) > 0 {
		fd := work[len(work)-1]
		work = work[:len(work)-1]
		if a.reach[fd] {
			continue
		}
		a.reach[fd] = true
		if fd.Body != nil {
			work = append(work, a.refsIn(a.funcPkg[fd], fd.Body, byName)...)
		}
	}
}

func (a *analyzer) refsIn(lp *loadedPkg, n ast.Node, byName map[string][]*ast.FuncDecl) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	ast.Inspect(n, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		fn, ok := lp.info.Uses[id].(*types.Func)
		if !ok {
			return true
		}
		if fd, ok := a.funcs[a.key(fn)]; ok {
			out = append(out, fd)
			return true
		}
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			if _, isIface := sig.Recv().Type().Underlying().(*types.Interface); isIface {
				out = append(out, byName[fn.Name()]...)
			}
		}
		return true
	})
	return out
}

// checkServers lists servers that are started but never shut down when
// some other server is.
func (a *analyzer) checkServers() {
	if len(a.f.shutdowns) == 0 {
		return
	}
	shut := map[okey]bool{}
	for _, s := range a.f.shutdowns {
		if s.server == "" {
			return // can't tell which servers are shut down
		}
		shut[s.server] = true
	}
	for _, s := range a.f.serves {
		if !s.pkgFunc && s.server != "" && !shut[s.server] {
			a.f.unshut = append(a.f.unshut, s.pos)
		}
	}
}

// receiverObj returns the variable or field a method is called on.
func (a *analyzer) receiverObj(lp *loadedPkg, call *ast.CallExpr) okey {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	obj := a.objOf(lp, sel.X)
	if _, isParam := a.params[obj]; isParam {
		return "" // the server is passed in; which one isn't followed
	}
	return obj
}

// channelRead reports whether anything receives from the channel passed
// to signal.Notify, or passes it on to code that might.
func (a *analyzer) channelRead(lp *loadedPkg, ch ast.Expr) bool {
	obj := a.objOf(lp, ch)
	if obj == "" {
		return true
	}
	read := false
	for _, olp := range a.prog.own {
		for _, f := range olp.files {
			walk(f, func(n ast.Node, path []ast.Node) {
				if read {
					return
				}
				switch n := n.(type) {
				case *ast.UnaryExpr:
					if n.Op == token.ARROW && a.objOf(olp, n.X) == obj {
						read = true
					}
				case *ast.RangeStmt:
					if a.objOf(olp, n.X) == obj {
						read = true
					}
				case *ast.CallExpr:
					fn := calleeFunc(olp.info, n)
					if isFunc(fn, "os/signal", "Notify", "Stop") {
						return
					}
					for _, arg := range n.Args {
						if a.objOf(olp, arg) == obj {
							read = true
						}
					}
				case *ast.AssignStmt:
					for _, r := range n.Rhs {
						if a.objOf(olp, r) == obj {
							read = true
						}
					}
				case *ast.ReturnStmt:
					for _, r := range n.Results {
						if a.objOf(olp, r) == obj {
							read = true
						}
					}
				case *ast.CompositeLit:
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							e = kv.Value
						}
						if a.objOf(olp, e) == obj {
							read = true
						}
					}
				}
			})
		}
	}
	return read
}

// index records assignments, flag registrations, functions and call sites.
func (a *analyzer) index(lp *loadedPkg, f *ast.File) {
	var curEnd token.Pos
	var curBlock ast.Node
	var curFn *ast.FuncDecl
	record := func(lhs ast.Expr, as assign) {
		if obj := a.objOf(lp, lhs); obj != "" {
			as.end, as.block, as.fn = curEnd, curBlock, curFn
			a.assigns[obj] = append(a.assigns[obj], as)
		}
	}
	walk(f, func(n ast.Node, path []ast.Node) {
		curEnd, curBlock, curFn = n.End(), nil, nil
		if len(path) > 1 {
			curFn, _ = path[1].(*ast.FuncDecl)
		}
		for i := len(path) - 1; i >= 0; i-- {
			if b, ok := path[i].(*ast.BlockStmt); ok {
				curBlock = b
				break
			}
		}
		if len(path) > 0 {
			if ds, ok := path[len(path)-1].(*ast.GenDecl); ok {
				curEnd = ds.End()
			}
		}
		switch n := n.(type) {
		case *ast.FuncDecl:
			if obj := lp.info.Defs[n.Name]; obj != nil {
				a.funcs[a.key(obj)] = n
				a.funcPkg[n] = lp
				a.indexParams(lp, a.key(obj), n.Name.Name, n.Type)
			}
		case *ast.FuncLit:
			a.indexParams(lp, "", "", n.Type)
		case *ast.ValueSpec:
			for i, name := range n.Names {
				switch {
				case len(n.Values) == len(n.Names):
					record(name, assign{rhs: n.Values[i], lp: lp, pos: name.Pos()})
				case len(n.Values) == 1:
					record(name, assign{rhs: n.Values[0], index: i, multi: true, lp: lp, pos: name.Pos()})
				default:
					record(name, assign{lp: lp, pos: name.Pos()}) // zero value
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE && n.Tok != token.ASSIGN {
				for _, l := range n.Lhs {
					record(l, assign{lp: lp, pos: l.Pos()}) // += and the like: unknown
				}
				return
			}
			for i, l := range n.Lhs {
				switch {
				case len(n.Rhs) == len(n.Lhs):
					record(l, assign{rhs: n.Rhs[i], lp: lp, pos: l.Pos()})
				case len(n.Rhs) == 1:
					record(l, assign{rhs: n.Rhs[0], index: i, multi: true, lp: lp, pos: l.Pos()})
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				if obj, ok := lp.info.Uses[id].(*types.Var); ok && obj.IsField() {
					k := a.key(obj)
					a.assigns[k] = append(a.assigns[k], assign{rhs: n.Value, lp: lp, pos: id.Pos(), fn: curFn})
				}
			}
		case *ast.UnaryExpr:
			// &x passed anywhere other than a DurationVar flag: x may be
			// changed through the pointer, so its value is unknown.
			if n.Op == token.AND && !a.isDurationVarTarget(lp, n, path) {
				if obj := a.objOf(lp, n.X); obj != "" {
					via := "a pointer to it"
					if len(path) > 0 {
						if call, ok := path[len(path)-1].(*ast.CallExpr); ok {
							via = exprString(call)
						}
					}
					a.assigns[obj] = append(a.assigns[obj], assign{lp: lp, pos: n.Pos(), fn: curFn, via: via})
				}
			}
		case *ast.CallExpr:
			fn := calleeFunc(lp.info, n)
			if fn == nil {
				return
			}
			site := callSite{call: n, path: append([]ast.Node(nil), path...), lp: lp}
			a.calls[a.key(fn)] = append(a.calls[a.key(fn)], site)
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
				if _, isIface := sig.Recv().Type().Underlying().(*types.Interface); isIface {
					a.ifaceCalls[fn.Name()] = append(a.ifaceCalls[fn.Name()], site)
				}
			}
			if name, def, target, ok := durationFlag(lp, fn, n); ok && target != nil {
				if obj := a.objOf(lp, target); obj != "" {
					a.flags[obj] = flagReg{name: name, def: def, lp: lp}
				}
			}
		}
	})
}

func (a *analyzer) isDurationVarTarget(lp *loadedPkg, u *ast.UnaryExpr, path []ast.Node) bool {
	if len(path) == 0 {
		return false
	}
	call, ok := path[len(path)-1].(*ast.CallExpr)
	if !ok {
		return false
	}
	fn := calleeFunc(lp.info, call)
	if fn == nil {
		return false
	}
	_, _, target, ok := durationFlag(lp, fn, call)
	return ok && target == u.X
}

// okey identifies an object across packages. Each package is
// type-checked on its own, with its imports read from export data, so the
// same function or variable is a different types.Object in each package
// that uses it. Its package, name and source position are the same.
type okey string

func (a *analyzer) key(obj types.Object) okey {
	if obj == nil {
		return ""
	}
	pkg := ""
	if obj.Pkg() != nil {
		pkg = obj.Pkg().Path()
	}
	pos := a.prog.fset.Position(obj.Pos())
	if !pos.IsValid() {
		return okey(fmt.Sprintf("%s.%s@%p", pkg, obj.Name(), obj))
	}
	// Export data and source disagree on columns, so only the line is used.
	return okey(fmt.Sprintf("%s.%s@%s:%d", pkg, obj.Name(), filepath.Base(pos.Filename), pos.Line))
}

// objOf returns the key of the variable or field an expression names.
func (a *analyzer) objOf(lp *loadedPkg, e ast.Expr) okey {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		if e.Name == "_" {
			return ""
		}
		if obj := lp.info.Defs[e]; obj != nil {
			return a.key(obj)
		}
		if v, ok := lp.info.Uses[e].(*types.Var); ok {
			return a.key(v)
		}
	case *ast.SelectorExpr:
		if v, ok := lp.info.Uses[e.Sel].(*types.Var); ok {
			return a.key(v)
		}
	}
	return ""
}

// durationFlag recognizes flag and pflag duration flags. It returns the
// flag name, its default and, for the Var forms, the target expression.
func durationFlag(lp *loadedPkg, fn *types.Func, call *ast.CallExpr) (name string, def ast.Expr, target ast.Expr, ok bool) {
	if fn.Pkg() == nil {
		return
	}
	switch fn.Pkg().Path() {
	case "flag", "github.com/spf13/pflag":
	default:
		return
	}
	args := call.Args
	switch fn.Name() {
	case "Duration": // (name, def, usage)
		if len(args) == 3 {
			name, ok = constString(lp.info, args[0])
			def = args[1]
		}
	case "DurationP": // (name, short, def, usage)
		if len(args) == 4 {
			name, ok = constString(lp.info, args[0])
			def = args[2]
		}
	case "DurationVar": // (p, name, def, usage)
		if len(args) == 4 {
			name, ok = constString(lp.info, args[1])
			def, target = args[2], addrOf(args[0])
		}
	case "DurationVarP": // (p, name, short, def, usage)
		if len(args) == 5 {
			name, ok = constString(lp.info, args[1])
			def, target = args[3], addrOf(args[0])
		}
	}
	return
}

func addrOf(e ast.Expr) ast.Expr {
	if u, ok := ast.Unparen(e).(*ast.UnaryExpr); ok && u.Op == token.AND {
		return u.X
	}
	return nil
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	tv := info.Types[e]
	if tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value), true
	}
	return "", false
}

// calleeFunc returns the function or method a call calls, if static.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	case *ast.IndexExpr:
		return calleeFunc(info, &ast.CallExpr{Fun: f.X})
	}
	return nil
}

func isFunc(fn *types.Func, pkg string, names ...string) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != pkg {
		return false
	}
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		return false
	}
	for _, n := range names {
		if fn.Name() == n {
			return true
		}
	}
	return false
}

// isMethod reports whether fn is a method of pkg.typ (or *pkg.typ).
func isMethod(fn *types.Func, pkg, typ string, names ...string) bool {
	if fn == nil {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != pkg || named.Obj().Name() != typ {
		return false
	}
	for _, n := range names {
		if fn.Name() == n {
			return true
		}
	}
	return false
}

// walk visits every node with the path of its ancestors.
func walk(root ast.Node, visit func(n ast.Node, path []ast.Node)) {
	var path []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			path = path[:len(path)-1]
			return true
		}
		visit(n, path)
		path = append(path, n)
		return true
	})
}

func (a *analyzer) scan(lp *loadedPkg, f *ast.File) {
	walk(f, func(n ast.Node, path []ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		fn := calleeFunc(lp.info, call)
		interesting := isFunc(fn, "os/signal", "Notify", "NotifyContext", "Ignore") ||
			isMethod(fn, "net/http", "Server", "Shutdown", "Close", "ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS") ||
			isFunc(fn, "net/http", "ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS")
		if !interesting {
			return
		}
		for _, p := range path {
			if fd, ok := p.(*ast.FuncDecl); ok && !a.reach[fd] {
				a.f.unreached = append(a.f.unreached, call.Pos())
				return
			}
		}
		switch {
		case isFunc(fn, "os/signal", "Notify", "NotifyContext", "Ignore"):
			a.f.signals = append(a.f.signals, a.signalCall(lp, fn.Name(), call))
			if fn.Name() == "Notify" && len(call.Args) > 0 && !a.channelRead(lp, call.Args[0]) {
				a.f.unread = append(a.f.unread, call.Pos())
			}
		case isMethod(fn, "net/http", "Server", "Shutdown") && len(call.Args) == 1:
			a.f.shutdowns = append(a.f.shutdowns, shutdownCall{
				server: a.receiverObj(lp, call),
				pos:    call.Pos(),
				ctx:    a.classifyCtx(lp, call.Args[0], 0),
				sleep:  a.sleepBefore(lp, call, path),
			})
		case isMethod(fn, "net/http", "Server", "Close"):
			a.f.closes = append(a.f.closes, call.Pos())
		case isMethod(fn, "net/http", "Server", "ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS"):
			a.f.serves = append(a.f.serves, a.serveCall(lp, fn.Name(), call, path, false))
		case isFunc(fn, "net/http", "ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS"):
			a.f.serves = append(a.f.serves, a.serveCall(lp, "http."+fn.Name(), call, path, true))
		}
	})
}

func (a *analyzer) signalCall(lp *loadedPkg, name string, call *ast.CallExpr) signalReg {
	reg := signalReg{pos: call.Pos(), fn: name}
	args := call.Args
	if name != "Ignore" {
		if len(args) == 0 {
			reg.unknown = "no arguments"
			return reg
		}
		args = args[1:]
	}
	if len(args) == 0 {
		reg.all = true
		return reg
	}
	if call.Ellipsis.IsValid() {
		list := a.sliceElems(lp, args[len(args)-1], 0)
		if list == nil {
			reg.unknown = "the signal list is passed as a slice draingap can't follow"
			return reg
		}
		args = append(append([]ast.Expr(nil), args[:len(args)-1]...), list...)
	}
	for _, e := range args {
		n, ok := signalValue(lp.info, e)
		if !ok {
			reg.unknown = "a signal value draingap can't work out: " + exprString(e)
			return reg
		}
		reg.sigs = append(reg.sigs, n)
	}
	return reg
}

// sliceElems follows a []os.Signal value to its composite literal.
func (a *analyzer) sliceElems(lp *loadedPkg, e ast.Expr, depth int) []ast.Expr {
	if depth > 4 {
		return nil
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		return e.Elts
	case *ast.Ident, *ast.SelectorExpr:
		obj := a.objOf(lp, e)
		as := a.assigns[obj]
		if obj == "" || len(as) != 1 || as[0].rhs == nil || as[0].multi {
			return nil
		}
		return a.sliceElems(as[0].lp, as[0].rhs, depth+1)
	}
	return nil
}

func signalValue(info *types.Info, e ast.Expr) (int, bool) {
	tv := info.Types[e]
	if tv.Value != nil {
		if v, ok := constant.Int64Val(constant.ToInt(tv.Value)); ok {
			return int(v), true
		}
	}
	var obj types.Object
	switch x := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		obj = info.Uses[x.Sel]
	case *ast.Ident:
		obj = info.Uses[x]
	}
	if obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "os" {
		switch obj.Name() {
		case "Interrupt":
			return 2, true
		case "Kill":
			return 9, true
		}
	}
	return 0, false
}

// classifyCtx works out the deadline of the context passed to Shutdown.
func (a *analyzer) classifyCtx(lp *loadedPkg, e ast.Expr, depth int) ctxInfo {
	if depth > 6 {
		return ctxInfo{why: "the context is built in too many steps to follow"}
	}
	e = ast.Unparen(e)
	switch x := e.(type) {
	case *ast.CallExpr:
		fn := calleeFunc(lp.info, x)
		switch {
		case isFunc(fn, "context", "Background", "TODO"):
			return ctxInfo{kind: ctxNone, why: "context." + fn.Name() + "()"}
		case isFunc(fn, "context", "WithoutCancel"):
			return ctxInfo{kind: ctxNone, why: "context.WithoutCancel"}
		case isFunc(fn, "os/signal", "NotifyContext"):
			return ctxInfo{kind: ctxCanceled, why: "signal.NotifyContext"}
		case isFunc(fn, "context", "WithCancel", "WithCancelCause", "WithValue") && len(x.Args) >= 1:
			return a.classifyCtx(lp, x.Args[0], depth+1)
		case isFunc(fn, "context", "WithTimeout", "WithTimeoutCause") && len(x.Args) >= 2:
			parent := a.classifyCtx(lp, x.Args[0], depth+1)
			if parent.kind == ctxCanceled {
				return parent
			}
			d := a.evalDur(lp, x.Args[1], 0)
			if parent.kind == ctxDeadline && parent.dur.known && d.known && parent.dur.secs < d.secs {
				return parent
			}
			if parent.kind == ctxUnknown {
				return ctxInfo{kind: ctxDeadline, dur: d, why: "parent " + parent.why}
			}
			return ctxInfo{kind: ctxDeadline, dur: d}
		case isFunc(fn, "context", "WithDeadline", "WithDeadlineCause"):
			parent := a.classifyCtx(lp, x.Args[0], depth+1)
			if parent.kind == ctxCanceled {
				return parent
			}
			return ctxInfo{kind: ctxDeadline, dur: durVal{why: "context.WithDeadline with a time draingap can't work out"}}
		}
		return ctxInfo{why: "the context comes from " + exprString(x.Fun)}
	case *ast.Ident, *ast.SelectorExpr:
		obj := a.objOf(lp, e)
		if obj == "" {
			return ctxInfo{why: "the context comes from " + exprString(e)}
		}
		if pi, ok := a.params[obj]; ok {
			return a.classifyParam(pi, depth)
		}
		as, ok := a.pickAssign(obj, e.Pos())
		if !ok {
			return ctxInfo{why: exprString(e) + " is set in more than one place"}
		}
		if as.multi && as.index != 0 {
			return ctxInfo{why: "the context comes from " + exprString(as.rhs)}
		}
		return a.classifyCtx(as.lp, as.rhs, depth+1)
	}
	return ctxInfo{why: "the context comes from " + exprString(e)}
}

// classifyParam follows a context parameter to the callers of its
// function. All callers have to agree.
func (a *analyzer) classifyParam(pi paramInfo, depth int) ctxInfo {
	fnObj, idx := pi.fn, pi.idx
	if fnObj == "" {
		return ctxInfo{why: "the context is a parameter of a function literal"}
	}
	sites := a.calls[fnObj]
	if fd := a.funcs[fnObj]; fd != nil && fd.Recv != nil {
		sites = append(append([]callSite(nil), sites...), a.ifaceCalls[pi.name]...)
	}
	if len(sites) == 0 {
		return ctxInfo{why: "the context is a parameter of " + pi.name + ", which nothing in the module calls"}
	}
	var out *ctxInfo
	for _, s := range sites {
		if idx >= len(s.call.Args) {
			return ctxInfo{why: "can't match the arguments of a call to " + pi.name}
		}
		c := a.classifyCtx(s.lp, s.call.Args[idx], depth+1)
		if out == nil {
			out = &c
			continue
		}
		if c.kind != out.kind || c.dur.known != out.dur.known || c.dur.secs != out.dur.secs {
			return ctxInfo{why: "callers of " + pi.name + " pass different contexts"}
		}
	}
	return *out
}

type paramInfo struct {
	fn   okey // empty for function literals
	name string
	idx  int
}

func (a *analyzer) indexParams(lp *loadedPkg, fn okey, fnName string, ft *ast.FuncType) {
	if ft.Params == nil {
		return
	}
	i := 0
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			if obj := lp.info.Defs[name]; obj != nil {
				a.params[a.key(obj)] = paramInfo{fn: fn, name: fnName, idx: i}
			}
			i++
		}
	}
}

// evalDur works out a time.Duration value.
func (a *analyzer) evalDur(lp *loadedPkg, e ast.Expr, depth int) durVal {
	if depth > 6 {
		return durVal{why: "the duration is built in too many steps to follow"}
	}
	e = ast.Unparen(e)
	if tv := lp.info.Types[e]; tv.Value != nil {
		if v, ok := constant.Int64Val(constant.ToInt(tv.Value)); ok {
			return durVal{known: true, secs: float64(v) / 1e9}
		}
	}
	switch x := e.(type) {
	case *ast.StarExpr:
		obj := a.objOf(lp, x.X)
		as := a.assigns[obj]
		if obj != "" && len(as) == 1 && as[0].rhs != nil {
			if call, ok := ast.Unparen(as[0].rhs).(*ast.CallExpr); ok {
				if fn := calleeFunc(as[0].lp.info, call); fn != nil {
					if name, def, target, ok := durationFlag(as[0].lp, fn, call); ok && target == nil {
						return a.flagDur(as[0].lp, name, def)
					}
				}
			}
		}
	case *ast.Ident, *ast.SelectorExpr:
		obj := a.objOf(lp, e)
		if obj == "" {
			break
		}
		if why := a.decoded(lp, e); why != "" {
			return durVal{why: why}
		}
		if fr, ok := a.flags[obj]; ok {
			others := 0
			for _, as := range a.assigns[obj] {
				if as.rhs != nil || as.multi {
					others++
				}
			}
			if others == 0 {
				return a.flagDur(fr.lp, fr.name, fr.def)
			}
			return durVal{why: exprString(e) + " is a flag that is also set in code"}
		}
		if pi, ok := a.params[obj]; ok {
			return a.durParam(pi, depth)
		}
		if d, ok := a.envWithFallback(a.assigns[obj]); ok {
			return d
		}
		as, ok := a.pickAssign(obj, e.Pos())
		if !ok {
			return durVal{why: exprString(e) + " is set in more than one place, or not in code draingap can read"}
		}
		if as.multi {
			if call, ok := ast.Unparen(as.rhs).(*ast.CallExpr); ok && as.index == 0 {
				fn := calleeFunc(as.lp.info, call)
				if isFunc(fn, "time", "ParseDuration") && len(call.Args) == 1 {
					return a.envDur(as.lp, call.Args[0])
				}
			}
			return durVal{why: exprString(e) + " comes from " + exprString(as.rhs)}
		}
		return a.evalDur(as.lp, as.rhs, depth+1)
	}
	return durVal{why: "the duration comes from " + exprString(e)}
}

// envWithFallback recognizes
//
//	d, err := time.ParseDuration(os.Getenv("X"))
//	if err != nil { d = 5 * time.Second }
//
// as $X with a constant fallback.
func (a *analyzer) envWithFallback(as []assign) (durVal, bool) {
	if len(as) != 2 {
		return durVal{}, false
	}
	var env durVal
	var def durVal
	gotEnv, gotDef := false, false
	for _, x := range as {
		if x.rhs == nil {
			return durVal{}, false
		}
		if call, ok := ast.Unparen(x.rhs).(*ast.CallExpr); ok && x.multi && x.index == 0 {
			if fn := calleeFunc(x.lp.info, call); isFunc(fn, "time", "ParseDuration") && len(call.Args) == 1 {
				env = a.envDur(x.lp, call.Args[0])
				gotEnv = env.env != ""
				continue
			}
		}
		if x.multi {
			return durVal{}, false
		}
		def = a.evalDur(x.lp, x.rhs, 5)
		gotDef = def.known
	}
	if !gotEnv || !gotDef {
		return durVal{}, false
	}
	env.envDef, env.envDok = def.secs, true
	return env, true
}

// decoded explains why a field's value can't be read from code: it has a
// struct tag, so a decoder may set it, or the struct it belongs to is
// passed by pointer to a call such as viper.Unmarshal(&cfg).
func (a *analyzer) decoded(lp *loadedPkg, e ast.Expr) string {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	field, ok := lp.info.Uses[sel.Sel].(*types.Var)
	if !ok || !field.IsField() {
		return ""
	}
	for x := ast.Expr(sel.X); x != nil; {
		base := a.objOf(lp, x)
		for _, as := range a.assigns[base] {
			if as.via != "" && (as.fn == nil || a.reach[as.fn]) {
				return exprString(x) + " is passed to " + as.via + ", which may set " + exprString(e)
			}
		}
		next, ok := ast.Unparen(x).(*ast.SelectorExpr)
		if !ok {
			break
		}
		x = next.X
	}
	if tag := fieldTag(field); tag != "" {
		return exprString(e) + " has the struct tag `" + tag + "`, so a decoder may set it"
	}
	return ""
}

// fieldTag finds the tag of a struct field from the struct type that
// declares it.
func fieldTag(field *types.Var) string {
	if field.Pkg() == nil {
		return ""
	}
	scope := field.Pkg().Scope()
	for _, name := range scope.Names() {
		st, ok := scope.Lookup(name).Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if f.Name() == field.Name() && f.Pos() == field.Pos() {
				return st.Tag(i)
			}
		}
	}
	return ""
}

// durParam follows a duration parameter to the callers of its function.
// All callers have to agree.
func (a *analyzer) durParam(pi paramInfo, depth int) durVal {
	if pi.fn == "" {
		return durVal{why: "the duration is a parameter of a function literal"}
	}
	sites := a.calls[pi.fn]
	if fd := a.funcs[pi.fn]; fd != nil && fd.Recv != nil {
		sites = append(append([]callSite(nil), sites...), a.ifaceCalls[pi.name]...)
	}
	if len(sites) == 0 {
		return durVal{why: "the duration is a parameter of " + pi.name + ", which nothing in the module calls"}
	}
	var out *durVal
	for _, s := range sites {
		if pi.idx >= len(s.call.Args) {
			return durVal{why: "can't match the arguments of a call to " + pi.name}
		}
		d := a.evalDur(s.lp, s.call.Args[pi.idx], depth+1)
		if out == nil {
			out = &d
			continue
		}
		if d != *out {
			return durVal{why: "callers of " + pi.name + " pass different durations"}
		}
	}
	return *out
}

func (a *analyzer) flagDur(lp *loadedPkg, name string, def ast.Expr) durVal {
	d := durVal{flag: name}
	if tv := lp.info.Types[def]; tv.Value != nil {
		if v, ok := constant.Int64Val(constant.ToInt(tv.Value)); ok {
			d.flagDef, d.flagDok = float64(v)/1e9, true
		}
	}
	if !d.flagDok {
		d.why = "the default of flag " + name + " isn't a constant"
	}
	return d
}

func (a *analyzer) envDur(lp *loadedPkg, arg ast.Expr) durVal {
	if call, ok := ast.Unparen(arg).(*ast.CallExpr); ok {
		if fn := calleeFunc(lp.info, call); isFunc(fn, "os", "Getenv") && len(call.Args) == 1 {
			if name, ok := constString(lp.info, call.Args[0]); ok {
				return durVal{env: name, why: "it is parsed from $" + name}
			}
		}
	}
	return durVal{why: "it is parsed by time.ParseDuration from " + exprString(arg)}
}

// sleepBefore adds up time.Sleep and <-time.After calls that run before the
// Shutdown call in the same function, outside loops and function literals.
func (a *analyzer) sleepBefore(lp *loadedPkg, shutdown *ast.CallExpr, path []ast.Node) durVal {
	var body *ast.BlockStmt
	for i := len(path) - 1; i >= 0; i-- {
		switch fn := path[i].(type) {
		case *ast.FuncLit:
			body = fn.Body
		case *ast.FuncDecl:
			body = fn.Body
		}
		if body != nil {
			break
		}
	}
	total := durVal{known: true}
	if body == nil {
		return total
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ForStmt, *ast.RangeStmt:
			if n.Pos() > shutdown.Pos() || n.End() < shutdown.Pos() {
				return false
			}
		case *ast.CallExpr:
			if n.Pos() >= shutdown.Pos() {
				return false
			}
			fn := calleeFunc(lp.info, n)
			if isFunc(fn, "time", "Sleep", "After") && len(n.Args) == 1 {
				d := a.evalDur(lp, n.Args[0], 0)
				if !d.known {
					total.known = false
					total.why = "a sleep before Shutdown lasts " + exprString(n.Args[0])
				} else {
					total.secs += d.secs
				}
			}
		}
		return true
	})
	return total
}

// serveCall records a Serve call and whether the program exits as soon
// as it returns. Serve returns ErrServerClosed the moment Shutdown starts,
// so exiting then cuts the drain short.
func (a *analyzer) serveCall(lp *loadedPkg, name string, call *ast.CallExpr, path []ast.Node, pkgFunc bool) serveCall {
	s := serveCall{pos: call.Pos(), name: name, pkgFunc: pkgFunc}
	if !pkgFunc {
		s.server = a.receiverObj(lp, call)
	}
	for _, n := range path {
		if _, ok := n.(*ast.GoStmt); ok {
			s.backgound = true
			return s
		}
	}
	if pkgFunc {
		return s
	}
	s.exitsWhy, s.exitsPos = a.exitsAfter(lp, call, path, 0)
	return s
}

func (a *analyzer) exitsAfter(lp *loadedPkg, call ast.Node, path []ast.Node, depth int) (string, token.Pos) {
	if depth > 3 {
		return "", 0
	}
	// Directly inside log.Fatal(...), panic(...) or os.Exit(...).
	if len(path) > 0 {
		if parent, ok := path[len(path)-1].(*ast.CallExpr); ok {
			if why := exitCall(lp.info, parent); why != "" {
				return why + " exits with the ErrServerClosed that Serve returns when Shutdown starts", parent.Pos()
			}
		}
	}
	// if err := srv.ListenAndServe(); err != nil { log.Fatal(err) }, or
	// err := srv.Serve(l) followed by the same check.
	for i := len(path) - 1; i >= 0; i-- {
		var resultObj okey
		var stmt ast.Stmt
		switch n := path[i].(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 && ast.Unparen(n.Rhs[0]) == call {
				resultObj = a.objOf(lp, n.Lhs[0])
				stmt = n
			}
		}
		if resultObj == "" {
			continue
		}
		var block *ast.BlockStmt
		var ifs *ast.IfStmt
		if i > 0 {
			if is, ok := path[i-1].(*ast.IfStmt); ok && is.Init == stmt {
				ifs = is
			}
			block, _ = path[i-1].(*ast.BlockStmt)
		}
		if ifs != nil {
			if !mentionsServerClosed(lp.info, ifs.Cond) {
				if why, pos := firstExit(lp.info, ifs.Body); why != "" {
					return why + " runs on the ErrServerClosed that Serve returns when Shutdown starts", pos
				}
			}
			break
		}
		if block != nil {
			for j, st := range block.List {
				if st != stmt {
					continue
				}
				for _, next := range block.List[j+1:] {
					is, ok := next.(*ast.IfStmt)
					if !ok || !a.usesObj(lp, is.Cond, resultObj) {
						continue
					}
					if !mentionsServerClosed(lp.info, is.Cond) {
						if why, pos := firstExit(lp.info, is.Body); why != "" {
							return why + " runs on the ErrServerClosed that Serve returns when Shutdown starts", pos
						}
					}
					break
				}
			}
		}
		break
	}
	// Find the enclosing function and the statements that run after
	// Serve returns.
	var fnDecl *ast.FuncDecl
	for i := len(path) - 1; i >= 0; i-- {
		if _, ok := path[i].(*ast.FuncLit); ok {
			return "", 0
		}
		if fd, ok := path[i].(*ast.FuncDecl); ok {
			fnDecl = fd
			break
		}
	}
	if fnDecl == nil {
		return "", 0
	}
	returned := false
	for _, n := range path {
		if _, ok := n.(*ast.ReturnStmt); ok {
			returned = true
		}
	}
	if returned {
		// return srv.ListenAndServe(): look at the callers instead.
		obj := lp.info.Defs[fnDecl.Name]
		for _, site := range a.calls[a.key(obj)] {
			if inGo(site.path) {
				continue
			}
			if why, pos := a.exitsAfter(site.lp, site.call, site.path, depth+1); why != "" {
				return why, pos
			}
		}
		return "", 0
	}
	if fnDecl.Name.Name != "main" || fnDecl.Recv != nil || lp.pkg.Name() != "main" {
		return "", 0
	}
	if waitsAfter(lp.info, call, path) {
		return "", 0
	}
	return "main returns as soon as Serve returns, which is the moment Shutdown starts, and nothing after it waits for Shutdown to finish", 0
}

func inGo(path []ast.Node) bool {
	for _, n := range path {
		if _, ok := n.(*ast.GoStmt); ok {
			return true
		}
	}
	return false
}

// waitsAfter reports whether anything that runs after call in its
// function could wait: a channel receive, a select, a Wait method, a
// Shutdown call or a call to another function.
func waitsAfter(info *types.Info, call ast.Node, path []ast.Node) bool {
	for i := len(path) - 1; i >= 0; i-- {
		block, ok := path[i].(*ast.BlockStmt)
		if !ok {
			continue
		}
		var child ast.Node
		if i+1 < len(path) {
			child = path[i+1]
		} else {
			child = call
		}
		after := false
		for _, st := range block.List {
			if st == child {
				after = true
				continue
			}
			if after && mayWait(info, st) {
				return true
			}
		}
		if i == 0 {
			break
		}
		if _, ok := path[i-1].(*ast.FuncDecl); ok {
			break
		}
		// Statements after a loop body run again: count the loop.
		{
			switch path[i-1].(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				return true
			}
		}
	}
	return false
}

func mayWait(info *types.Info, n ast.Node) bool {
	wait := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectStmt:
			wait = true
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				wait = true
			}
		case *ast.RangeStmt:
			if t, ok := info.Types[n.X].Type.(*types.Chan); ok && t != nil {
				wait = true
			}
		case *ast.CallExpr:
			fn := calleeFunc(info, n)
			if fn == nil {
				wait = true // dynamic call: assume it may wait
				return false
			}
			if exitCall(info, n) != "" {
				return false
			}
			pkg := ""
			if fn.Pkg() != nil {
				pkg = fn.Pkg().Path()
			}
			switch {
			case fn.Name() == "Wait" || fn.Name() == "Shutdown" || fn.Name() == "Sleep":
				wait = true
			case pkg == "log" || pkg == "fmt" || pkg == "errors" || pkg == "log/slog" || pkg == "os" && fn.Name() != "Exit":
			case fn.Name() == "Error" || fn.Name() == "String" || fn.Name() == "Is" || fn.Name() == "Close":
			default:
				wait = true
			}
		}
		return !wait
	})
	return wait
}

// exitCall names the call if it ends the program.
func exitCall(info *types.Info, call *ast.CallExpr) string {
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok && id.Name == "panic" {
		if _, ok := info.Uses[id].(*types.Builtin); ok {
			return "panic"
		}
	}
	fn := calleeFunc(info, call)
	if fn == nil || fn.Pkg() == nil {
		return ""
	}
	name := fn.Name()
	switch fn.Pkg().Path() {
	case "os":
		if name == "Exit" && isFunc(fn, "os", "Exit") {
			return "os.Exit"
		}
	case "log":
		if strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic") {
			return "log." + name
		}
	}
	if strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic") {
		// zap, logrus, zerolog and similar loggers.
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			return exprString(call.Fun)
		}
	}
	return ""
}

func firstExit(info *types.Info, body *ast.BlockStmt) (string, token.Pos) {
	why, pos := "", token.Pos(0)
	ast.Inspect(body, func(n ast.Node) bool {
		if why != "" {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.IfStmt:
			// A nested if that checks ErrServerClosed guards its exit.
			if mentionsServerClosed(info, n.Cond) {
				return false
			}
		case *ast.CallExpr:
			if w := exitCall(info, n); w != "" {
				why, pos = w, n.Pos()
			}
		}
		return true
	})
	return why, pos
}

func mentionsServerClosed(info *types.Info, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if obj := info.Uses[sel.Sel]; obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == "ErrServerClosed" {
				found = true
			}
		}
		if id, ok := n.(*ast.Ident); ok {
			if obj := info.Uses[id]; obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == "ErrServerClosed" {
				found = true
			}
		}
		return !found
	})
	return found
}

func (a *analyzer) usesObj(lp *loadedPkg, e ast.Expr, obj okey) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && a.key(lp.info.Uses[id]) == obj {
			found = true
		}
		return !found
	})
	return found
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.CallExpr:
		return exprString(x.Fun) + "(...)"
	case *ast.ParenExpr:
		return "(" + exprString(x.X) + ")"
	case *ast.BasicLit:
		return x.Value
	case *ast.BinaryExpr:
		return exprString(x.X) + " " + x.Op.String() + " " + exprString(x.Y)
	case *ast.IndexExpr:
		return exprString(x.X) + "[...]"
	}
	return "an expression"
}

// where formats a position relative to the module root.
func (p *program) where(pos token.Pos) string {
	if !pos.IsValid() {
		return ""
	}
	position := p.fset.Position(pos)
	name := position.Filename
	if rel, err := filepath.Rel(p.root, name); err == nil && !strings.HasPrefix(rel, "..") {
		name = rel
	}
	return filepath.ToSlash(name) + ":" + strconv.Itoa(position.Line)
}

func sortedPositions(p *program, ps []token.Pos) []string {
	var out []string
	for _, pos := range ps {
		out = append(out, p.where(pos))
	}
	sort.Strings(out)
	return out
}
