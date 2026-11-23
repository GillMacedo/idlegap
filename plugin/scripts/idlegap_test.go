package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write creates files under a new temporary directory.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, src := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runIn runs idlegap with args, from dir.
func runIn(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	var out, errb bytes.Buffer
	code := mainCode(args, &out, &errb)
	return out.String(), errb.String(), code
}

const goHead = "package main\n\nimport (\n\t\"flag\"\n\t\"net/http\"\n\t\"time\"\n)\n\nvar _ = flag.Parse\nvar _ = time.Second\n\n"

func goSrc(body string) string { return goHead + body + "\n" }

// result runs the Go source against --upstream-idle and returns the JSON.
type jsonOut struct {
	Comparisons []struct {
		IdleTimeout string `json:"idleTimeout"`
		Result      string `json:"result"`
		Port        int    `json:"port"`
		Proxy       string `json:"proxy"`
		ProxyIdle   string `json:"proxyIdle"`
		ProxyFrom   string `json:"proxyIdleFrom"`
	} `json:"comparisons"`
	Findings []struct {
		Code    string `json:"code"`
		Level   string `json:"level"`
		Message string `json:"message"`
	} `json:"findings"`
	Pass bool `json:"pass"`
}

func runJSON(t *testing.T, dir string, args ...string) (jsonOut, int, string) {
	t.Helper()
	out, errs, code := runIn(t, dir, append([]string{"--format", "json"}, args...)...)
	var j jsonOut
	if code == exitOK || code == exitFindings {
		if err := json.Unmarshal([]byte(out), &j); err != nil {
			t.Fatalf("bad JSON: %v\n%s", err, out)
		}
	}
	return j, code, errs
}

