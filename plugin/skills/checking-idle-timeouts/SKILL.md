---
name: checking-idle-timeouts
description: Checks whether a Go HTTP server closes idle keep-alive connections before the load balancer or ingress in front of it, a common cause of intermittent 502 Bad Gateway errors that never reach a handler. Reads http.Server IdleTimeout and ReadTimeout from Go source, and the upstream idle timeout of AWS ALB, ingress-nginx and Google Cloud load balancers from Kubernetes manifests. Use when the user sees random or occasional 502s behind a load balancer, asks what IdleTimeout to set, or wants Go service and Kubernetes timeouts reviewed together.
---

# Checking idle timeouts

A server must keep idle connections open longer than the proxy in front of it. Otherwise the proxy can reuse a connection at the moment the server closes it, and that request fails with a 502.

## Run the tool

It needs Go 1.22 or newer. It only reads files, and never builds or runs the project.

    sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" --k8s DEPLOY_DIR GO_PACKAGE_DIR

- `--k8s` takes manifest files, folders or globs, and can repeat. Use rendered output for Helm: `helm template ... > /tmp/rendered.yaml`, then pass that file. Ask before running commands that contact a cluster.
- With no manifests, ask the user which proxy is in front and pass its timeout: `--upstream-idle 60s`.
- A Go package folder ending in `/...` includes subfolders.
- Add `--format json` to read the result in detail.

Exit codes: 0 means every compared pair is fine, 1 means findings, 2 means bad options, and 3 means the input can't be trusted or nothing could be compared. With exit 3 there is no result, so never report the setup as fine.

## Read the result

- `IG101` and `IG102` are the real problem. `IG102` means `IdleTimeout` isn't set and `ReadTimeout` is used instead, which people often don't know.
- `IG201` means a timeout comes from config the tool can't follow. Find the real value yourself, in config files or Helm values, and compare it with the proxy's.
- `IG202` means the proxy is one the tool doesn't know. Ask the user, or look up that proxy's upstream idle timeout, then rerun with `--upstream-idle`.
- `IG203` and `IG301` are about matching servers to ports. Check that the right server was compared.
- `IG302` means a flag default was used and the container's args weren't found.

## Report

1. For each problem, give the server's `file:line`, its idle timeout, the proxy and its timeout.
2. Propose the smallest fix: set `IdleTimeout` above the proxy's timeout, matching how the project sets other timeouts. The tool suggests a value a few seconds or about 3% above the proxy's.
3. Say what wasn't checked: values set at run time, environment variables, other proxies in the path such as a service mesh sidecar, and HTTP/2 or gRPC connections.

## Don't

Don't change code unless the user asks. Don't call the setup safe when the tool exited 3 or reported warnings.
