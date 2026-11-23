package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// obj is one Kubernetes object read from a manifest file.
type obj struct {
	file             string
	line             int
	apiVersion, kind string
	name, ns         string
	node             *Node
}

func (o *obj) ref() string {
	if o.ns != "" {
		return o.kind + " " + o.ns + "/" + o.name
	}
	return o.kind + " " + o.name
}

func (o *obj) annotation(k string) string {
	return stringMap(get(o.node, "metadata", "annotations"))[k]
}

func (o *obj) where() string { return fmt.Sprintf("%s:%d", o.file, o.line) }

// get walks mapping keys from n and returns the node, or nil.
func get(n *Node, path ...string) *Node {
	for _, k := range path {
		if n == nil || n.Kind != MappingNode {
			return nil
		}
		var next *Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
			}
		}
		n = next
	}
	return n
}

func str(n *Node, path ...string) string {
	n = get(n, path...)
	if n == nil || n.Kind != ScalarNode {
		return ""
	}
	return n.Value
}

func items(n *Node) []*Node {
	if n == nil || n.Kind != SequenceNode {
		return nil
	}
	return n.Content
}

func stringMap(n *Node) map[string]string {
	m := map[string]string{}
	if n == nil || n.Kind != MappingNode {
		return m
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i+1].Kind == ScalarNode {
			m[n.Content[i].Value] = n.Content[i+1].Value
		}
	}
	return m
}

// manifestFiles expands --k8s arguments: files, directories (their .yaml,
// .yml and .json files, recursively) and globs.
func manifestFiles(args []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, a := range args {
		matches := []string{a}
		if strings.ContainsAny(a, "*?[") {
			m, err := filepath.Glob(a)
			if err != nil {
				return nil, usageErr("bad --k8s pattern %q: %v", a, err)
			}
			if len(m) == 0 {
				return nil, untrusted("--k8s pattern %q matches no file", a)
			}
			matches = m
		}
		for _, p := range matches {
			st, err := os.Stat(p)
			if err != nil {
				return nil, untrusted("manifest %s: %v", p, cleanErr(err))
			}
			if !st.IsDir() {
				add(p)
				continue
			}
			n := len(out)
			err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if path != p && strings.HasPrefix(d.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}
				switch strings.ToLower(filepath.Ext(path)) {
				case ".yaml", ".yml", ".json":
					add(path)
				}
				return nil
			})
			if err != nil {
				return nil, untrusted("reading %s: %v", p, cleanErr(err))
			}
			if len(out) == n {
				return nil, untrusted("directory %s has no .yaml, .yml or .json files", p)
			}
		}
	}
	return out, nil
}

// loadManifests reads every object in the files and refuses anything that
// isn't a complete set of Kubernetes objects.
func loadManifests(files []string) ([]*obj, error) {
	var objs []*obj
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, untrusted("manifest %s: %v", f, cleanErr(err))
		}
		if err := checkText(f, src, "manifest"); err != nil {
			return nil, err
		}
		if why := failedCommand(src); why != "" {
			return nil, untrusted("manifest %s looks like the output of a failed command (%s), not Kubernetes objects", f, why)
		}
		dec := NewDecoder(bytes.NewReader(src))
		n := 0
		for doc := 1; ; doc++ {
			var root Node
			err := dec.Decode(&root)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, untrusted("manifest %s isn't valid YAML, so it may be cut off: %s", f, firstLine(err.Error()))
			}
			body := &root
			if body.Kind == DocumentNode && len(body.Content) == 1 {
				body = body.Content[0]
			}
			if body.Kind == ScalarNode && body.Tag == "!!null" {
				continue // an empty document between --- lines
			}
			if body.Kind == MappingNode && buildConfig(str(body, "apiVersion")) {
				n++ // kustomize or skaffold configuration, not a cluster object
				continue
			}
			got, err := objectsIn(f, doc, body)
			if err != nil {
				return nil, err
			}
			objs = append(objs, got...)
			n += len(got)
		}
		if n == 0 {
			return nil, untrusted("manifest %s has no Kubernetes objects", f)
		}
	}
	return objs, nil
}

