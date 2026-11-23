# Architecture

    Go packages --> gosrc.go: parse, find http.Server and http.ListenAndServe, record IdleTimeout, ReadTimeout, Addr
    manifests  --> k8s.go: parse YAML, refuse broken input, find Ingress and HTTPRoute
                   route: proxy --> Service --> port --> Deployment container (and its args)
    check.go   --> for each route and server on that port: evaluate the timeouts with the container's args,
                   apply net/http's rule, compare with the proxy's idle timeout
    main.go    --> table, findings, exit code

- **net/http's rule:** `IdleTimeout` if non-zero, else `ReadTimeout`. Zero means no idle timeout. A negative value is reported as not checked, because Go 1.22 and Go 1.23 treat it differently (`tests/integration_test.go` checks the rule on a real server).
- **Values are evaluated, not guessed.** Literals, constants, `time` units, arithmetic, `time.Duration(n)`, and `flag` or `pflag` definitions are followed. A flag's value comes from the container's `args` when the manifests have them, otherwise from its default. Anything else is "not checked".
- **Proxies:** AWS ALB (annotation, Ingress groups, `IngressClassParams`), ingress-nginx (controller ConfigMap), and Google Cloud load balancers (fixed at 600 seconds). Others need `--upstream-idle`.
- **Broken input** is refused before any comparison: empty, binary, not UTF-8, manifests without a final newline, YAML that doesn't parse, objects missing required fields, and text that starts like an error from helm, kubectl, kustomize or make.

Key choices are in `decisions/`.
