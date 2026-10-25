# Ultrac

**English** | [简体中文](README.zh-CN.md)

Ultrac is a desktop AI coding agent. Its engine is a single static Go binary
that runs headless over RPC; an Electron shell provides the chat interface and
drives that binary as a child process. The engine reads your codebase, runs
tools (bash, file edit/write, read, ls, grep, find), calls language models, and
loops through a *think → act → observe* cycle until the task is done. A CLI is
also available for direct use.

## What it looks like

Two faces share the same engine:

- **Desktop** — an Electron shell that runs the binary headless over RPC and
  shows a chat-style interface (lives in `desktop/`). This is the primary way
  to use Ultrac.
- **CLI** — run one prompt and watch tools execute in the terminal, or stream
  the event log as JSON lines.

## Install

Requires **Go ≥ 1.26** and a provider declared in `~/.ultrac/settings.json`
(see Configuration).

```bash
go build -o bin/ultrac ./cmd/ultrac
```

## Use

```bash
# one prompt — the agent runs tools and exits
./bin/ultrac -p "Read go.mod and say the module name in one sentence"

# event stream, one JSON object per line
./bin/ultrac --mode json -p "List the Go files under internal/session"

# resume the most recent session for this directory
./bin/ultrac -c "now also count the test files"

# headless RPC mode on stdin/stdout (for the desktop shell or your own client)
./bin/ultrac --mode rpc

# diagnostics
./bin/ultrac config              # settings layers + provider/credential summary
./bin/ultrac sessions [dir]      # list sessions for a directory
```

## Desktop

```bash
make build                                       # build the kernel: bin/ultrac(.exe)
cd desktop/shell-spa && npm install && npm run build   # build the page → dist/
cd ../shell && pnpm install && pnpm start             # launch the shell
```

The shell spawns `ultrac --mode rpc` and talks to it over the child's
stdin/stdout — no HTTP, no port, no token.

## Configuration

Provider credentials live in `~/.ultrac/settings.json`. One section adds a
provider — and its key — without recompiling:

```json
{
  "providers": {
    "my-proxy": {
      "baseUrl": "https://proxy.example.com/v1",
      "api": "openai-completions",
      "apiKey": "sk-my-proxy-key",
      "models": [
        { "id": "my-model", "name": "My Model", "contextWindow": 128000, "maxTokens": 8192 }
      ]
    }
  }
}
```

- Write `baseUrl` at the **API root** (`/v1`); the adapter appends
  `/chat/completions` or `/v1/messages`.
- A key-only entry is valid for built-in providers.
- You must name a model: `--model provider/id`, or the model a resumed session
  used. With none, the CLI reports `no model configured` and exits.

## Structure

```
cmd/ultrac/      # CLI: flags, print mode, RPC mode, diagnostics
internal/        # engine: agent loop, providers, session, tools, transport
desktop/         # Electron shell + React SPA
```

## License

Internal use. All trademarks are property of their respective owners.