func objectsIn(file string, doc int, body *Node) ([]*obj, error) {
	if body.Kind != MappingNode {
		return nil, untrusted("manifest %s, document %d (line %d), isn't a Kubernetes object", file, doc, body.Line)
	}
	kind, api := str(body, "kind"), str(body, "apiVersion")
	if kind == "" || api == "" {
		return nil, untrusted("manifest %s, document %d (line %d), has no kind or apiVersion, so it isn't a Kubernetes object or it is cut off", file, doc, body.Line)
	}
	if buildConfig(api) {
		return nil, nil
	}
	if strings.HasSuffix(kind, "List") && get(body, "items") != nil {
		var out []*obj
		for _, it := range items(get(body, "items")) {
			got, err := objectsIn(file, doc, it)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
		return out, nil
	}
	o := &obj{file: file, line: body.Line, apiVersion: api, kind: kind, node: body,
		name: str(body, "metadata", "name"), ns: str(body, "metadata", "namespace")}
	if o.name == "" {
		return nil, untrusted("manifest %s:%d: %s has no metadata.name, so it may be cut off", file, body.Line, kind)
	}
	if err := checkShape(o); err != nil {
		return nil, err
	}
	return []*obj{o}, nil
}

// checkShape refuses objects missing a part Kubernetes requires, which is
// what a file cut off at the end of a line looks like.
func checkShape(o *obj) error {
	cut := func(what string) error {
		return untrusted("%s at %s has no %s, which Kubernetes requires, so the file may be cut off", o.ref(), o.where(), what)
	}
	switch o.kind {
	case "Ingress":
		if get(o.node, "spec", "defaultBackend") == nil && len(items(get(o.node, "spec", "rules"))) == 0 {
			return cut("spec.rules or spec.defaultBackend")
		}
		for _, r := range items(get(o.node, "spec", "rules")) {
			if get(r, "http") != nil && len(items(get(r, "http", "paths"))) == 0 {
				return cut("paths in a rule")
			}
			for _, p := range items(get(r, "http", "paths")) {
				if get(p, "backend") == nil {
					return cut("backend in a path")
				}
			}
		}
	case "Service":
		if str(o.node, "spec", "type") != "ExternalName" && len(items(get(o.node, "spec", "ports"))) == 0 {
			return cut("spec.ports")
		}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Rollout", "Pod":
		if len(containers(o)) == 0 && get(podSpec(o), "workloadRef") == nil {
			return cut("containers")
		}
	case "HTTPRoute":
		if len(items(get(o.node, "spec", "parentRefs"))) == 0 {
			return cut("spec.parentRefs")
		}
	case "Gateway":
		if str(o.node, "spec", "gatewayClassName") == "" {
			return cut("spec.gatewayClassName")
		}
	}
	return nil
}

// buildConfig reports whether apiVersion belongs to a build tool's own
// configuration file, which sits next to manifests but isn't applied.
func buildConfig(api string) bool {
	return strings.HasPrefix(api, "kustomize.config.k8s.io/") || strings.HasPrefix(api, "skaffold/")
}

var failedOutputs = []string{
	"Error: ", "error: ", "Error from server", "panic: ", "fatal error: ", "FAILED", "make: ***", "unable to recognize",
	"The connection to the server", "no matches for kind", "command not found", "Traceback (most recent call last)",
}

// failedCommand recognizes the first line of common error output, from
// helm, kubectl, kustomize, make or a shell.
func failedCommand(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		for _, p := range failedOutputs {
			if strings.HasPrefix(t, p) {
				return "it starts with " + strconv.Quote(p)
			}
		}
		return ""
	}
	return ""
}

func podSpec(o *obj) *Node {
	if o.kind == "Pod" {
		return get(o.node, "spec")
	}
	return get(o.node, "spec", "template", "spec")
}

