# idlegap plugin

This plugin adds the `checking-idle-timeouts` skill. It finds Go HTTP servers that close idle keep-alive connections before the AWS ALB, ingress-nginx or Google Cloud load balancer in front of them, a cause of intermittent 502 errors. It reads Go source and Kubernetes manifests and never runs your code.

It needs Go 1.22 or newer: `scripts/run.sh` builds the tool from the bundled source each time it runs. See the repository README for usage, findings and limits.
