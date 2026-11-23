// Command idlegap compares the idle timeout of each Go http.Server with
// the upstream idle timeout of the proxy or load balancer in front of it,
// read from Kubernetes manifests, and reports servers that close idle
// keep-alive connections first.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "0.1.0"

// Exit codes.
const (
	exitOK       = 0
	exitFindings = 1
	exitUsage    = 2
	exitInput    = 3
)

// inputError means the input can't be trusted, so no result is printed.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

func untrusted(format string, a ...any) error { return inputError{fmt.Sprintf(format, a...)} }

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usageErr(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// cleanErr drops the operation prefix from path errors, which already
// name the path in the message around them.
func cleanErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	k8s          []string
	upstreamIdle time.Duration
	format       string
	failOn       string
	pkgs         []string
}

func parseArgs(args []string, stderr io.Writer) (*options, bool, error) {
	fl := flag.NewFlagSet("idlegap", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var o options
	var k8s multiFlag
	var upstream string
	showVersion := false
	fl.Var(&k8s, "k8s", "Kubernetes manifest file, directory or glob (repeatable)")
	fl.StringVar(&upstream, "upstream-idle", "", "upstream idle timeout of the proxy, such as 60s, for when it isn't in the manifests")
	fl.StringVar(&o.format, "format", "text", "output format: text or json")
	fl.StringVar(&o.failOn, "fail-on", "warn", "exit 1 on findings at this level or worse: error, warn or never")
	fl.BoolVar(&showVersion, "version", false, "print the version")
	fl.Usage = func() {
		fmt.Fprint(stderr, `usage: idlegap [flags] GO_PACKAGE_DIR...

Compares each Go http.Server's idle timeout with the upstream idle timeout
of the proxy in front of it. A directory ending in /... includes its
subdirectories.

  idlegap --k8s deploy/ ./cmd/api
  idlegap --upstream-idle 60s ./...

Flags:
`)
		fl.PrintDefaults()
		fmt.Fprint(stderr, `
Exit codes: 0 nothing found, 1 findings at --fail-on level, 2 bad usage,
3 input that can't be trusted or nothing to compare.
`)
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, true, nil
		}
		return nil, false, usageError{err.Error()}
	}
	if showVersion {
		return nil, true, nil
	}
	o.k8s = k8s
	o.pkgs = fl.Args()
	switch o.format {
	case "text", "json":
	default:
		return nil, false, usageErr("--format must be text or json, not %q", o.format)
	}
	switch o.failOn {
	case "error", "warn", "never":
	default:
		return nil, false, usageErr("--fail-on must be error, warn or never, not %q", o.failOn)
	}
	if upstream != "" {
		d, err := parseSeconds(upstream)
		if err != nil {
			return nil, false, usageErr("--upstream-idle %q: %v", upstream, err)
		}
		o.upstreamIdle = d
	}
	if len(o.pkgs) == 0 {
		return nil, false, usageErr("name at least one Go package directory, such as ./cmd/api or ./...")
	}
	if len(o.k8s) == 0 && o.upstreamIdle == 0 {
		return nil, false, usageErr("pass --k8s with your manifests, or --upstream-idle with the proxy's idle timeout")
	}
	return &o, false, nil
}

// parseSeconds reads a Go duration, or a bare number of seconds as proxies
// write it.
func parseSeconds(s string) (time.Duration, error) {
	var d time.Duration
	if n, err := strconv.Atoi(s); err == nil {
		d = time.Duration(n) * time.Second
	} else {
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, errors.New("not a duration such as 60s or 10m")
		}
	}
	if d <= 0 {
		return 0, errors.New("must be more than zero")
	}
	return d, nil
}

// report is the full result of one run.
type report struct {
	servers  []*server
	pairs    []*pair
	findings []finding
}