func podLabels(o *obj) map[string]string {
	if o.kind == "Pod" {
		return stringMap(get(o.node, "metadata", "labels"))
	}
	return stringMap(get(o.node, "spec", "template", "metadata", "labels"))
}

func containers(o *obj) []*Node {
	return items(get(podSpec(o), "containers"))
}

var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true, "Rollout": true, "Pod": true}

// proxy is a load balancer or reverse proxy that keeps idle connections to
// pods open for idle (zero with keepAliveOff).
type proxy struct {
	kind         string // "AWS ALB", "Google Cloud load balancer", "ingress-nginx", "proxy"
	idle         time.Duration
	idleFrom     string // how idle was found
	keepAliveOff bool
	isDefault    bool // idle is the product default because no setting was read
	unknown      string
}

// route is one path from a proxy to a container port.
type route struct {
	via        *obj // the Ingress or HTTPRoute; nil for --upstream-idle
	label      string
	proxy      *proxy
	svcName    string
	svc        *obj
	targetPort int // 0 when unknown
	portNote   string
	workload   *obj
	container  *Node
}

type cluster struct {
	objs     []*obj
	notes    []finding     // IG202 findings found while reading
	override time.Duration // --upstream-idle, used for proxies idlegap can't read
}

// fallback replaces an unknown proxy with --upstream-idle when it is set.
func (c *cluster) fallback(p *proxy) *proxy {
	if p.unknown == "" || c.override == 0 {
		return p
	}
	return &proxy{kind: "proxy", idle: c.override, idleFrom: "--upstream-idle, because " + p.unknown}
}

func (c *cluster) byKind(kind string) []*obj {
	var out []*obj
	for _, o := range c.objs {
		if o.kind == kind {
			out = append(out, o)
		}
	}
	return out
}

func sameNS(a, b string) bool { return a == "" || b == "" || a == b }

func (c *cluster) find(kind, ns, name string) *obj {
	for _, o := range c.objs {
		if o.kind == kind && o.name == name && sameNS(o.ns, ns) {
			return o
		}
	}
	return nil
}

// routes lists every path from a proxy idlegap knows to a pod port.
func (c *cluster) routes() []*route {
	var out []*route
	for _, ing := range c.byKind("Ingress") {
		p := c.fallback(c.ingressProxy(ing))
		if p.unknown != "" {
			c.notes = append(c.notes, finding{code: "IG202", level: levelWarn, file: ing.file, line: ing.line, ctx: ing.ref(),
				msg: p.unknown + ", so its upstream idle timeout is unknown and nothing behind it was compared",
				fix: "pass the proxy's upstream idle timeout with --upstream-idle"})
			continue
		}
		var backends []*Node
		if b := get(ing.node, "spec", "defaultBackend"); b != nil {
			backends = append(backends, b)
		}
		for _, r := range items(get(ing.node, "spec", "rules")) {
			for _, path := range items(get(r, "http", "paths")) {
				backends = append(backends, get(path, "backend"))
			}
		}
		seen := map[string]bool{}
		for _, b := range backends {
			name := str(b, "service", "name")
			if name == "" {
				continue // a resource backend
			}
			port := str(b, "service", "port", "number")
			if port == "" {
				port = str(b, "service", "port", "name")
			}
			key := name + "|" + port
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c.resolve(ing, p, ing.ns, name, port)...)
		}
	}
	for _, hr := range c.byKind("HTTPRoute") {
		for _, parent := range items(get(hr.node, "spec", "parentRefs")) {
			if k := str(parent, "kind"); k != "" && k != "Gateway" {
				continue
			}
			ns := str(parent, "namespace")
			if ns == "" {
				ns = hr.ns
			}
			gw := c.find("Gateway", ns, str(parent, "name"))
			var p *proxy
			if gw == nil {
				p = &proxy{unknown: "its parent Gateway " + str(parent, "name") + " isn't in the files read"}
			} else {
				p = gatewayProxy(gw)
			}
			p = c.fallback(p)
			if p.unknown != "" {
				c.notes = append(c.notes, finding{code: "IG202", level: levelWarn, file: hr.file, line: hr.line, ctx: hr.ref(),
					msg: p.unknown + ", so its upstream idle timeout is unknown and nothing behind it was compared",
					fix: "add the Gateway's manifest, or pass the proxy's upstream idle timeout with --upstream-idle"})
				continue
			}
			seen := map[string]bool{}
			for _, rule := range items(get(hr.node, "spec", "rules")) {
				for _, br := range items(get(rule, "backendRefs")) {
					if k := str(br, "kind"); k != "" && k != "Service" {
						continue
					}
					bns := str(br, "namespace")
					if bns == "" {
						bns = hr.ns
					}
					name, port := str(br, "name"), str(br, "port")
					if seen[name+"|"+port] {
						continue
					}
					seen[name+"|"+port] = true
					out = append(out, c.resolve(hr, p, bns, name, port)...)
				}
			}
		}
	}
	return out
}

