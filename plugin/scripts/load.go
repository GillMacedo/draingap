package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	goparser "go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// untrusted marks an error that means the input can't be trusted, so
// nothing is reported and the exit code is 2.
type untrusted struct{ msg string }

func (e untrusted) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return untrusted{fmt.Sprintf(format, args...)}
}

// listModule is the module part of go list -json output.
type listModule struct {
	Path string
	Main bool
	Dir  string
}

// listPkg is the part of go list -json output draingap uses.
type listPkg struct {
	ImportPath string
	Name       string
	Dir        string
	Standard   bool
	DepOnly    bool
	Export     string
	GoFiles    []string
	CgoFiles   []string
	Imports    []string
	ImportMap  map[string]string
	Module     *listModule
	Error      *struct{ Err string }
	DepsErrors []*struct{ Err string }
	Incomplete bool
}

const listFields = "ImportPath,Name,Dir,Standard,DepOnly,Export,GoFiles,CgoFiles,Imports,ImportMap,Module,Error,DepsErrors,Incomplete"

// goEnv is the environment draingap runs the go command with. The program
// runs in a Linux container, so it is read as GOOS=linux. GOTOOLCHAIN=local
// keeps the export data in the format of the Go that built draingap.
func goEnv() []string {
	env := append(os.Environ(), "GOOS=linux", "GOTOOLCHAIN=local")
	if _, ok := os.LookupEnv("CGO_ENABLED"); !ok {
		env = append(env, "CGO_ENABLED=0")
	}
	return env
}

// goList runs go list -deps -export on the package in dir. -export makes the
// go command compile every package, so code that doesn't build is refused
// here rather than read.
func goList(dir, tags string) ([]*listPkg, error) {
	args := []string{"list", "-deps", "-export", "-json=" + listFields}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "--", ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = goEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, refuse("can't run the go command: %v", err)
		}
		return nil, refuse("go list -deps -export failed in %s, so the program doesn't build for linux or isn't a Go package: %s", dir, firstLines(stderr.String(), 6))
	}
	return decodeList(&stdout)
}

// decodeList reads the stream of JSON objects go list -json prints.
func decodeList(r io.Reader) ([]*listPkg, error) {
	dec := json.NewDecoder(r)
	var pkgs []*listPkg
	for {
		var p listPkg
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, refuse("go list printed output that isn't complete JSON: %v", err)
		}
		if p.ImportPath == "" {
			return nil, refuse("go list printed a package with no import path")
		}
		if p.Error != nil {
			return nil, refuse("package %s has an error: %s", p.ImportPath, oneLine(p.Error.Err))
		}
		for _, e := range p.DepsErrors {
			if e != nil {
				return nil, refuse("a dependency of %s has an error: %s", p.ImportPath, oneLine(e.Err))
			}
		}
		if p.Incomplete {
			return nil, refuse("go list says package %s is incomplete", p.ImportPath)
		}
		pkgs = append(pkgs, &p)
	}
	if len(pkgs) == 0 {
		return nil, refuse("go list found no packages")
	}
	return pkgs, nil
}

