# 0002: Refuse input that can't be trusted, with exit code 3

**Decision:** when an input is empty, garbage, cut off, the output of a failed command, or leaves nothing to compare, idlegap prints no table and exits 3, whatever `--fail-on` says.

**Why:** a check that passes on a failed `helm template` or an empty folder hides the problem it exists to find. A manifest cut inside a value can still be valid YAML (`timeout_seconds=12` from `120`), so manifests must also end with a newline.

**Trade-off:** a real manifest without a final newline is refused, with a message saying to add one. Go files don't need one, because the Go parser rejects a file cut inside a declaration and real Go files sometimes lack it.