// resolve follows a Service and port to the containers it selects.
func (c *cluster) resolve(via *obj, p *proxy, ns, svcName, port string) []*route {
	label := via.ref()
	base := route{via: via, label: label, proxy: p, svcName: svcName}
	svc := c.find("Service", ns, svcName)
	if svc == nil {
		r := base
		r.portNote = "Service " + svcName + " isn't in the files read, so the pod port is unknown"
		return []*route{&r}
	}
	base.svc = svc
	var sp *Node
	for _, it := range items(get(svc.node, "spec", "ports")) {
		if str(it, "port") == port || (port != "" && str(it, "name") == port) || port == "" && len(items(get(svc.node, "spec", "ports"))) == 1 {
			sp = it
		}
	}
	if sp == nil {
		r := base
		r.portNote = fmt.Sprintf("Service %s has no port %q, so the pod port is unknown", svcName, port)
		return []*route{&r}
	}
	target := str(sp, "targetPort")
	if target == "" {
		target = str(sp, "port")
	}
	sel := stringMap(get(svc.node, "spec", "selector"))
	var workloads []*obj
	if len(sel) > 0 {
		for _, o := range c.objs {
			if !workloadKinds[o.kind] || !sameNS(o.ns, svc.ns) {
				continue
			}
			labels := podLabels(o)
			match := true
			for k, v := range sel {
				if labels[k] != v {
					match = false
				}
			}
			if match {
				workloads = append(workloads, o)
			}
		}
	}
	num, numErr := strconv.Atoi(target)
	var out []*route
	for _, w := range workloads {
		for _, ct := range containers(w) {
			for _, cp := range items(get(ct, "ports")) {
				cnum, _ := strconv.Atoi(str(cp, "containerPort"))
				if (numErr == nil && cnum == num) || (numErr != nil && str(cp, "name") == target) {
					r := base
					r.workload, r.container, r.targetPort = w, ct, cnum
					out = append(out, &r)
				}
			}
		}
		if len(out) == 0 && numErr == nil && len(containers(w)) == 1 {
			r := base
			r.workload, r.container, r.targetPort = w, containers(w)[0], num
			out = append(out, &r)
		}
	}
	if len(out) > 0 {
		return out
	}
	r := base
	if numErr == nil {
		r.targetPort = num
		if len(workloads) == 0 {
			r.portNote = "no workload in the files read matches Service " + svcName + "'s selector, so container args weren't read"
		}
		return []*route{&r}
	}
	r.portNote = fmt.Sprintf("no container in the files read has a port named %q for Service %s, so the pod port is unknown", target, svcName)
	return []*route{&r}
}

