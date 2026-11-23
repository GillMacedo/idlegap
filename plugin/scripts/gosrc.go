package main

import (
	"bytes"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/printer"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// goPkg is the non-test Go files of one directory, parsed together so
// constants and flags declared in one file resolve in another.
type goPkg struct {
	dir   string
	fset  *token.FileSet
	files []*ast.File
	// imports maps each file to its import names: local name -> path.
	imports map[*ast.File]map[string]string
	// consts and vars hold package-level declarations with a value.
	consts map[string]ast.Expr
	vars   map[string]ast.Expr
	// assigned counts plain assignments to each package-level name.
	assigned map[string]int
	// flagCalls are calls that define a flag, keyed by the text of the
	// variable they store into (for the ...Var forms).
	flagVars map[string]flagDef
	// tied holds the positions of field assignments tied to a server.
	tied map[token.Pos]bool
}

type flagDef struct {
	name string   // flag name, without dashes
	def  ast.Expr // default value expression
	kind string   // "duration", "string" or "int"
	file *ast.File
}

// server is one http.Server the program creates, or a call to a helper
// such as http.ListenAndServe that uses a server with no timeouts.
type server struct {
	pkg  *goPkg
	file *ast.File
	pos  token.Position
	fn   *ast.BlockStmt // enclosing function body, nil at package level
	// Field expressions, nil when the field isn't set.
	idle, read, addr ast.Expr
	// fieldPos records where each field was set, for messages.
	idleNote, readNote string
	// keepAlivesOff is true when SetKeepAlivesEnabled(false) is called.
	keepAlivesOff bool
	// conflict names a field set in more than one place.
	conflict string
	// stray describes an IdleTimeout or ReadTimeout assignment elsewhere
	// in the package that can't be tied to a server.
	stray string
	// helper is set for http.ListenAndServe and friends.
	helper string
	// noAddr is true when the server is started with Serve(listener),
	// where Addr isn't used.
	addrArg ast.Expr
}

// loadGoPackages reads the directories named by patterns. A pattern ending
// in /... includes subdirectories, skipping vendor, testdata and names
// starting with . or _.
func loadGoPackages(patterns []string) ([]*goPkg, error) {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, p := range patterns {
		recursive := false
		if p == "..." {
			p, recursive = ".", true
		} else if strings.HasSuffix(p, "/...") {
			p, recursive = strings.TrimSuffix(p, "/..."), true
			if p == "" {
				p = "/"
			}
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, untrusted("Go package %s: %v", p, cleanErr(err))
		}
		if !st.IsDir() {
			if strings.HasSuffix(p, ".go") {
				return nil, untrusted("%s is a file; pass its directory (idlegap reads every non-test .go file in a directory as one package)", p)
			}
			return nil, untrusted("Go package %s is not a directory", p)
		}
		if !recursive {
			add(filepath.Clean(p))
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != p && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
					return filepath.SkipDir
				}
				if hasGoFiles(path) {
					add(filepath.Clean(path))
				}
			}
			return nil
		})
		if err != nil {
			return nil, untrusted("reading %s: %v", p, cleanErr(err))
		}
	}
	var pkgs []*goPkg
	for _, d := range dirs {
		pkg, err := parseDir(d)
		if err != nil {
			return nil, err
		}
		if pkg != nil {
			pkgs = append(pkgs, pkg)
		}
	}
	if len(pkgs) == 0 {
		return nil, untrusted("no Go files found in %s", strings.Join(patterns, " "))
	}
	return pkgs, nil
}

func hasGoFiles(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && isSourceName(e.Name()) {
			return true
		}
	}
	return false
}

func isSourceName(n string) bool {
	return strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "_")
}

