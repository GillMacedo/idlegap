# 0001: A skill with Go source, not an MCP server

**Decision:** idlegap ships as a Claude Code skill with a standard-library Go package and a `run.sh` that builds it, instead of an MCP server with prebuilt binaries.

**Why:** people with Go services already have Go, so building from source is the whole install. The agent needs one command and its output, which a skill gives without a server process. Reading source with `go/parser` needs no build, so it works on code that doesn't compile here and never runs anyone's code.

**Alternatives:** an MCP server would need binaries for each platform and an npm package, for no extra capability. Type checking with `golang.org/x/tools` would follow more values, but needs a buildable project and dependencies.
