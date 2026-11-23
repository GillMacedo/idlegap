package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type level int

const (
	levelNote level = iota
	levelWarn
	levelError
)

func (l level) String() string {
	switch l {
	case levelError:
		return "error"
	case levelWarn:
		return "warn"
	}
	return "note"
}

type finding struct {
	code  string
	level level
	file  string
	line  int
	ctx   string
	msg   string
	fix   string
}

// idleResult is a server's idle timeout as Go computes it.
type idleResult struct {
	state    string // "timeout", "none", "off", "unknown"
	d        time.Duration
	field    string // "IdleTimeout" or "ReadTimeout"
	from     string // flag or args note
	reason   string // why it is unknown
	fallback bool   // IdleTimeout unset, ReadTimeout used
}

func (r idleResult) text() string {
	switch r.state {
	case "timeout":
		s := fmtDur(r.d) + " (" + r.field
		if r.from != "" {
			s += ", " + r.from
		}
		return s + ")"
	case "none":
		return "none"
	case "off":
		return "keep-alives off"
	}
	return "unknown"
}

// serverIdle applies net/http's rule: IdleTimeout if non-zero, else
// ReadTimeout; zero or negative means no timeout.
func serverIdle(s *server, env *evalEnv) idleResult {
	if s.helper != "" {
		return idleResult{state: "none"}
	}
	if s.keepAlivesOff {
		return idleResult{state: "off"}
	}
	if s.stray != "" {
		return idleResult{state: "unknown", reason: s.stray}
	}
	if s.conflict == "IdleTimeout" || s.conflict == "ReadTimeout" {
		return idleResult{state: "unknown", reason: s.conflict + " is set in more than one place"}
	}
	if s.idle != nil {
		v, err := s.eval(s.idle, env)
		if err != nil {
			return idleResult{state: "unknown", reason: "IdleTimeout " + err.Error()}
		}
		if v.isS {
			return idleResult{state: "unknown", reason: "IdleTimeout isn't a number"}
		}
		if v.num < 0 {
			return idleResult{state: "unknown", reason: negNote("IdleTimeout")}
		}
		if v.num > 0 {
			return idleResult{state: "timeout", d: time.Duration(v.num), field: "IdleTimeout", from: v.from}
		}
	}
	if s.read == nil {
		return idleResult{state: "none"}
	}
	v, err := s.eval(s.read, env)
	if err != nil {
		return idleResult{state: "unknown", reason: "IdleTimeout isn't set, so ReadTimeout is the idle timeout, and ReadTimeout " + err.Error()}
	}
	if v.isS {
		return idleResult{state: "unknown", reason: "ReadTimeout isn't a number"}
	}
	if v.num < 0 {
		return idleResult{state: "unknown", reason: "IdleTimeout isn't set, so ReadTimeout is the idle timeout, and " + negNote("ReadTimeout")}
	}
	if v.num == 0 {
		return idleResult{state: "none"}
	}
	return idleResult{state: "timeout", d: time.Duration(v.num), field: "ReadTimeout", from: v.from, fallback: true}
}

// negNote explains a negative timeout, which net/http treats differently
// by version.
func negNote(field string) string {
	return field + " is negative: from Go 1.23 that means no idle timeout, and in Go 1.22 and older the server closes each connection right after its response"
}

// serverPort returns the TCP port a server listens on, or 0 with a reason.
func serverPort(s *server, env *evalEnv) (int, string, string) {
	e := s.addr
	if s.helper != "" {
		e = s.addrArg
		if e == nil {
			return 0, "", s.helper + " takes a listener, so the port isn't in the code"
		}
	}
	if e == nil {
		return 0, "", "Addr isn't set, so the port depends on the listener passed to Serve"
	}
	if s.conflict == "Addr" {
		return 0, "", "Addr is set in more than one place"
	}
	v, err := s.eval(e, env)
	if err != nil {
		return 0, "", "the port can't be read from Addr: " + err.Error()
	}
	if !v.isS {
		return 0, "", "Addr isn't a string"
	}
	addr := v.str
	if addr == "" {
		return 80, v.from, ""
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0, "", fmt.Sprintf("Addr %q has no port", addr)
	}
	port := addr[i+1:]
	switch port {
	case "http", "":
		return 80, v.from, ""
	case "https":
		return 443, v.from, ""
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, "", fmt.Sprintf("Addr %q has a port idlegap can't read", addr)
	}
	return n, v.from, ""
}

// pair is one server compared with one proxy route.
type pair struct {
	srv      *server
	rt       *route
	port     int
	portWhy  string
	idle     idleResult
	result   string // "server closes first", "ok", "not checked", "proxy doesn't reuse"
	findings []finding
}