func parseDir(dir string) (*goPkg, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, untrusted("reading %s: %v", dir, cleanErr(err))
	}
	pkg := &goPkg{dir: dir, fset: token.NewFileSet(), imports: map[*ast.File]map[string]string{},
		consts: map[string]ast.Expr{}, vars: map[string]ast.Expr{}, assigned: map[string]int{}, flagVars: map[string]flagDef{}}
	for _, e := range ents {
		if e.IsDir() || !isSourceName(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, untrusted("reading %s: %v", path, cleanErr(err))
		}
		if err := checkText(path, src, "Go file"); err != nil {
			return nil, err
		}
		f, err := goparser.ParseFile(pkg.fset, path, src, goparser.ParseComments|goparser.AllErrors)
		if err != nil {
			return nil, untrusted("Go file %s doesn't parse, so it may be cut off or not Go: %s", path, firstLine(err.Error()))
		}
		if ignored(f) {
			continue
		}
		pkg.files = append(pkg.files, f)
	}
	if len(pkg.files) == 0 {
		return nil, nil
	}
	for _, f := range pkg.files {
		imps := map[string]string{}
		for _, is := range f.Imports {
			path, _ := strconv.Unquote(is.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if strings.HasPrefix(name, "v") && isDigits(name[1:]) && strings.Contains(path, "/") {
				// gopkg.in/x.v1 style or /v2 major suffixes.
				p := strings.TrimSuffix(path, "/"+name)
				name = p[strings.LastIndex(p, "/")+1:]
			}
			if i := strings.Index(name, ".v"); i > 0 {
				name = name[:i]
			}
			if is.Name != nil {
				name = is.Name.Name
			}
			imps[name] = path
		}
		pkg.imports[f] = imps
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if gd.Tok == token.CONST {
						pkg.consts[n.Name] = vs.Values[i]
					} else {
						pkg.vars[n.Name] = vs.Values[i]
					}
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if n.Tok == token.ASSIGN {
					for _, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							pkg.assigned[id.Name]++
						}
					}
				}
			case *ast.CallExpr:
				pkg.noteFlagVar(f, n)
			}
			return true
		})
	}
	return pkg, nil
}

// ignored reports whether a file has a //go:build ignore constraint.
func ignored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build") {
				fields := strings.Fields(strings.TrimPrefix(c.Text, "//go:build"))
				if len(fields) == 1 && fields[0] == "ignore" {
					return true
				}
			}
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// flagMethod describes the flag-defining functions of the flag package,
// pflag and FlagSet methods of either: which argument is the name and
// which the default.
var flagMethods = map[string]struct {
	kind          string
	varForm       bool
	nameAt, defAt int
}{
	"Duration": {"duration", false, 0, 1}, "DurationP": {"duration", false, 0, 2},
	"DurationVar": {"duration", true, 1, 2}, "DurationVarP": {"duration", true, 1, 3},
	"String": {"string", false, 0, 1}, "StringP": {"string", false, 0, 2},
	"StringVar": {"string", true, 1, 2}, "StringVarP": {"string", true, 1, 3},
	"Int": {"int", false, 0, 1}, "IntP": {"int", false, 0, 2},
	"IntVar": {"int", true, 1, 2}, "IntVarP": {"int", true, 1, 3},
}

// flagCall reports whether call defines a flag, as flag.Duration(...),
// fs.DurationVar(...) or cmd.Flags().StringVarP(...).
func flagCall(call *ast.CallExpr) (name string, def ast.Expr, kind string, target ast.Expr, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return
	}
	m, known := flagMethods[sel.Sel.Name]
	if !known || len(call.Args) <= m.defAt {
		return
	}
	lit, isLit := call.Args[m.nameAt].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		return
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil || name == "" {
		return "", nil, "", nil, false
	}
	if m.varForm {
		u, isAddr := call.Args[0].(*ast.UnaryExpr)
		if !isAddr || u.Op != token.AND {
			return "", nil, "", nil, false
		}
		target = u.X
	}
	return name, call.Args[m.defAt], m.kind, target, true
}

func (p *goPkg) noteFlagVar(f *ast.File, call *ast.CallExpr) {
	name, def, kind, target, ok := flagCall(call)
	if !ok || target == nil {
		return
	}
	p.flagVars[exprText(target)] = flagDef{name: name, def: def, kind: kind, file: f}
}