func run(o *options) (*report, error) {
	pkgs, err := loadGoPackages(o.pkgs)
	if err != nil {
		return nil, err
	}
	var servers []*server
	for _, p := range pkgs {
		servers = append(servers, p.findServers()...)
	}
	if len(servers) == 0 {
		return nil, untrusted("no net/http server found in %s (idlegap looks for http.Server and http.ListenAndServe), so there is nothing to compare", strings.Join(o.pkgs, " "))
	}
	var routes []*route
	var notes []finding
	if len(o.k8s) > 0 {
		files, err := manifestFiles(o.k8s)
		if err != nil {
			return nil, err
		}
		objs, err := loadManifests(files)
		if err != nil {
			return nil, err
		}
		c := &cluster{objs: objs, override: o.upstreamIdle}
		routes = c.routes()
		notes = c.notes
		if len(routes) == 0 && len(notes) == 0 {
			return nil, untrusted("the manifests in %s have no Ingress or HTTPRoute, so idlegap can't tell which proxy is in front of the servers; pass --upstream-idle instead", strings.Join(o.k8s, " "))
		}
	} else {
		routes = []*route{{label: "--upstream-idle", proxy: &proxy{kind: "proxy", idle: o.upstreamIdle, idleFrom: "--upstream-idle"}}}
	}
	r := &report{servers: servers, findings: notes}
	r.pairs = compare(servers, routes)
	for _, rt := range routes {
		if rt.via == nil || rt.targetPort == 0 {
			continue
		}
		matched := false
		for _, p := range r.pairs {
			if p.rt == rt {
				matched = true
			}
		}
		if !matched {
			r.findings = append(r.findings, finding{code: "IG203", level: levelWarn, file: rt.via.file, line: rt.via.line,
				ctx: rt.label + " (" + rt.proxy.kind + ")",
				msg: fmt.Sprintf("sends to port %d, and no server in the Go code read listens there, so nothing behind it was compared", rt.targetPort),
				fix: "pass the Go package of the service behind it, or check the Service's targetPort"})
		}
	}
	for _, p := range r.pairs {
		r.findings = append(r.findings, p.findings...)
	}
	if checked(r.pairs) == 0 {
		why := "no server could be matched with a proxy"
		if len(r.findings) > 0 {
			why = r.findings[0].msg
		}
		return nil, untrusted("nothing was compared: %s", why)
	}
	sort.SliceStable(r.findings, func(i, j int) bool {
		if r.findings[i].level != r.findings[j].level {
			return r.findings[i].level > r.findings[j].level
		}
		if r.findings[i].file != r.findings[j].file {
			return r.findings[i].file < r.findings[j].file
		}
		return r.findings[i].line < r.findings[j].line
	})
	return r, nil
}

// checked counts comparisons that reached a result.
func checked(pairs []*pair) int {
	n := 0
	for _, p := range pairs {
		if p.result != "not checked" {
			n++
		}
	}
	return n
}

func (r *report) count(l level) int {
	n := 0
	for _, f := range r.findings {
		if f.level == l {
			n++
		}
	}
	return n
}

