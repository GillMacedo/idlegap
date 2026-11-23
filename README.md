# idlegap: find Go servers that cause intermittent 502s behind a load balancer

[![CI](https://github.com/GillMacedo/idlegap/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/idlegap/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for Go services on Kubernetes that checks whether each `http.Server` closes idle keep-alive connections before the AWS ALB, ingress-nginx or Google Cloud load balancer in front of it.**

When the server's idle timeout is shorter than the proxy's, the proxy can send a request on a connection at the moment the server closes it. That request fails with a 502 that never reaches a handler, so your logs show nothing. AWS and Google both document this cause of 502s. The trap in Go: if `IdleTimeout` isn't set, net/http uses `ReadTimeout` as the idle timeout, so a server with `ReadTimeout: 30 * time.Second` behind an ALB at 60 seconds closes first.

`idlegap` reads your Go source and your manifests, follows each Ingress or HTTPRoute through its Service to the container, reads the flag values in the container's `args`, and compares the two timeouts. It never builds or runs your code and never contacts a cluster.

## What you can ask Claude

- "We get random 502s from the ALB with nothing in our logs. Why?"
- "What should IdleTimeout be for our Go API behind ingress-nginx?"
- "Check our Go service's timeouts against our Kubernetes ingress."
- "Is it safe to raise the ALB idle timeout to 300 seconds?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/idlegap
    claude plugin install idlegap@idlegap-marketplace

The skill builds the tool from source with Go 1.22 or newer. It has no dependencies. To run it yourself, clone the repo and use `go run ./plugin/scripts`, or `sh plugin/scripts/run.sh` from any folder.

## Example

`examples/alb` is a small service behind an ALB whose idle timeout is set to 120 seconds. Its `http.Server` sets `ReadTimeout` from a flag and no `IdleTimeout`, and the Deployment passes `--read-timeout=90s`:

    $ go run ./plugin/scripts --fail-on never --k8s examples/alb/deploy ./examples/alb/api
    idlegap: 1 server, 1 comparison

    SERVER                             IDLE TIMEOUT                                               PROXY                       PROXY IDLE  RESULT
    examples/alb/api/main.go:22 :8080  90s (ReadTimeout, --read-timeout in the container's args)  Ingress shop/api (AWS ALB)  120s        server closes first

    ERROR IG102 examples/alb/api/main.go:22  server :8080, behind Ingress shop/api (AWS ALB)
      IdleTimeout isn't set, so net/http closes idle keep-alive connections after ReadTimeout, 90s (--read-timeout in the container's args). AWS ALB keeps idle connections to the server open for 120s (idle_timeout.timeout_seconds annotation on Ingress shop/api). It can send a request on a connection the server is closing at that moment, and that request can fail with a 502 that never reaches a handler.
      Fix: set IdleTimeout explicitly to more than 120s, for example 125s

    Result: 1 pair where the server can close an idle connection before the proxy does.
    idlegap reads code and manifests without running them; a value set at run time or by a wrapper can differ.

`--fail-on never` is here only so the README's example exits 0. Without it, findings exit 1.

`examples/nginx` sets `IdleTimeout` to 75 seconds behind ingress-nginx at 60, and has a debug server on port 6060 that isn't behind the Ingress, so it isn't compared:

    $ go run ./plugin/scripts --k8s examples/nginx/deploy ./examples/nginx/web
    idlegap: 2 servers, 1 comparison

    SERVER                               IDLE TIMEOUT       PROXY                             PROXY IDLE  RESULT
    examples/nginx/web/main.go:24 :8080  75s (IdleTimeout)  Ingress site/web (ingress-nginx)  60s         ok

    Result: in every pair compared, the server keeps idle connections open longer than the proxy.
    idlegap reads code and manifests without running them; a value set at run time or by a wrapper can differ.

Without manifests, give the proxy's idle timeout yourself: `--upstream-idle 60s ./...`.

## Usage

    idlegap [--k8s PATH]... [--upstream-idle DURATION] [--format text|json] [--fail-on warn|error|never] GO_PACKAGE_DIR...

- `--k8s` takes manifest files, folders or globs and can repeat. For Helm, render first: `helm template my-release ./chart > rendered.yaml`. kustomization and skaffold files in a folder are skipped.
- `--upstream-idle` is used when there are no manifests, and for any Ingress whose proxy idlegap doesn't know.
- A package folder ending in `/...` includes subfolders, skipping `vendor`, `testdata` and hidden folders. Test files are skipped.

| Exit code | Meaning |
|---|---|
| 0 | Every compared pair is fine, or `--fail-on never` |
| 1 | Findings at the `--fail-on` level: `warn` by default, so "not checked" fails too |
| 2 | Bad options |
| 3 | The input can't be trusted, or nothing could be compared. No result is printed, whatever `--fail-on` says |

Exit 3 covers empty files, binary or random text, Go that doesn't parse, YAML that doesn't parse, manifests cut off (missing required fields, or no final newline), files that start like an error from helm, kubectl, kustomize or make, folders with no Go server, and manifests with no Ingress or HTTPRoute. `go run` turns any failing exit code into 1, so build the binary when you need the exact code.

## Findings

| Code | Level | Meaning |
|---|---|---|
| IG101 | error | `IdleTimeout` is shorter than or equal to the proxy's idle timeout |
| IG102 | error | `IdleTimeout` isn't set, and `ReadTimeout`, which net/http uses instead, is shorter than or equal to the proxy's |
| IG201 | warn | The server's timeout comes from something idlegap can't follow, such as a config struct or the environment |
| IG202 | warn | The proxy isn't one idlegap knows, so nothing behind it was compared |
| IG203 | warn | The proxy sends to a port no server in the Go code listens on |
| IG301 | note | A server and a route were matched on an assumption, such as an unknown port |
| IG302 | warn | A flag default was used, and the container whose args could change it isn't in the manifests |

## Proxies it reads

| Proxy | Where the idle timeout comes from | Default |
|---|---|---|
| AWS ALB (`alb` class or `ingress.k8s.aws/alb` controller) | `idle_timeout.timeout_seconds` in the `load-balancer-attributes` annotation, across an Ingress group, or in `IngressClassParams` | 60s |
| ingress-nginx (`nginx` class or `k8s.io/ingress-nginx` controller) | `upstream-keepalive-timeout` in the controller ConfigMap. `upstream-keepalive-connections: "0"` means it doesn't reuse connections | 60s |
| Google Cloud load balancers (`gce`, `gce-internal`, `gke-l7-*` Gateway classes) | Fixed by Google | 600s |
| Anything else | `--upstream-idle` | none |

The suggested fix is the proxy's timeout plus 5 seconds or a thirtieth, whichever is more. For Google's 600 seconds that gives the 620 seconds Google recommends.

## How it compares

[golangci-lint](https://github.com/golangci/golangci-lint) with `gosec` flags a server with no `ReadHeaderTimeout`, and [kube-linter](https://github.com/stackrox/kube-linter) checks manifests on their own. Neither compares a value in the code with one in the manifests, which is where this bug lives. Ingress controller docs and AWS and Google troubleshooting pages explain the rule, and idlegap checks it.

## Limits

- **It reads code shape, not types.** It follows literals, constants, arithmetic, `time` units and `flag` or `pflag` values. A timeout from a config struct, Viper or the environment is reported as not checked.
- **A timeout set in another function is not tied to a server.** If the package assigns `IdleTimeout` or `ReadTimeout` anywhere idlegap can't tie to a server, such as after a constructor returns it, the servers in that package are reported as not checked.
- **Only `net/http`.** Frameworks that build the server for you, such as gin's `Run` or echo's `Start`, aren't found yet.
- **One service per run is best.** All servers in the packages you pass are compared with every route on the same port, so in a monorepo two services on port 8080 can be matched with each other's Ingress.
- **Container `env` isn't read.** If your code reads a timeout from an environment variable that overrides a flag, the flag value shown can be wrong.
- **A negative timeout is reported as not checked,** because Go 1.23 and later treat it as no timeout and Go 1.22 closes the connection after each response.
- **Other hops aren't checked,** such as a service mesh sidecar between the proxy and your server, or an NLB.
- **Manifests must end with a newline.** A file that doesn't is refused, because it may be cut off. Add the newline if the file is complete.

## FAQ

**Why does my ALB return 502 errors randomly when my Go service logs nothing?** One common cause is that the Go server closes idle keep-alive connections before the ALB does. Set `IdleTimeout` above the ALB's idle timeout, which is 60 seconds unless you changed it.

**What happens if I don't set IdleTimeout in Go?** net/http uses `ReadTimeout` as the idle timeout. If both are zero, idle connections never time out on the server side.

**What IdleTimeout should I use behind ingress-nginx?** More than `upstream-keepalive-timeout`, which is 60 seconds by default. idlegap reads the controller ConfigMap if you pass it.

**What about GKE?** Google Cloud load balancers keep backend connections for 600 seconds and you can't change that, so the server needs more than 600 seconds. Google suggests 620.

**Is it an MCP server?** No. It's a skill with a Go package and a script that builds it, so Go is all it needs.

## Tested on

- Unit tests for each finding, each proxy and its default, flag values from container args, and port matching. Integration tests build the binary and run it on both examples, and on broken input on each side with the other side real: empty, whitespace, binary, random text, cut in half, cut at a line, cut inside a value, failed helm and make output, and a failed Go build. Each one must exit 3 with no result, with and without `--fail-on never`.
- An integration test starts a real net/http server and checks the rule idlegap applies: with only `ReadTimeout` set, an idle connection is closed after `ReadTimeout`, `IdleTimeout` overrides it, and with neither the connection stays open. I also checked by hand that a negative `IdleTimeout` closes the connection after each response in Go 1.22.12 and keeps it open in Go 1.27.
- Real projects, with `--upstream-idle 60s` unless noted: `mccutchen/go-httpbin` (IG102: `ReadTimeout` defaults to 5 seconds and `IdleTimeout` isn't set, confirmed in the source), `umputun/remark42` (IG101: `IdleTimeout` is 30 seconds), `ardanlabs/service` and `stefanprodan/podinfo` (timeouts from config, reported as not checked), `traefik/whoami`, `gotify/server` and `go-shiori/shiori` (no idle timeout, fine). With their own manifests: `GoogleCloudPlatform/microservices-demo` frontend, matched through its Ingress, Service and Deployment to port 8080, and `GoogleCloudPlatform/kubernetes-engine-samples` hello-app, whose Ingress has no class and so needed `--upstream-idle 600`.
- Not tested: a live cluster, real load balancer traffic, or a reproduction of the 502 itself. The proxy defaults come from AWS, Google and ingress-nginx documentation and the ingress-nginx config template. It hasn't been tested in a fresh Claude session.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/idlegap/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `idlegap` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
