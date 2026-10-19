# Ultrac

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
[`docs/pi-core-harness-plan.md`](docs/pi-core-harness-plan.md) (the constitution
for this work).

**Goals (all met)**

| # | 目标 | 度量方式 |
|---|---|---|
| G1 | 可跑通真实编码任务 | agent 自主 read→edit→bash 闭环，无需人工干预 |
| G2 | 与 pi 可对照 | 同一 prompt 双跑，事件序列 diff 一致 |
| G3 | provider 可替换 | 新增一家 = 一个文件 + `init()` 注册，agent 零改动 |
| G4 | 新增工具不改引擎 | 新增工具 = 一个文件 + `init()` 注册，agent 零改动 |
| G5 | 零外部运行时 | 单个静态二进制，不依赖 Node / Bun / Python |
| G6 | 无权限层 | `internal/permission`、`internal/shell` 已从仓库消失（P7） |

**非目标（写在这里防蔓延）**

- interactive TUI（Ink/React → bubbletea 是重新设计，不是翻译；计划书 §0.2 论证过为何 pi 语义下不复用 TS TUI）
- pi 的 `durable` / `chord` / `server` / `client` / CBOR protocol
- pi 的 `mcp` / `codemode` / `extensions` / `skills` / `telemetry` / `evals`
- OAuth 登录（只做 API key）；图像/分类器模型
- compaction（上下文压缩）——超长上下文直接报错
- **权限系统**——已决策删除（pi 有意不做）
- 自动下载 `rg` / `fd`——缺失时报错并提示安装

**进度**：P0 ✅ 契约冻结 / P1 ✅ provider 流式（真实端点双 dialect 验收）/ P2 ✅ agent loop + Agent 状态机（假 provider 47 测试）/ P3 ✅ pi 8 工具（133 测试）/ P4 ✅ 会话/配置/提示词（43 新测试）/ P5 ✅ CLI + print/json（真实端点四场景验收）/ P6 ✅ RPC 模式（18 命令，驱动脚本 38/38 断言）/ P7 ✅ 退场清理 + 端到端验收（删 44 文件 / 13,255 行；E1–E7 全过，pi 双跑抓出并修掉两个协议 bug）。

**验证关卡（每阶段必过）**：`go build ./cmd/... ./internal/...`、`go vet` 同范围、
`staticcheck` 全仓、全量 `go test`。P7 起 `./...` 也可跑（TS 树与 node_modules
已退场），但 Makefile 仍用显式范围。

## 🚀 Getting Started

Requirements: **Go ≥ 1.26** and a provider credential in `~/.ultrac/settings.json`
(`env` 块：`ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_MODEL` 等，
兼容任意 anthropic-messages 或 openai-completions 端点)。

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

## 🗂️ Project Structure

```
cmd/ultrac/          # CLI: flags, print mode, RPC mode, diagnostics, shared assembly
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
[`docs/config-loading.md`](docs/config-loading.md) (written for the TS era; the Go
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