// ingressProxy works out which controller serves an Ingress and its
// upstream idle timeout.
func (c *cluster) ingressProxy(ing *obj) *proxy {
	ann := stringMap(get(ing.node, "metadata", "annotations"))
	class := str(ing.node, "spec", "ingressClassName")
	if class == "" {
		class = ann["kubernetes.io/ingress.class"]
	}
	var ic *obj
	if class == "" {
		for _, o := range c.byKind("IngressClass") {
			if str(o.node, "metadata", "annotations", "ingressclass.kubernetes.io/is-default-class") == "true" {
				ic, class = o, o.name
			}
		}
		if ic == nil {
			return &proxy{unknown: "it has no ingress class and no default IngressClass is in the files read"}
		}
	} else {
		ic = c.find("IngressClass", "", class)
	}
	controller := ""
	if ic != nil {
		controller = str(ic.node, "spec", "controller")
	}
	switch {
	case controller == "ingress.k8s.aws/alb" || controller == "" && class == "alb":
		return c.albProxy(ing, ic)
	case controller == "k8s.io/ingress-nginx" || controller == "" && class == "nginx":
		return c.nginxProxy(controller == "")
	case controller == "" && (class == "gce" || class == "gce-internal"):
		return googleProxy("ingress class " + class)
	case controller != "":
		return &proxy{unknown: fmt.Sprintf("its IngressClass %s uses controller %s, which idlegap doesn't know", class, controller)}
	}
	return &proxy{unknown: fmt.Sprintf("its ingress class %q isn't one idlegap knows (alb, nginx, gce, gce-internal) and no IngressClass named %s is in the files read", class, class)}
}

func googleProxy(how string) *proxy {
	return &proxy{kind: "Google Cloud load balancer", idle: 600 * time.Second,
		idleFrom: "fixed backend keepalive timeout, from " + how}
}

func gatewayProxy(gw *obj) *proxy {
	class := str(gw.node, "spec", "gatewayClassName")
	if strings.HasPrefix(class, "gke-l7-") {
		return googleProxy("GatewayClass " + class)
	}
	return &proxy{unknown: fmt.Sprintf("its Gateway %s uses GatewayClass %s, which idlegap doesn't know", gw.name, class)}
}

const albAttrKey = "idle_timeout.timeout_seconds"

func (c *cluster) albProxy(ing *obj, ic *obj) *proxy {
	p := &proxy{kind: "AWS ALB"}
	if ic != nil && str(ic.node, "spec", "parameters", "kind") == "IngressClassParams" {
		name := str(ic.node, "spec", "parameters", "name")
		params := c.find("IngressClassParams", "", name)
		if params == nil {
			return &proxy{unknown: fmt.Sprintf("its IngressClass %s takes parameters from IngressClassParams %s, which isn't in the files read", ic.name, name)}
		}
		if attrs := get(params.node, "spec", "loadBalancerAttributes"); attrs != nil {
			for _, a := range items(attrs) {
				if str(a, "key") == albAttrKey {
					return albIdle(p, str(a, "value"), fmt.Sprintf("%s in IngressClassParams %s (%s)", albAttrKey, name, params.where()))
				}
			}
			if v := ing.annotation("alb.ingress.kubernetes.io/load-balancer-attributes"); strings.Contains(v, albAttrKey) {
				return &proxy{unknown: fmt.Sprintf("its IngressClassParams %s sets loadBalancerAttributes without %s while the Ingress annotation sets it, and which one applies isn't documented", name, albAttrKey)}
			}
			p.idle, p.isDefault = 60*time.Second, true
			p.idleFrom = fmt.Sprintf("AWS default; IngressClassParams %s sets loadBalancerAttributes without %s", name, albAttrKey)
			return p
		}
	}
	group := stringMap(get(ing.node, "metadata", "annotations"))["alb.ingress.kubernetes.io/group.name"]
	members := []*obj{ing}
	if group != "" {
		for _, o := range c.byKind("Ingress") {
			if o != ing && stringMap(get(o.node, "metadata", "annotations"))["alb.ingress.kubernetes.io/group.name"] == group {
				members = append(members, o)
			}
		}
	}
	var found []string
	var where []string
	for _, m := range members {
		v := stringMap(get(m.node, "metadata", "annotations"))["alb.ingress.kubernetes.io/load-balancer-attributes"]
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if ok && strings.TrimSpace(k) == albAttrKey {
				found = append(found, strings.TrimSpace(val))
				where = append(where, m.ref())
			}
		}
	}
	if len(found) == 0 {
		p.idle, p.isDefault = 60*time.Second, true
		p.idleFrom = "AWS default; no " + albAttrKey + " attribute is set"
		if group != "" {
			p.idleFrom += " on any Ingress in group " + group
		}
		return p
	}
	for _, v := range found[1:] {
		if v != found[0] {
			return &proxy{unknown: fmt.Sprintf("the Ingresses in ALB group %s set different %s values (%s), which the controller rejects", group, albAttrKey, strings.Join(found, ", "))}
		}
	}
	return albIdle(p, found[0], albAttrKey+" annotation on "+where[0])
}

