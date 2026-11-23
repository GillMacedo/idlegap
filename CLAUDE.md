# idlegap

Claude Code skill and Go CLI that finds Go HTTP servers that close idle keep-alive connections before the proxy or load balancer in front of them, which causes intermittent 502s.

## Architecture
All code is in `plugin/scripts`, one `package main` with no dependencies, so `go run` and `run.sh` need no setup. `gosrc.go` parses Go source and resolves timeout expressions, `k8s.go` reads manifests and finds each route from a proxy to a container, `check.go` applies net/http's idle timeout rule and compares, and `main.go` has flags and output. The `yaml_*.go` files are a renamed copy of go.yaml.in/yaml/v3: don't edit them. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet, unit tests and integration tests in `tests/`
- `make lint`: plugin and marketplace validation
- `make demo`: run both examples

## Conventions
- Standard library only, plus the copied YAML parser. Never build or run the code being checked, and never contact a cluster.
- Input that can't be trusted (empty, binary, cut off, failed command output, nothing to compare) exits 3 with no result. Never print a clean result unless at least one pair was really compared.
- Each finding has a stable `IGnnn` code. Don't reuse codes.
- A proxy default must come from that proxy's documentation, linked in the pull request.
- Tests must not assume the machine they run on.

## Do and don't
- Do add a test for what a change flags and for what it must not flag.
- Don't guess a value the tool can't read. Report it as not checked.

## Definition of done
`make test`, `make lint` and `make demo` pass, and `README.md`, `ROADMAP.md` and `CHANGELOG.md` are updated when relevant.