func codes(j jsonOut) string {
	var c []string
	for _, f := range j.Findings {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

func TestIdleTimeoutRules(t *testing.T) {
	cases := []struct {
		name, body string
		idle       string
		result     string
		codes      string
	}{
		{"idle shorter", `func main() { s := &http.Server{Addr: ":8080", IdleTimeout: 30 * time.Second}; s.ListenAndServe() }`,
			"30s (IdleTimeout)", "server closes first", "IG101"},
		{"idle equal races", `func main() { s := &http.Server{IdleTimeout: time.Minute}; s.ListenAndServe() }`,
			"60s (IdleTimeout)", "server closes first", "IG101"},
		{"idle longer", `func main() { s := &http.Server{IdleTimeout: 75 * time.Second}; s.ListenAndServe() }`,
			"75s (IdleTimeout)", "ok", ""},
		{"read timeout fallback", `func main() { s := &http.Server{ReadTimeout: 10 * time.Second}; s.ListenAndServe() }`,
			"10s (ReadTimeout)", "server closes first", "IG102"},
		{"idle set beats read", `func main() { s := &http.Server{ReadTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}; s.ListenAndServe() }`,
			"120s (IdleTimeout)", "ok", ""},
		{"no timeouts", `func main() { s := &http.Server{}; s.ListenAndServe() }`, "none", "ok", ""},
		{"helper", `func main() { http.ListenAndServe(":8080", nil) }`, "none", "ok", ""},
		{"keep-alives off", `func main() { s := &http.Server{IdleTimeout: time.Second}; s.SetKeepAlivesEnabled(false); s.ListenAndServe() }`,
			"keep-alives off", "ok", ""},
		{"assignment after literal", `func main() { s := &http.Server{}; s.IdleTimeout = 5 * time.Second; s.ListenAndServe() }`,
			"5s (IdleTimeout)", "server closes first", "IG101"},
		{"var declaration", `func main() { var s http.Server; s.ReadTimeout = 9 * time.Second; s.ListenAndServe() }`,
			"9s (ReadTimeout)", "server closes first", "IG102"},
		{"assigned twice", `func main() { s := &http.Server{}; s.IdleTimeout = time.Second; s.IdleTimeout = time.Hour; s.ListenAndServe() }`,
			"unknown", "not checked", "IG201"},
		{"negative idle", `func main() { s := &http.Server{IdleTimeout: -1}; s.ListenAndServe() }`,
			"unknown", "not checked", "IG201"},
		{"negative read fallback", `func main() { s := &http.Server{ReadTimeout: -1}; s.ListenAndServe() }`,
			"unknown", "not checked", "IG201"},
		{"constant", "const idle = 45 * time.Second\nfunc main() { s := &http.Server{IdleTimeout: idle}; s.ListenAndServe() }",
			"45s (IdleTimeout)", "server closes first", "IG101"},
		{"seconds times int", "const n = 90\nfunc main() { s := &http.Server{IdleTimeout: time.Duration(n) * time.Second}; s.ListenAndServe() }",
			"90s (IdleTimeout)", "ok", ""},
		{"flag default", `func main() { d := flag.Duration("idle", 20*time.Second, ""); s := &http.Server{IdleTimeout: *d}; s.ListenAndServe() }`,
			"20s (IdleTimeout, default of flag -idle)", "server closes first", "IG101"},
		{"flag var", "var idle time.Duration\nfunc main() { flag.DurationVar(&idle, \"idle\", 2*time.Minute, \"\"); s := &http.Server{IdleTimeout: idle}; s.ListenAndServe() }",
			"120s (IdleTimeout, default of flag -idle)", "ok", ""},
		{"from config", "type cfg struct{ Idle time.Duration }\nfunc main() { var c cfg; s := &http.Server{IdleTimeout: c.Idle}; s.ListenAndServe() }",
			"unknown", "not checked", "IG201"},
		{"from env", "func main() { s := &http.Server{Addr: \":\" + os.Getenv(\"PORT\"), IdleTimeout: time.Hour}; s.ListenAndServe() }",
			"3600s (IdleTimeout)", "ok", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := goSrc(c.body)
			if strings.Contains(c.body, "os.") {
				src = strings.Replace(src, "\"flag\"", "\"flag\"\n\t\"os\"", 1)
			}
			dir := writeFiles(t, map[string]string{"main.go": src})
			j, code, errs := runJSON(t, dir, "--upstream-idle", "60s", ".")
			if c.result == "not checked" {
				// The only comparison couldn't be made, so there is no result.
				if code != exitInput || !strings.Contains(errs, "not checked") || j.Comparisons != nil {
					t.Fatalf("exit %d: %s", code, errs)
				}
				return
			}
			if code != exitOK && code != exitFindings {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if len(j.Comparisons) != 1 {
				t.Fatalf("want 1 comparison, got %+v", j.Comparisons)
			}
			if got := j.Comparisons[0]; got.IdleTimeout != c.idle || got.Result != c.result {
				t.Errorf("got %q, %q; want %q, %q", got.IdleTimeout, got.Result, c.idle, c.result)
			}
			if got := codes(j); got != c.codes {
				t.Errorf("findings %q, want %q", got, c.codes)
			}
			wantCode := exitOK
			if c.codes != "" {
				wantCode = exitFindings
			}
			if code != wantCode {
				t.Errorf("exit %d, want %d", code, wantCode)
			}
		})
	}
}

func TestSuggest(t *testing.T) {
	for in, want := range map[string]string{"60s": "65s", "600s": "620s", "3000s": "3100s", "10s": "15s"} {
		d, _ := parseSeconds(in)
		if got := fmtDur(suggest(d)); got != want {
			t.Errorf("suggest(%s) = %s, want %s", in, got, want)
		}
	}
}

const albDeploy = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api
  annotations:
    alb.ingress.kubernetes.io/load-balancer-attributes: deletion_protection.enabled=true,idle_timeout.timeout_seconds=120
spec:
  ingressClassName: alb
  rules:
    - http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: api
                port:
                  number: 80
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  selector:
    app: api
  ports:
    - port: 80
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
      containers:
        - name: api
          image: example.com/api
          args: ["--idle", "3m"]
          ports:
            - name: http
              containerPort: 8080
`

const flagServer = `func main() {
	idle := flag.Duration("idle", 30*time.Second, "")
	flag.Parse()
	s := &http.Server{Addr: ":8080", IdleTimeout: *idle}
	s.ListenAndServe()
	d := &http.Server{Addr: ":9090", IdleTimeout: time.Second}
	d.ListenAndServe()
}`

func TestALBWithContainerArgs(t *testing.T) {
	dir := writeFiles(t, map[string]string{"svc/main.go": goSrc(flagServer), "deploy/app.yaml": albDeploy})
	j, code, errs := runJSON(t, dir, "--k8s", "deploy", "./svc")
	if code != exitOK {
		t.Fatalf("exit %d: %s %+v", code, errs, j)
	}
	if len(j.Comparisons) != 1 {
		t.Fatalf("the :9090 server isn't behind the Ingress and must be skipped: %+v", j.Comparisons)
	}
	c := j.Comparisons[0]
	if c.IdleTimeout != "180s (IdleTimeout, --idle in the container's args)" || c.ProxyIdle != "120s" || c.Port != 8080 || c.Result != "ok" {
		t.Errorf("got %+v", c)
	}
}

func TestALBDefaultAndShorter(t *testing.T) {
	deploy := strings.Replace(albDeploy, "deletion_protection.enabled=true,idle_timeout.timeout_seconds=120", "deletion_protection.enabled=true", 1)
	deploy = strings.Replace(deploy, `args: ["--idle", "3m"]`, `args: ["--idle=45s"]`, 1)
	dir := writeFiles(t, map[string]string{"svc/main.go": goSrc(flagServer), "deploy/app.yaml": deploy})
	j, code, _ := runJSON(t, dir, "--k8s", "deploy/app.yaml", "./svc")
	if code != exitFindings || codes(j) != "IG101" {
		t.Fatalf("exit %d, findings %s", code, codes(j))
	}
	if !strings.Contains(j.Comparisons[0].ProxyFrom, "AWS default") || j.Comparisons[0].ProxyIdle != "60s" {
		t.Errorf("got %+v", j.Comparisons[0])
	}
	// --fail-on never reports the finding but exits 0.
	_, code, _ = runJSON(t, dir, "--fail-on", "never", "--k8s", "deploy/app.yaml", "./svc")
	if code != exitOK {
		t.Errorf("--fail-on never: exit %d", code)
	}
}

func TestALBGroupConflict(t *testing.T) {
	other := `---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: other
  annotations:
    alb.ingress.kubernetes.io/group.name: shared
    alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=300
spec:
  ingressClassName: alb
  defaultBackend:
    service:
      name: api
      port:
        number: 80
`
	deploy := strings.Replace(albDeploy, "  annotations:\n", "  annotations:\n    alb.ingress.kubernetes.io/group.name: shared\n", 1) + other
	dir := writeFiles(t, map[string]string{"svc/main.go": goSrc(flagServer), "deploy/app.yaml": deploy})
	_, code, errs := runJSON(t, dir, "--k8s", "deploy", "./svc")
	if code != exitInput || !strings.Contains(errs, "set different idle_timeout.timeout_seconds") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestALBIngressClassParams(t *testing.T) {
	deploy := strings.Replace(albDeploy, "ingressClassName: alb", "ingressClassName: shared-alb", 1) + `---
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata:
  name: shared-alb
spec:
  controller: ingress.k8s.aws/alb
  parameters:
    apiGroup: elbv2.k8s.aws
    kind: IngressClassParams
    name: shared
---
apiVersion: elbv2.k8s.aws/v1beta1
kind: IngressClassParams
metadata:
  name: shared
spec:
  loadBalancerAttributes:
    - key: idle_timeout.timeout_seconds
      value: "240"
`
	dir := writeFiles(t, map[string]string{"svc/main.go": goSrc(flagServer), "deploy/app.yaml": deploy})
	j, code, errs := runJSON(t, dir, "--k8s", "deploy", "./svc")
	if code != exitFindings || codes(j) != "IG101" || j.Comparisons[0].ProxyIdle != "240s" {
		t.Fatalf("exit %d %s: %+v", code, errs, j)
	}
}

const nginxDeploy = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
spec:
  ingressClassName: nginx
  rules:
    - host: example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: web
                port:
                  name: web
---
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  selector:
    app: web
  ports:
    - name: web
      port: 80
      targetPort: 8080
`

func TestNginx(t *testing.T) {
	server := goSrc(`func main() { s := &http.Server{Addr: ":8080", IdleTimeout: 90 * time.Second}; s.ListenAndServe() }`)
	cm := func(data string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ingress-nginx-controller\n  namespace: ingress-nginx\ndata:\n" + data
	}
	cases := []struct {
		name, extra, result, proxyIdle string
		code                           int
	}{
		{"default", "", "ok", "60s", exitOK},
		{"configmap longer", cm("  upstream-keepalive-timeout: \"120\"\n"), "server closes first", "120s", exitFindings},
		{"configmap shorter", cm("  upstream-keepalive-timeout: \"30\"\n"), "ok", "30s", exitOK},
		{"keepalive off", cm("  upstream-keepalive-connections: \"0\"\n  upstream-keepalive-timeout: \"300\"\n"), "proxy doesn't reuse", "", exitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"web/main.go": server, "deploy/web.yaml": nginxDeploy}
			if c.extra != "" {
				files["deploy/cm.yaml"] = c.extra
			}
			dir := writeFiles(t, files)
			j, code, errs := runJSON(t, dir, "--k8s", "deploy", "./web")
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, errs)
			}
			got := j.Comparisons[0]
			if got.Result != c.result || got.ProxyIdle != c.proxyIdle {
				t.Errorf("got %+v", got)
			}
		})
	}
	t.Run("bad configmap value", func(t *testing.T) {
		dir := writeFiles(t, map[string]string{"web/main.go": server, "deploy/web.yaml": nginxDeploy, "deploy/cm.yaml": cm("  upstream-keepalive-timeout: 60s\n")})
		_, code, errs := runJSON(t, dir, "--k8s", "deploy", "./web")
		if code != exitInput || !strings.Contains(errs, "isn't a whole number") {
			t.Fatalf("exit %d: %s", code, errs)
		}
	})
}