// findServers lists the servers created in pkg.
func (p *goPkg) findServers() []*server {
	var out []*server
	for _, f := range p.files {
		imps := p.imports[f]
		httpName := ""
		for n, path := range imps {
			if path == "net/http" {
				httpName = n
			}
		}
		if httpName == "" {
			continue
		}
		isServerType := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Server" {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && id.Name == httpName
		}
		var visit func(body *ast.BlockStmt, root ast.Node)
		visit = func(body *ast.BlockStmt, root ast.Node) {
			ast.Inspect(root, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					if n.Body != body {
						visit(n.Body, n.Body)
						return false
					}
				case *ast.CompositeLit:
					if isServerType(n.Type) {
						out = append(out, p.serverFromLit(f, body, n, ownerOf(root, n)))
					}
				case *ast.ValueSpec:
					if n.Type != nil && isServerType(n.Type) && len(n.Values) == 0 {
						for _, name := range n.Names {
							s := &server{pkg: p, file: f, pos: p.fset.Position(name.Pos()), fn: body}
							p.applyAssignments(s, body, name.Name)
							out = append(out, s)
						}
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						break
					}
					id, ok := sel.X.(*ast.Ident)
					if !ok || id.Name != httpName {
						break
					}
					switch sel.Sel.Name {
					case "ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS":
						s := &server{pkg: p, file: f, pos: p.fset.Position(n.Pos()), fn: body, helper: httpName + "." + sel.Sel.Name}
						if strings.HasPrefix(sel.Sel.Name, "ListenAndServe") && len(n.Args) > 0 {
							s.addrArg = n.Args[0]
						}
						out = append(out, s)
					}
				}
				return true
			})
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					visit(d.Body, d.Body)
				}
			case *ast.GenDecl:
				visit(nil, d)
			}
		}
	}
	if stray := p.strayAssignment(); stray != "" {
		for _, s := range out {
			if s.helper == "" {
				s.stray = stray
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].pos, out[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return out
}

// strayAssignment finds an assignment to an IdleTimeout or ReadTimeout
// field that wasn't tied to a server, such as one made in another
// function than the one that creates the server. It may also be a
// config struct's field, so it only makes servers "not checked".
func (p *goPkg) strayAssignment() string {
	stray := ""
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || stray != "" {
				return stray == ""
			}
			for _, l := range as.Lhs {
				sel, ok := l.(*ast.SelectorExpr)
				if !ok || p.tied[l.Pos()] {
					continue
				}
				if sel.Sel.Name == "IdleTimeout" || sel.Sel.Name == "ReadTimeout" {
					pos := p.fset.Position(l.Pos())
					stray = fmt.Sprintf("%s is assigned at %s:%d, and idlegap can't tell which server that is", exprText(sel), pos.Filename, pos.Line)
				}
			}
			return true
		})
	}
	return stray
}

// ownerOf finds the expression a composite literal is stored in, such as
// "srv" for srv := &http.Server{...}, or "" when it isn't stored directly.
func ownerOf(root ast.Node, lit *ast.CompositeLit) string {
	owner := ""
	ast.Inspect(root, func(n ast.Node) bool {
		if owner != "" {
			return false
		}
		match := func(v ast.Expr) bool {
			if u, ok := v.(*ast.UnaryExpr); ok && u.Op == token.AND {
				v = u.X
			}
			return v == lit
		}
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == len(n.Rhs) {
				for i, r := range n.Rhs {
					if match(r) {
						owner = exprText(n.Lhs[i])
					}
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) == len(n.Values) {
				for i, r := range n.Values {
					if match(r) {
						owner = n.Names[i].Name
					}
				}
			}
		}
		return true
	})
	return owner
}

func (p *goPkg) serverFromLit(f *ast.File, body *ast.BlockStmt, lit *ast.CompositeLit, owner string) *server {
	s := &server{pkg: p, file: f, pos: p.fset.Position(lit.Pos()), fn: body}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "IdleTimeout":
			s.idle = kv.Value
		case "ReadTimeout":
			s.read = kv.Value
		case "Addr":
			s.addr = kv.Value
		}
	}
	if owner != "" && body != nil {
		p.applyAssignments(s, body, owner)
	}
	return s
}