func albIdle(p *proxy, v, from string) *proxy {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > 4000 {
		return &proxy{unknown: fmt.Sprintf("its ALB %s value %q isn't a whole number of seconds from 1 to 4000", albAttrKey, v)}
	}
	p.idle = time.Duration(n) * time.Second
	p.idleFrom = from
	return p
}

// nginxProxy reads the ingress-nginx controller ConfigMap.
func (c *cluster) nginxProxy(assumed bool) *proxy {
	p := &proxy{kind: "ingress-nginx"}
	var cm *obj
	for _, o := range c.objs {
		if !workloadKinds[o.kind] {
			continue
		}
		for _, ct := range containers(o) {
			for _, a := range append(items(get(ct, "args")), items(get(ct, "command"))...) {
				if v, ok := strings.CutPrefix(a.Value, "--configmap="); ok {
					ns, name, hasNS := strings.Cut(v, "/")
					if !hasNS {
						ns, name = "", v
					}
					if found := c.find("ConfigMap", ns, name); found != nil {
						cm = found
					}
				}
			}
		}
	}
	if cm == nil {
		for _, o := range c.byKind("ConfigMap") {
			if o.name == "ingress-nginx-controller" || o.name == "nginx-configuration" {
				cm = o
			}
		}
	}
	note := ""
	if assumed {
		note = "; class nginx was taken to mean ingress-nginx (kubernetes/ingress-nginx)"
	}
	if cm == nil {
		p.idle, p.isDefault = 60*time.Second, true
		p.idleFrom = "ingress-nginx default upstream-keepalive-timeout; the controller ConfigMap isn't in the files read" + note
		return p
	}
	data := stringMap(get(cm.node, "data"))
	if v, ok := data["upstream-keepalive-connections"]; ok && strings.TrimSpace(v) == "0" {
		p.keepAliveOff = true
		p.idleFrom = "upstream-keepalive-connections is 0 in ConfigMap " + cm.name + " (" + cm.where() + "), so ingress-nginx doesn't reuse connections" + note
		return p
	}
	v, ok := data["upstream-keepalive-timeout"]
	if !ok {
		p.idle, p.isDefault = 60*time.Second, true
		p.idleFrom = "ingress-nginx default; ConfigMap " + cm.name + " doesn't set upstream-keepalive-timeout" + note
		return p
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return &proxy{unknown: fmt.Sprintf("ConfigMap %s sets upstream-keepalive-timeout to %q, which isn't a whole number of seconds", cm.name, v)}
	}
	p.idle = time.Duration(n) * time.Second
	p.idleFrom = "upstream-keepalive-timeout in ConfigMap " + cm.name + " (" + cm.where() + ")" + note
	return p
}

// containerArgs reads --name=value and --name value pairs from a
// container's command and args.
func containerArgs(ct *Node) map[string]string {
	m := map[string]string{}
	if ct == nil {
		return m
	}
	var toks []string
	for _, n := range append(items(get(ct, "command")), items(get(ct, "args"))...) {
		toks = append(toks, n.Value)
	}
	for i, t := range toks {
		if !strings.HasPrefix(t, "-") || t == "-" || t == "--" {
			continue
		}
		name := strings.TrimLeft(t, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			m[k] = v
		} else if i+1 < len(toks) && !strings.HasPrefix(toks[i+1], "-") {
			if _, set := m[name]; !set {
				m[name] = toks[i+1]
			}
		}
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