func TestGKEGateway(t *testing.T) {
	manifests := `apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: external
spec:
  gatewayClassName: gke-l7-global-external-managed
  listeners:
    - name: http
      protocol: HTTP
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: store
spec:
  parentRefs:
    - name: external
  rules:
    - backendRefs:
        - name: store
          port: 8080
`
	for idle, want := range map[string]string{"590 * time.Second": "server closes first", "600 * time.Second": "server closes first", "620 * time.Second": "ok"} {
		dir := writeFiles(t, map[string]string{"store/main.go": goSrc(`func main() { s := &http.Server{IdleTimeout: ` + idle + `}; s.ListenAndServe() }`), "deploy/gw.yaml": manifests})
		j, _, errs := runJSON(t, dir, "--k8s", "deploy", "./store")
		if len(j.Comparisons) != 1 || j.Comparisons[0].Result != want || j.Comparisons[0].ProxyIdle != "600s" {
			t.Errorf("%s: %s %+v", idle, errs, j)
		}
	}
}

func TestUnknownProxy(t *testing.T) {
	ing := strings.Replace(nginxDeploy, "ingressClassName: nginx", "ingressClassName: traefik", 1)
	server := goSrc(`func main() { s := &http.Server{Addr: ":8080", IdleTimeout: 30 * time.Second}; s.ListenAndServe() }`)
	dir := writeFiles(t, map[string]string{"web/main.go": server, "deploy/web.yaml": ing})
	_, code, errs := runJSON(t, dir, "--k8s", "deploy", "./web")
	if code != exitInput || !strings.Contains(errs, "nothing was compared") {
		t.Fatalf("an unknown proxy must not pass: exit %d %s", code, errs)
	}
	j, code, _ := runJSON(t, dir, "--k8s", "deploy", "--upstream-idle", "90", "./web")
	if code != exitFindings || codes(j) != "IG101" || j.Comparisons[0].ProxyIdle != "90s" {
		t.Fatalf("--upstream-idle fills in: exit %d %+v", code, j)
	}
}