// loadedPkg is one package of the program's own module, type-checked.
type loadedPkg struct {
	path  string
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// program is everything draingap read from the Go side.
type program struct {
	fset    *token.FileSet
	mainPkg string
	own     []*loadedPkg
	// depSignals lists dependencies outside the module that call os/signal.
	depSignals []string
	root       string // directory paths are shown relative to
}

// loadProgram lists the package in dir and type-checks every package of the
// main module it uses. Dependencies are read from the export data go list
// built, so their source is only parsed to look for os/signal calls.
func loadProgram(dir, tags string) (*program, error) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, refuse("--pkg %s is not a directory", dir)
	}
	pkgs, err := goList(dir, tags)
	if err != nil {
		return nil, err
	}
	var target *listPkg
	for _, p := range pkgs {
		if !p.DepOnly {
			if target != nil {
				return nil, refuse("--pkg %s matched more than one package", dir)
			}
			target = p
		}
	}
	if target == nil {
		return nil, refuse("go list printed no package for %s", dir)
	}
	if target.Name != "main" {
		return nil, refuse("%s is package %s, not a main package: point --pkg at the directory of the program's func main", dir, target.Name)
	}
	if target.Module == nil {
		return nil, refuse("%s is not in a Go module", dir)
	}
	exports := map[string]string{}
	for _, p := range pkgs {
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}
	prog := &program{fset: token.NewFileSet(), mainPkg: target.ImportPath, root: target.Module.Dir}
	for _, p := range pkgs {
		if p.Standard {
			continue
		}
		own := p.Module != nil && p.Module.Path == target.Module.Path
		if !own {
			if depCallsSignal(p) {
				prog.depSignals = append(prog.depSignals, p.ImportPath)
			}
			continue
		}
		lp, err := checkPkg(prog.fset, p, exports)
		if err != nil {
			return nil, err
		}
		if p == target && !hasMainFunc(lp) {
			return nil, refuse("package main in %s has no func main, so it doesn't link into a program", dir)
		}
		prog.own = append(prog.own, lp)
	}
	if len(prog.own) == 0 {
		return nil, refuse("no package of module %s was found", target.Module.Path)
	}
	return prog, nil
}

func checkPkg(fset *token.FileSet, p *listPkg, exports map[string]string) (*loadedPkg, error) {
	if len(p.CgoFiles) > 0 {
		return nil, refuse("package %s uses cgo, which draingap can't read yet", p.ImportPath)
	}
	lp := &loadedPkg{path: p.ImportPath}
	for _, name := range p.GoFiles {
		f, err := goparser.ParseFile(fset, filepath.Join(p.Dir, name), nil, goparser.ParseComments|goparser.SkipObjectResolution)
		if err != nil {
			return nil, refuse("can't parse %s: %v", filepath.Join(p.Dir, name), err)
		}
		lp.files = append(lp.files, f)
	}
	if len(lp.files) == 0 {
		return nil, refuse("package %s has no Go files for linux", p.ImportPath)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		if mapped, ok := p.ImportMap[path]; ok {
			path = mapped
		}
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", lookup),
		Sizes:    types.SizesFor("gc", runtime.GOARCH),
	}
	lp.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	pkg, err := conf.Check(p.ImportPath, fset, lp.files, lp.info)
	if err != nil {
		return nil, refuse("can't type-check %s: %s (draingap was built with %s; build it with the same Go as the go command on PATH)", p.ImportPath, oneLine(err.Error()), runtime.Version())
	}
	lp.pkg = pkg
	return lp, nil
}

func hasMainFunc(lp *loadedPkg) bool {
	for _, f := range lp.files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" && fd.Type.TypeParams == nil {
				return true
			}
		}
	}
	return false
}

// depCallsSignal reports whether a dependency's source calls os/signal's
// Notify or NotifyContext, which means a framework may handle the stop
// signal for the program.
func depCallsSignal(p *listPkg) bool {
	imports := false
	for _, imp := range p.Imports {
		if imp == "os/signal" {
			imports = true
		}
	}
	if !imports {
		return false
	}
	fset := token.NewFileSet()
	for _, name := range p.GoFiles {
		f, err := goparser.ParseFile(fset, filepath.Join(p.Dir, name), nil, goparser.SkipObjectResolution)
		if err != nil {
			// The go command compiled it, so treat an unreadable file as a
			// possible handler rather than ignoring it.
			return true
		}
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "os/signal" {
				local = "signal"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && (sel.Sel.Name == "Notify" || sel.Sel.Name == "NotifyContext") {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// firstLines keeps the first n lines of go command errors, leaving out
// its "go: downloading" progress lines.
func firstLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "go: downloading ") {
			keep = append(keep, l)
		}
	}
	s = strings.TrimSpace(strings.Join(keep, "\n"))
	if s == "" {
		return "(no message)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("(%d more lines)", len(lines)-n))
	}
	return strings.Join(lines, "; ")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
