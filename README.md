# Ultrac

**English** | [简体中文](README.zh-CN.md)

> A Go coding-agent engine — a from-scratch port of the **pi core harness**
> (agent loop, event protocol, session format, tools, print/RPC modes).
> Think, act, observe, repeat — from a single static binary.

Ultrac is a terminal-native AI agent for software engineering tasks. It reads your
codebase, runs tools (bash, file edit/write, read, ls, grep, find), calls language
models, and loops through a *think → act → observe* cycle until the job is done.

> ⚠️ **Status:** `1.0.0-dev`. The engine is complete (P0–P7 of the port plan, all
> acceptance gates passed); the CLI surface is still pi's headless surface — no
> interactive TUI by design.

---

## 🎯 What this is

Ultrac's engine (`cmd/`, `internal/`) is a **Go implementation of pi's core
harness**, replacing the earlier Claude-Code-shaped TypeScript reconstruction.
The port plan — decisions, deviations, and per-phase acceptance records — lives in
[`docs/开发计划/pi-core-harness-plan.md`](docs/开发计划/pi-core-harness-plan.md) (the constitution
for this work).

**Goals (all met)**

| # | Goal | Measure |
|---|---|---|
| G1 | Runs real coding tasks | agent completes the read→edit→bash loop autonomously, no human intervention |
| G2 | Comparable to pi | same prompt double-run, event sequences diff identical |
| G3 | Swappable providers | new provider = one file + `init()` registration, agent untouched |
| G4 | New tools without engine changes | new tool = one file + `init()` registration, agent untouched |
| G5 | Zero external runtime | a single static binary, no Node / Bun / Python dependency |
| G6 | No permission layer | `internal/permission`, `internal/shell` removed from the repo (P7) |

**Non-goals (written down to prevent scope creep)**