func TestPortMismatchWarns(t *testing.T) {
	server := goSrc(`func main() { s := &http.Server{Addr: ":9000", IdleTimeout: 30 * time.Second}; s.ListenAndServe() }`)
	dir := writeFiles(t, map[string]string{"web/main.go": server, "deploy/web.yaml": nginxDeploy})
	_, code, errs := runJSON(t, dir, "--k8s", "deploy", "./web")
	if code != exitInput || !strings.Contains(errs, "port 8080") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestMissingWorkloadWarnsOnFlagDefault(t *testing.T) {
	// The Service is there but no Deployment, so container args are unknown.
	server := goSrc(`func main() { d := flag.Duration("idle", 2*time.Minute, ""); s := &http.Server{Addr: ":8080", IdleTimeout: *d}; s.ListenAndServe() }`)
	dir := writeFiles(t, map[string]string{"web/main.go": server, "deploy/web.yaml": nginxDeploy})
	j, code, _ := runJSON(t, dir, "--k8s", "deploy", "./web")
	if code != exitFindings || !strings.Contains(codes(j), "IG302") || j.Pass {
		t.Fatalf("exit %d %+v", code, j)
	}
	_, code, _ = runJSON(t, dir, "--fail-on", "error", "--k8s", "deploy", "./web")
	if code != exitOK {
		t.Fatalf("--fail-on error: exit %d", code)
	}
}

func TestKustomizationSkipped(t *testing.T) {
	server := goSrc(`func main() { s := &http.Server{Addr: ":8080", IdleTimeout: 90 * time.Second}; s.ListenAndServe() }`)
	k := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - web.yaml\n"
	dir := writeFiles(t, map[string]string{"web/main.go": server, "deploy/web.yaml": nginxDeploy, "deploy/kustomization.yaml": k})
	_, code, errs := runJSON(t, dir, "--k8s", "deploy", "./web")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestUsage(t *testing.T) {
	dir := writeFiles(t, map[string]string{"main.go": goSrc(`func main() { http.ListenAndServe(":1", nil) }`)})
	for _, args := range [][]string{
		{"."},
		{"--upstream-idle", "60s"},
		{"--upstream-idle", "soon", "."},
		{"--upstream-idle", "0", "."},
		{"--format", "xml", "--upstream-idle", "60s", "."},
		{"--fail-on", "maybe", "--upstream-idle", "60s", "."},
		{"--nope"},
	} {
		if _, _, code := runIn(t, dir, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	if out, _, code := runIn(t, dir, "--version"); code != 0 || out != "idlegap 0.1.0\n" {
		t.Errorf("--version: %d %q", code, out)
	}
}

func TestAssignmentElsewhereIsNotChecked(t *testing.T) {
	src := goSrc(`func newServer() *http.Server {
	return &http.Server{Addr: ":8080", IdleTimeout: 90 * time.Second}
}

func main() {
	s := newServer()
	s.IdleTimeout = 10 * time.Second
	s.ListenAndServe()
}`)
	dir := writeFiles(t, map[string]string{"main.go": src})
	_, code, errs := runJSON(t, dir, "--upstream-idle", "60s", ".")
	if code != exitInput || !strings.Contains(errs, "s.IdleTimeout is assigned at") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// An assignment tied to the server in the same function is fine.
	src = goSrc(`func main() {
	s := &http.Server{Addr: ":8080"}
	s.IdleTimeout = 90 * time.Second
	s.ListenAndServe()
}`)
	dir = writeFiles(t, map[string]string{"main.go": src})
	if _, code, errs := runJSON(t, dir, "--upstream-idle", "60s", "."); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
