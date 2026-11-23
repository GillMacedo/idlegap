# Roadmap

## Next
- Read the `go` line in `go.mod`, so a negative timeout can be judged instead of reported as not checked
- Container `env` values for timeouts read with `os.Getenv`
- Traefik, Envoy Gateway and Istio sidecars as known proxies

## Later
- gin, echo and other frameworks that create the `http.Server` for you
- AWS NLB and Azure Application Gateway
- Following timeouts through config structs filled from YAML or environment
