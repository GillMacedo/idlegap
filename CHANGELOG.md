# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-23

### Added
- Compare each Go `http.Server`'s idle timeout with the proxy in front of it, following net/http's rule that `ReadTimeout` is used when `IdleTimeout` isn't set.
- Read AWS ALB, ingress-nginx and Google Cloud load balancer idle timeouts from Kubernetes manifests, through Ingress or HTTPRoute, Service and workload, including flag values from container args.
- `--upstream-idle` for other proxies, `--format json` and `--fail-on`.
- Exit code 3, with no result, for empty, garbage, cut-off or failed-command input.
- `checking-idle-timeouts` skill and plugin manifest.