// applyAssignments applies owner.Field = value statements and
// owner.SetKeepAlivesEnabled(false) calls in the function body.
func (p *goPkg) applyAssignments(s *server, body *ast.BlockStmt, owner string) {
	if body == nil {
		return
	}
	set := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) || n.Tok != token.ASSIGN {
				return true
			}
			for i, l := range n.Lhs {
				sel, ok := l.(*ast.SelectorExpr)
				if !ok || exprText(sel.X) != owner {
					continue
				}
				pos := p.fset.Position(n.Pos())
				if p.tied == nil {
					p.tied = map[token.Pos]bool{}
				}
				p.tied[l.Pos()] = true
				switch sel.Sel.Name {
				case "IdleTimeout":
					set["IdleTimeout"]++
					s.idle = n.Rhs[i]
					s.idleNote = fmt.Sprintf("set at line %d", pos.Line)
				case "ReadTimeout":
					set["ReadTimeout"]++
					s.read = n.Rhs[i]
					s.readNote = fmt.Sprintf("set at line %d", pos.Line)
				case "Addr":
					set["Addr"]++
					s.addr = n.Rhs[i]
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "SetKeepAlivesEnabled" && exprText(sel.X) == owner && len(n.Args) == 1 {
				if id, ok := n.Args[0].(*ast.Ident); ok && id.Name == "false" {
					s.keepAlivesOff = true
				}
			}
		}
		return true
	})
	for _, f := range []string{"IdleTimeout", "ReadTimeout", "Addr"} {
		if set[f] > 1 {
			s.conflict = f
		}
	}
}

// value is a resolved constant: a duration in nanoseconds, a number or a
// string, with a short description of where it came from.
type value struct {
	num  float64
	str  string
	isS  bool
	from string // "flag -idle-timeout default", "--idle-timeout in container args"
}

// evalEnv carries container arguments that override flag defaults.
type evalEnv struct {
	args  map[string]string // flag name -> value from container args
	depth int
}

type unresolved struct{ reason string }

func (u unresolved) Error() string { return u.reason }

func (s *server) eval(e ast.Expr, env *evalEnv) (value, error) {
	return s.pkg.eval(s.file, s.fn, e, env)
}

