// Package tests builds idlegap and runs it the way users do, on the
// examples and on broken input. It also checks the net/http behavior the
// rules rely on, against a real server.
package tests

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "idlegap-test")
	if err != nil {
		panic(err)
	}
	name := "idlegap"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary = filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", binary, "./plugin/scripts")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic("go build: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run runs idlegap from the repo root.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = ".."
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestExamples(t *testing.T) {
	out, _, code := run(t, "--k8s", "examples/alb/deploy", "./examples/alb/api")
	if code != 1 || !strings.Contains(out, "IG102") || !strings.Contains(out, "90s (ReadTimeout, --read-timeout in the container's args)") {
		t.Errorf("alb: exit %d\n%s", code, out)
	}
	out, _, code = run(t, "--fail-on", "never", "--k8s", "examples/alb/deploy", "./examples/alb/api")
	if code != 0 || !strings.Contains(out, "IG102") {
		t.Errorf("alb --fail-on never: exit %d\n%s", code, out)
	}
	// The healthy example must stay quiet.
	out, _, code = run(t, "--k8s", "examples/nginx/deploy", "./examples/nginx/web")
	if code != 0 || strings.Contains(out, "IG") || !strings.Contains(out, "75s (IdleTimeout)") || strings.Contains(out, ":6060") {
		t.Errorf("nginx: exit %d\n%s", code, out)
	}
}

// broken returns files that must never give a result: empty, binary
// garbage, text garbage, cut off, and the output of a failed command.
func broken(t *testing.T, real []byte, failed string) map[string][]byte {
	junk := make([]byte, 512)
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	junk[0] = 0
	return map[string][]byte{
		"empty":       {},
		"whitespace":  []byte("\n\n  \n"),
		"binary":      junk,
		"text":        []byte("lorem ipsum dolor sit amet\n"),
		"cut-half":    real[:len(real)/2],
		"cut-line":    real[:bytes.LastIndexByte(real[:len(real)*2/3], '\n')+1],
		"failed":      []byte(failed),
		"cut-nolast":  real[:len(real)-1],
		"failed-make": []byte("make: *** [Makefile:12: manifests] Error 1\n"),
	}
}

func TestBrokenManifests(t *testing.T) {
	real, err := os.ReadFile("../examples/alb/deploy/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	failed := "Error: INSTALLATION FAILED: template: api/templates/ingress.yaml:9:20: executing \"api/templates/ingress.yaml\" at <.Values.x>: nil pointer\n"
	for name, data := range broken(t, real, failed) {
		if name == "cut-nolast" {
			continue // ends right before the last newline, checked below
		}
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "app.yaml")
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, extra := range [][]string{nil, {"--fail-on", "never"}, {"--fail-on", "error"}, {"--format", "json"}} {
				args := append(append([]string{}, extra...), "--k8s", p, "./examples/alb/api")
				out, errs, code := run(t, args...)
				if code != 3 || out != "" || !strings.Contains(errs, "can't trust the input") {
					t.Errorf("%v: exit %d, stdout %q, stderr %q", extra, code, out, errs)
				}
			}
		})
	}
	// A manifest cut inside a value can still be valid YAML, so a missing
	// final newline is refused too.
	p := filepath.Join(t.TempDir(), "app.yaml")
	cut := bytes.Replace(real, []byte("timeout_seconds=120\n"), []byte("timeout_seconds=12"), 1)
	cut = cut[:bytes.Index(cut, []byte("timeout_seconds=12"))+len("timeout_seconds=12")]
	if err := os.WriteFile(p, cut, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := run(t, "--k8s", p, "./examples/alb/api"); code != 3 {
		t.Errorf("cut inside a value: exit %d %s", code, errs)
	}
}

func TestBrokenGo(t *testing.T) {
	real, err := os.ReadFile("../examples/alb/api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	failed := "# example.com/api\n./main.go:14:2: undefined: muxx\nFAIL\texample.com/api [build failed]\n"
	for name, data := range broken(t, real, failed) {
		if name == "cut-nolast" {
			continue // a whole Go file without its final newline is valid Go
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, extra := range [][]string{nil, {"--fail-on", "never"}, {"--format", "json"}} {
				args := append(append([]string{}, extra...), "--k8s", "examples/alb/deploy", dir)
				out, errs, code := run(t, args...)
				if code != 3 || out != "" || !strings.Contains(errs, "can't trust the input") {
					t.Errorf("%v: exit %d, stdout %q, stderr %q", extra, code, out, errs)
				}
			}
		})
	}
	// A directory with no Go files, and a Go file named directly.
	dir := t.TempDir()
	if _, _, code := run(t, "--upstream-idle", "60s", dir); code != 3 {
		t.Errorf("empty directory: exit %d", code)
	}
	if _, _, code := run(t, "--upstream-idle", "60s", "examples/alb/api/main.go"); code != 3 {
		t.Errorf("file instead of directory: exit %d", code)
	}
	if _, _, code := run(t, "--upstream-idle", "60s", filepath.Join(dir, "missing")); code != 3 {
		t.Errorf("missing directory: exit %d", code)
	}
}

func TestManifestsWithNothingToCompare(t *testing.T) {
	dir := t.TempDir()
	svc := "apiVersion: v1\nkind: Service\nmetadata:\n  name: api\nspec:\n  ports:\n    - port: 80\n"
	p := filepath.Join(dir, "svc.yaml")
	if err := os.WriteFile(p, []byte(svc), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--fail-on", "never"}} {
		args := append(append([]string{}, extra...), "--k8s", p, "./examples/alb/api")
		if out, errs, code := run(t, args...); code != 3 || out != "" {
			t.Errorf("no Ingress: exit %d %q %q", code, out, errs)
		}
	}
	if _, _, code := run(t, "--k8s", filepath.Join(dir, "*.json"), "./examples/alb/api"); code != 3 {
		t.Errorf("glob with no match: exit %d", code)
	}
	if _, _, code := run(t, "--k8s", t.TempDir(), "./examples/alb/api"); code != 3 {
		t.Errorf("directory with no manifests: exit %d", code)
	}
}

// idleClosed starts srv on a free port, sends one keep-alive request, waits
// for wait, and reports whether the server closed the connection.
func idleClosed(t *testing.T, srv *http.Server, wait time.Duration) bool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	go srv.Serve(ln)
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	get := func() error {
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return err
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := get(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(wait)
	return get() != nil
}

// TestNetHTTPIdleRules checks, on a real net/http server, the rule
// idlegap applies: IdleTimeout if set, else ReadTimeout.
func TestNetHTTPIdleRules(t *testing.T) {
	const short = 200 * time.Millisecond
	if !idleClosed(t, &http.Server{ReadTimeout: short}, 3*short) {
		t.Error("with only ReadTimeout set, the server should close the idle connection after ReadTimeout")
	}
	if !idleClosed(t, &http.Server{IdleTimeout: short, ReadTimeout: time.Minute}, 3*short) {
		t.Error("the server should close the idle connection after IdleTimeout")
	}
	if idleClosed(t, &http.Server{ReadTimeout: short, IdleTimeout: time.Minute}, 3*short) {
		t.Error("IdleTimeout should override ReadTimeout for idle connections")
	}
	if idleClosed(t, &http.Server{}, 3*short) {
		t.Error("with no timeouts, the server should keep the idle connection")
	}
}