- interactive TUI (Ink/React → bubbletea is a redesign, not a translation; plan §0.2 argues why the TS TUI isn't reused under pi semantics)
- pi's `durable` / `chord` / `server` / `client` / CBOR protocol
- pi's `mcp` / `codemode` / `extensions` / `skills` / `telemetry` / `evals`
- OAuth login (API key only); image/classifier models
- compaction (context compression) — over-long contexts error out
- **permission system** — decided to remove (pi deliberately has none)
- auto-downloading `rg` / `fd` — missing binaries error out with an install hint

**Progress**: P0 ✅ contracts frozen / P1 ✅ provider streaming (real-endpoint, both dialects) / P2 ✅ agent loop + Agent state machine (47 tests with a fake provider) / P3 ✅ pi's 8 tools (133 tests) / P4 ✅ session/config/prompt (43 new tests) / P5 ✅ CLI + print/json (four real-endpoint scenarios) / P6 ✅ RPC mode (18 commands, driver script 38/38 assertions) / P7 ✅ retirement cleanup + end-to-end acceptance (44 files / 13,255 lines removed; E1–E7 all pass, the double run caught and fixed two protocol bugs).

**Verification gates (required every phase)**: `go build ./cmd/... ./internal/...`,
`go vet` on the same scope, `staticcheck` across the whole repo, and the full
`go test` suite. Since P7 `./...` also works (the TS tree and node_modules are gone),
but the Makefile still uses explicit scopes.

## 🚀 Getting Started

Requirements: **Go ≥ 1.26** and a provider credential in `~/.ultrac/settings.json`
(an `env` block: `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_MODEL`
etc., compatible with any anthropic-messages or openai-completions endpoint).

```bash
# build (or: make build)
go build -o bin/ultrac ./cmd/ultrac

# one prompt, print mode — the agent runs tools and exits
./bin/ultrac -p "Read go.mod and say the module name in one sentence"

# event stream, one JSON object per line
./bin/ultrac --mode json -p "List the Go files under internal/session"

# resume the most recent session for this directory
./bin/ultrac -c "now also count the test files"

# RPC mode: the headless JSONL protocol on stdin/stdout
./bin/ultrac --mode rpc

# diagnostics
./bin/ultrac config                                   # settings layers + effective env
./bin/ultrac sessions [dir]                           # list sessions for a directory
./bin/ultrac probe -mode anthropic                    # one real request, 12-event coverage
```

`make live-agent` runs the read-only real-endpoint acceptance prompts.

## 🖥️ Desktop App (Electron + React SPA)

The desktop UI is two packages under `desktop/` (the old `reasonix-desktop/` tree is a
**read-only reference copy** of Reasonix's shell — gitignored, do **not** run it):

- `desktop/shell/` — Electron shell. Spawns `bin/ultrac --mode rpc` and bridges the
  C4 JSONL protocol over stdio (the pipe *is* the security boundary — no token/gate).
- `desktop/shell-spa/` — React 19 + Vite + TypeScript SPA, the chat face loaded into
  the window.

The full design is in [`docs/开发计划/desktop-shell-plan.md`](docs/开发计划/desktop-shell-plan.md).

**Prerequisites**

- Go ≥ 1.26 (to build the kernel — already built as `bin/ultrac.exe` in this repo)
- Node ≥ 22.18 (the managed `node` 22.22.2 here works)
- A provider credential in `~/.ultrac/settings.json` (same as the CLI — see above)

**Build & run**

```bash
# 1. Build the Go kernel (already present as bin/ultrac.exe; rebuild if you change Go code)
go build -o bin/ultrac ./cmd/ultrac

# 2. Build the SPA — produces desktop/shell-spa/dist/ (the window loads this)
cd desktop/shell-spa
npm install
npm run build
cd ../..

# 3. Install the Electron shell, then launch it
cd desktop/shell
npm install          # pulls electron ^44 (uses the npmmirror electron mirror in .npmrc)
npm start            # runs `electron .`
```

The shell auto-discovers the kernel at `bin/ultrac.exe` (dev path
`desktop/shell/src/../../../bin`) and the page at `desktop/shell-spa/dist`.
Override either with the env knobs `ULTRAC_BIN` / `ULTRAC_SHELL_PAGE`.

> ⚠️ **Windows / this-machine gotcha:** the environment sets `ELECTRON_RUN_AS_NODE=1`
> globally, which makes `electron .` degrade to pure Node (no GUI window). Launch with
> the variable unset:
>
> ```bash
> cd desktop/shell
> env -u ELECTRON_RUN_AS_NODE npm start
> ```

**How the pieces fit** (one line): the Electron window loads the SPA; the SPA talks to
the shell's preload bridge (`kernel:call` / `kernel:event` / `session_loaded` /
`shell:open-external`); the shell spawns `ultrac --mode rpc` over stdio. Closing the
window closes stdin, the kernel exits (parent-lease semantics) — no extra plumbing.

**SPA dev loop** (hot reload, no Electron): `cd desktop/shell-spa && npm run dev`
serves the UI at the Vite dev URL; point it at a running RPC kernel for live testing.

## 🗂️ Project Structure

```
cmd/ultrac/          # CLI: flags, print mode, RPC mode, diagnostics, shared assembly
desktop/            # Electron shell + React SPA (see "🖥️ Desktop App" above)
├── shell/           #   Electron shell — spawns `ultrac --mode rpc`, JSONL bridge
└── shell-spa/       #   React 19 + Vite SPA — the chat UI loaded into the window
internal/
├── agent/           # C3: agent loop, Agent state machine, tool-call execution
├── ai/              # C1+C2: message/event types, provider registry, dialect adapters
│   ├── anthropic/   #   anthropic-messages adapter
│   └── openai/      #   openai-completions adapter
├── config/          # settings layers, env application, path resolution
├── prompt/          # segmented system prompt (sections patch semantics)
├── rpc/             # C4: RPC command/response types + JSONL server
├── session/         # pi-shaped SessionManager (header + entry tree + uuidv7)
├── tool/            # the 8 tools (bash, edit, find, grep, ls, read, write + powershell)
├── transport/       # pi wire protocol writer (print/json + RPC share it)
├── logging/         # dated error/MCP logs
└── version/         # build metadata
```

## 🧪 Verifying against pi

The acceptance gate is a **double run**: the same prompt through the real pi and
through ultrac, event-type sequences diffed. The harness script (local, not
tracked — see `bin/`):

```bash
bash bin/double-run.sh "Reply with exactly: E2-OK"
```

Setup notes for the pi side (node/npm, the `dsanthropic` provider-id collision
workaround, `</dev/null`) are documented in the script's header comments and in
the plan's P7 record.

## ⚙️ Configuration

Layered resolution (user `~/.ultrac/settings.json` → project `.ultrac/settings.json`
→ env → defaults), with host-management and safe-allowlist filters reported by
`ultrac config`. The resolution order is described in
[`docs/项目理解/config-loading.md`](docs/项目理解/config-loading.md) (written for the TS era; the Go
layer mirrors the same order — see `internal/config/`).

## 📜 Note on the TypeScript tree

The original Claude-Code-shaped TypeScript reconstruction (`src/`, ~513k lines,
Ink TUI, bun build) was **retired in P7**: with the engine on pi semantics it no
longer had a consumer (plan §0.2 / risk R11), and the port was complete and
double-run verified. The full tree remains in git history (pre-P7 commits); the
Go engine is the implementation.

## 📄 License

Internal reconstruction / research use. Not affiliated with or endorsed by
Anthropic. All trademarks are property of their respective owners.