func (p *goPkg) eval(f *ast.File, fn *ast.BlockStmt, e ast.Expr, env *evalEnv) (v value, err error) {
	if env == nil {
		env = &evalEnv{}
	}
	if env.depth > 40 {
		return value{}, unresolved{"too many levels of indirection"}
	}
	env.depth++
	defer func() { env.depth-- }()
	imps := p.imports[f]
	pkgOf := func(x ast.Expr) string {
		if id, ok := x.(*ast.Ident); ok {
			return imps[id.Name]
		}
		return ""
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return p.eval(f, fn, e.X, env)
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT, token.FLOAT:
			n, err := strconv.ParseFloat(strings.ReplaceAll(e.Value, "_", ""), 64)
			if err != nil {
				if i, err2 := strconv.ParseInt(strings.ReplaceAll(e.Value, "_", ""), 0, 64); err2 == nil {
					return value{num: float64(i)}, nil
				}
				return value{}, unresolved{"can't read number " + e.Value}
			}
			return value{num: n}, nil
		case token.STRING:
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				return value{}, unresolved{"can't read string " + e.Value}
			}
			return value{str: s, isS: true}, nil
		}
	case *ast.UnaryExpr:
		if e.Op == token.SUB || e.Op == token.ADD {
			v, err := p.eval(f, fn, e.X, env)
			if err != nil || v.isS {
				return v, err
			}
			if e.Op == token.SUB {
				v.num = -v.num
			}
			return v, nil
		}
	case *ast.BinaryExpr:
		a, err := p.eval(f, fn, e.X, env)
		if err != nil {
			return a, err
		}
		b, err := p.eval(f, fn, e.Y, env)
		if err != nil {
			return b, err
		}
		from := a.from
		if from == "" {
			from = b.from
		}
		if a.isS || b.isS {
			if a.isS && b.isS && e.Op == token.ADD {
				return value{str: a.str + b.str, isS: true, from: from}, nil
			}
			break
		}
		switch e.Op {
		case token.MUL:
			return value{num: a.num * b.num, from: from}, nil
		case token.ADD:
			return value{num: a.num + b.num, from: from}, nil
		case token.SUB:
			return value{num: a.num - b.num, from: from}, nil
		case token.QUO:
			if b.num == 0 {
				return value{}, unresolved{"division by zero"}
			}
			q := a.num / b.num
			if a.num == math.Trunc(a.num) && b.num == math.Trunc(b.num) && !isFloatLit(e.X) && !isFloatLit(e.Y) {
				q = math.Trunc(q)
			}
			return value{num: q, from: from}, nil
		}
	case *ast.SelectorExpr:
		if pkgOf(e.X) == "time" {
			if d, ok := timeUnits[e.Sel.Name]; ok {
				return value{num: float64(d)}, nil
			}
		}
		if text := exprText(e); p.flagVars[text].name != "" {
			return p.flagValue(p.flagVars[text], env)
		}
		return value{}, unresolved{"comes from " + exprText(e) + ", which idlegap can't follow"}
	case *ast.StarExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			break
		}
		def := p.lookup(fn, id.Name)
		if def == nil {
			break
		}
		if call, ok := def.(*ast.CallExpr); ok {
			if name, d, kind, target, ok := flagCall(call); ok && target == nil {
				return p.flagValue(flagDef{name: name, def: d, kind: kind, file: f}, env)
			}
		}
	case *ast.Ident:
		switch e.Name {
		case "true", "false", "nil":
			return value{}, unresolved{e.Name}
		}
		if fd, ok := p.flagVars[e.Name]; ok {
			return p.flagValue(fd, env)
		}
		if def := p.lookup(fn, e.Name); def != nil {
			return p.eval(f, fn, def, env)
		}
		if why := p.whyNotLookup(fn, e.Name); why != "" {
			return value{}, unresolved{why}
		}
		return value{}, unresolved{"comes from " + e.Name + ", which idlegap can't follow"}
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			pkg := pkgOf(sel.X)
			switch {
			case pkg == "time" && sel.Sel.Name == "Duration" && len(e.Args) == 1:
				return p.eval(f, fn, e.Args[0], env)
			case pkg == "fmt" && sel.Sel.Name == "Sprintf" && len(e.Args) >= 1:
				return p.sprintf(f, fn, e.Args, env)
			case pkg == "strconv" && sel.Sel.Name == "Itoa" && len(e.Args) == 1:
				v, err := p.eval(f, fn, e.Args[0], env)
				if err != nil || v.isS {
					return value{}, unresolvedOr(err, "strconv.Itoa of a string")
				}
				return value{str: strconv.FormatInt(int64(v.num), 10), isS: true, from: v.from}, nil
			case pkg == "net" && sel.Sel.Name == "JoinHostPort" && len(e.Args) == 2:
				h, err := p.eval(f, fn, e.Args[0], env)
				if err != nil {
					return h, err
				}
				pt, err := p.eval(f, fn, e.Args[1], env)
				if err != nil {
					return pt, err
				}
				from := h.from
				if from == "" {
					from = pt.from
				}
				return value{str: h.str + ":" + pt.str, isS: true, from: from}, nil
			case pkg == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv"):
				return value{}, unresolved{"comes from the environment (" + exprText(e) + ")"}
			}
		}
		if id, ok := e.Fun.(*ast.Ident); ok && len(e.Args) == 1 {
			switch id.Name {
			case "int", "int64", "float64", "string":
				return p.eval(f, fn, e.Args[0], env)
			}
		}
		return value{}, unresolved{"comes from " + exprText(e) + ", which idlegap can't follow"}
	}
	return value{}, unresolved{"comes from " + exprText(e) + ", which idlegap can't follow"}
}

func unresolvedOr(err error, reason string) error {
	if err != nil {
		return err
	}
	return unresolved{reason}
}

func isFloatLit(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.FLOAT
}