func (r *report) exitCode(failOn string) int {
	switch failOn {
	case "error":
		if r.count(levelError) > 0 {
			return exitFindings
		}
	case "warn":
		if r.count(levelError)+r.count(levelWarn) > 0 {
			return exitFindings
		}
	}
	return exitOK
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (r *report) writeText(w io.Writer) {
	fmt.Fprintf(w, "idlegap: %s, %s\n\n", plural(len(r.servers), "server", "servers"), plural(len(r.pairs), "comparison", "comparisons"))
	rows := [][]string{{"SERVER", "IDLE TIMEOUT", "PROXY", "PROXY IDLE", "RESULT"}}
	for _, p := range r.pairs {
		srv := p.srv.where()
		if p.port != 0 {
			srv += " :" + strconv.Itoa(p.port)
		}
		pidle := "-"
		if !p.rt.proxy.keepAliveOff {
			pidle = fmtDur(p.rt.proxy.idle)
		}
		rows = append(rows, []string{srv, p.idle.text(), p.rt.label + " (" + p.rt.proxy.kind + ")", pidle, p.result})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, c := range row {
			if i == len(row)-1 {
				b.WriteString(c)
			} else {
				b.WriteString(c + strings.Repeat(" ", widths[i]-len(c)+2))
			}
		}
		fmt.Fprintln(w, b.String())
	}
	for _, f := range r.findings {
		fmt.Fprintf(w, "\n%-5s %s %s:%d  %s\n  %s\n  Fix: %s\n", strings.ToUpper(f.level.String()), f.code, f.file, f.line, f.ctx, f.msg, f.fix)
	}
	fmt.Fprintln(w)
	e, wn := r.count(levelError), r.count(levelWarn)
	switch {
	case e > 0:
		fmt.Fprintf(w, "Result: %s where the server can close an idle connection before the proxy does", plural(e, "pair", "pairs"))
		if wn > 0 {
			fmt.Fprintf(w, ", and %s", plural(wn, "warning", "warnings"))
		}
		fmt.Fprintln(w, ".")
	case wn > 0:
		fmt.Fprintf(w, "Result: %s; part of the setup wasn't compared, so this isn't a clean result.\n", plural(wn, "warning", "warnings"))
	default:
		fmt.Fprintln(w, "Result: in every pair compared, the server keeps idle connections open longer than the proxy.")
	}
	fmt.Fprintln(w, "idlegap reads code and manifests without running them; a value set at run time or by a wrapper can differ.")
}

type jsonFinding struct {
	Code    string `json:"code"`
	Level   string `json:"level"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Context string `json:"context"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

type jsonPair struct {
	Server        string `json:"server"`
	Port          int    `json:"port,omitempty"`
	IdleTimeout   string `json:"idleTimeout"`
	IdleSeconds   *int64 `json:"idleSeconds,omitempty"`
	Proxy         string `json:"proxy"`
	ProxyKind     string `json:"proxyKind"`
	ProxyIdle     string `json:"proxyIdle"`
	ProxyIdleFrom string `json:"proxyIdleFrom"`
	Result        string `json:"result"`
}

func (r *report) writeJSON(w io.Writer, failOn string) error {
	out := struct {
		Version     string        `json:"version"`
		Servers     int           `json:"servers"`
		Comparisons []jsonPair    `json:"comparisons"`
		Findings    []jsonFinding `json:"findings"`
		Errors      int           `json:"errors"`
		Warnings    int           `json:"warnings"`
		Pass        bool          `json:"pass"`
	}{Version: version, Servers: len(r.servers), Comparisons: []jsonPair{}, Findings: []jsonFinding{},
		Errors: r.count(levelError), Warnings: r.count(levelWarn)}
	out.Pass = out.Errors == 0 && out.Warnings == 0
	for _, p := range r.pairs {
		jp := jsonPair{Server: p.srv.where(), Port: p.port, IdleTimeout: p.idle.text(), Proxy: p.rt.label,
			ProxyKind: p.rt.proxy.kind, ProxyIdleFrom: p.rt.proxy.idleFrom, Result: p.result}
		if p.idle.state == "timeout" {
			s := int64(p.idle.d / time.Second)
			jp.IdleSeconds = &s
		}
		if !p.rt.proxy.keepAliveOff {
			jp.ProxyIdle = fmtDur(p.rt.proxy.idle)
		}
		out.Comparisons = append(out.Comparisons, jp)
	}
	for _, f := range r.findings {
		out.Findings = append(out.Findings, jsonFinding{f.code, f.level.String(), f.file, f.line, f.ctx, f.msg, f.fix})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func mainCode(args []string, stdout, stderr io.Writer) int {
	o, done, err := parseArgs(args, stderr)
	if done {
		if o == nil && err == nil {
			for _, a := range args {
				if a == "-version" || a == "--version" {
					fmt.Fprintln(stdout, "idlegap", version)
				}
			}
		}
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "idlegap:", err)
		return exitUsage
	}
	r, err := run(o)
	if err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			fmt.Fprintln(stderr, "idlegap: can't trust the input, so there is no result:", err)
			return exitInput
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "idlegap:", err)
			return exitUsage
		}
		fmt.Fprintln(stderr, "idlegap:", err)
		return exitInput
	}
	if o.format == "json" {
		if err := r.writeJSON(stdout, o.failOn); err != nil {
			fmt.Fprintln(stderr, "idlegap:", err)
			return exitInput
		}
	} else {
		r.writeText(stdout)
	}
	return r.exitCode(o.failOn)
}

func main() {
	os.Exit(mainCode(os.Args[1:], os.Stdout, os.Stderr))
}