// fmtDur prints whole seconds up to an hour as seconds, the unit proxies
// use, and anything else as Go prints it.
func fmtDur(d time.Duration) string {
	if d%time.Second == 0 && d <= time.Hour {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// suggest returns an idle timeout comfortably above the proxy's: 5
// seconds or a thirtieth more, whichever is larger. For Google's 600
// seconds this gives the 620 seconds Google suggests.
func suggest(proxy time.Duration) time.Duration {
	extra := proxy / 30
	if extra < 5*time.Second {
		extra = 5 * time.Second
	}
	return (proxy + extra).Round(time.Second)
}

func (s *server) where() string { return fmt.Sprintf("%s:%d", s.pos.Filename, s.pos.Line) }

func compare(servers []*server, routes []*route) []*pair {
	var out []*pair
	for _, rt := range routes {
		env := &evalEnv{args: containerArgs(rt.container)}
		for _, s := range servers {
			port, portFrom, why := serverPort(s, env)
			if rt.via != nil && port != 0 && rt.targetPort != 0 && port != rt.targetPort {
				continue
			}
			p := &pair{srv: s, rt: rt, port: port, portWhy: why}
			_ = portFrom
			p.idle = serverIdle(s, env)
			p.judge()
			out = append(out, p)
		}
	}
	return out
}

func (p *pair) ctx() string {
	s := "server"
	if p.port != 0 {
		s += " :" + strconv.Itoa(p.port)
	}
	return s + ", behind " + p.rt.label + " (" + p.rt.proxy.kind + ")"
}

func (p *pair) judge() {
	s, rt, px := p.srv, p.rt, p.rt.proxy
	add := func(f finding) {
		f.file, f.line, f.ctx = s.pos.Filename, s.pos.Line, p.ctx()
		p.findings = append(p.findings, f)
	}
	if rt.via != nil {
		switch {
		case p.port == 0 && rt.targetPort == 0:
			add(finding{code: "IG301", level: levelNote, msg: fmt.Sprintf("compared on the assumption that this server is the one behind %s: %s, and %s", rt.label, p.portWhy, rt.portNote),
				fix: "check that this server is the one the proxy sends to"})
		case p.port == 0:
			add(finding{code: "IG301", level: levelNote, msg: fmt.Sprintf("compared on the assumption that this server listens on port %d, which %s sends to: %s", rt.targetPort, rt.label, p.portWhy),
				fix: "check that this server listens on the port the proxy sends to"})
		case rt.targetPort == 0:
			add(finding{code: "IG301", level: levelNote, msg: fmt.Sprintf("compared on the assumption that %s sends to this server: %s", rt.label, rt.portNote),
				fix: "add the Service and workload manifests so idlegap can match ports"})
		}
	}
	if rt.via != nil && rt.container == nil && strings.HasPrefix(p.idle.from, "default of flag") {
		add(finding{code: "IG302", level: levelWarn, msg: fmt.Sprintf("the idle timeout is the %s, and the container that runs this server isn't in the files read, so its args may set another value", p.idle.from),
			fix: "add the Deployment or other workload manifest the Service selects"})
	}
	if px.keepAliveOff {
		p.result = "proxy doesn't reuse"
		return
	}
	switch p.idle.state {
	case "none":
		p.result = "ok"
		return
	case "off":
		p.result = "ok"
		return
	case "unknown":
		p.result = "not checked"
		add(finding{code: "IG201", level: levelWarn, msg: "not checked: " + p.idle.reason,
			fix: fmt.Sprintf("make sure the server's idle timeout is longer than the %s idle timeout of %s, or set it in code where idlegap can read it", px.kind, fmtDur(px.idle))})
		return
	}
	if p.idle.d > px.idle {
		p.result = "ok"
		return
	}
	p.result = "server closes first"
	from := ""
	if p.idle.from != "" {
		from = " (" + p.idle.from + ")"
	}
	who := px.kind
	if who == "proxy" {
		who = "The proxy"
	}
	risk := fmt.Sprintf("%s keeps idle connections to the server open for %s (%s). It can send a request on a connection the server is closing at that moment, and that request can fail with a 502 that never reaches a handler.", who, fmtDur(px.idle), px.idleFrom)
	if p.idle.d == px.idle {
		risk = "Both sides use the same idle timeout, so they race to close the connection. " + risk
	}
	want := suggest(px.idle)
	if p.idle.fallback {
		add(finding{code: "IG102", level: levelError,
			msg: fmt.Sprintf("IdleTimeout isn't set, so net/http closes idle keep-alive connections after ReadTimeout, %s%s. %s", fmtDur(p.idle.d), from, risk),
			fix: fmt.Sprintf("set IdleTimeout explicitly to more than %s, for example %s", fmtDur(px.idle), fmtDur(want))})
		return
	}
	add(finding{code: "IG101", level: levelError,
		msg: fmt.Sprintf("the server closes idle keep-alive connections after IdleTimeout, %s%s. %s", fmtDur(p.idle.d), from, risk),
		fix: fmt.Sprintf("set IdleTimeout to more than %s, for example %s", fmtDur(px.idle), fmtDur(want))})
}