var timeUnits = map[string]time.Duration{
	"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
	"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

func (p *goPkg) flagValue(fd flagDef, env *evalEnv) (value, error) {
	if raw, ok := env.args[fd.name]; ok {
		from := "--" + fd.name + " in the container's args"
		switch fd.kind {
		case "duration":
			d, err := time.ParseDuration(raw)
			if err != nil {
				return value{}, unresolved{fmt.Sprintf("the container's args set --%s to %q, which isn't a Go duration", fd.name, raw)}
			}
			return value{num: float64(d), from: from}, nil
		case "int":
			n, err := strconv.ParseInt(raw, 0, 64)
			if err != nil {
				return value{}, unresolved{fmt.Sprintf("the container's args set --%s to %q, which isn't a number", fd.name, raw)}
			}
			return value{num: float64(n), from: from}, nil
		default:
			return value{str: raw, isS: true, from: from}, nil
		}
	}
	v, err := p.eval(fd.file, nil, fd.def, env)
	if err != nil {
		return v, err
	}
	v.from = "default of flag -" + fd.name
	return v, nil
}

// lookup finds the single definition of name, in the function body first,
// then at package level. It returns nil when there is none or more than
// one.
func (p *goPkg) lookup(fn *ast.BlockStmt, name string) ast.Expr {
	if fn != nil {
		defs, count := localDefs(fn, name)
		if count == 1 && len(defs) == 1 {
			return defs[0]
		}
		if count > 0 {
			return nil
		}
	}
	if c, ok := p.consts[name]; ok {
		return c
	}
	if v, ok := p.vars[name]; ok && p.assigned[name] == 0 {
		return v
	}
	return nil
}

func (p *goPkg) whyNotLookup(fn *ast.BlockStmt, name string) string {
	if fn != nil {
		if _, count := localDefs(fn, name); count > 1 {
			return name + " is assigned more than once"
		}
		if _, count := localDefs(fn, name); count == 1 {
			return "comes from " + name + ", which idlegap can't follow"
		}
	}
	if _, ok := p.vars[name]; ok && p.assigned[name] > 0 {
		return name + " is assigned at run time"
	}
	return ""
}

// localDefs returns the values assigned to name in body, and how many
// assignments there are (including ones without a single value).
func localDefs(body *ast.BlockStmt, name string) ([]ast.Expr, int) {
	var defs []ast.Expr
	count := 0
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					count++
					if len(n.Lhs) == len(n.Rhs) && n.Tok != token.DEFINE || n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
						if n.Tok == token.ASSIGN || n.Tok == token.DEFINE {
							defs = append(defs, n.Rhs[i])
						}
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name == name {
					count++
					if i < len(n.Values) {
						defs = append(defs, n.Values[i])
					}
				}
			}
		case *ast.RangeStmt:
			for _, x := range []ast.Expr{n.Key, n.Value} {
				if id, ok := x.(*ast.Ident); ok && id.Name == name {
					count += 2
				}
			}
		case *ast.FuncType:
			if n.Params != nil {
				for _, fld := range n.Params.List {
					for _, id := range fld.Names {
						if id.Name == name {
							count += 2
						}
					}
				}
			}
		}
		return true
	})
	return defs, count
}

func (p *goPkg) sprintf(f *ast.File, fn *ast.BlockStmt, args []ast.Expr, env *evalEnv) (value, error) {
	fv, err := p.eval(f, fn, args[0], env)
	if err != nil || !fv.isS {
		return value{}, unresolvedOr(err, "fmt.Sprintf with a format that isn't a string")
	}
	var b strings.Builder
	from := ""
	ai := 1
	format := fv.str
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(format) {
			return value{}, unresolved{"bad format"}
		}
		switch format[i] {
		case '%':
			b.WriteByte('%')
		case 'd', 's', 'v':
			if ai >= len(args) {
				return value{}, unresolved{"fmt.Sprintf with too few arguments"}
			}
			v, err := p.eval(f, fn, args[ai], env)
			if err != nil {
				return v, err
			}
			ai++
			if from == "" {
				from = v.from
			}
			if v.isS {
				b.WriteString(v.str)
			} else {
				b.WriteString(strconv.FormatFloat(v.num, 'f', -1, 64))
			}
		default:
			return value{}, unresolved{"fmt.Sprintf verb %" + string(format[i])}
		}
	}
	return value{str: b.String(), isS: true, from: from}, nil
}

// exprText prints an expression compactly.
func exprText(e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return "?"
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// checkText refuses input that can't be a complete text file of the given
// kind. Manifests must end with a newline, because YAML cut off in the
// middle of a value can still parse, as "seconds=12" from "seconds=120".
// Go files don't need one: the parser rejects a file cut inside a
// declaration, and real Go files sometimes lack the final newline.
func checkText(path string, src []byte, kind string) error {
	if len(bytes.TrimSpace(src)) == 0 {
		return untrusted("%s %s is empty", kind, path)
	}
	if bytes.IndexByte(src, 0) >= 0 {
		return untrusted("%s %s is binary", kind, path)
	}
	if !utf8.Valid(src) {
		return untrusted("%s %s isn't valid UTF-8", kind, path)
	}
	if kind != "Go file" && src[len(src)-1] != '\n' {
		return untrusted("%s %s doesn't end with a newline, so it may be cut off; if the file is complete, add a newline at the end", kind, path)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
