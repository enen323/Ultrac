# Ultrac

> A reconstructed AI coding agent CLI — built from the ground up on the Claude Code
> architecture. Think, act, observe, repeat.

Ultrac is a terminal-native AI agent for software engineering tasks. It reads your
codebase, runs tools (shell, file edit, search), calls language models, and loops
through a *think → act → observe* cycle until the job is done — all from your
terminal.

> ⚠️ **Status:** `1.0.0-dev`. This is an active reconstruction/learning project based
> on the Claude Code source layout. APIs and CLI surface are unstable and change
> frequently.

---

## ✨ Features

- **Agent loop core** — a persistent query engine that plans, executes tools, and
  reflects on results across turns (`src/QueryEngine.ts`, `src/query.ts`).
- **Tool system** — modular, schema-validated tools (file read/write, shell, search,
  diff, MCP bridges) built on `zod` (`src/Tool.ts`, `src/tools.ts`).
- **Multi-model support** — Anthropic, Amazon Bedrock, Google Vertex AI, and the
  Foundry SDK via a single provider abstraction.
- **MCP (Model Context Protocol)** — first-class MCP client + server support, plus
  `mcpb` packaged bundles (`src/sdk`, `src/entrypoints/mcp.ts`).
- **Rich TUI** — a full terminal UI rendered with [Ink](https://github.com/vadimdemedes/ink)
  (React for the CLI), with vim keybindings, dialogs, and interactive helpers.
- **Hooks & Skills** — lifecycle hooks (`PreToolUse`, `PostToolUse`, …) and a
  composable skills system for extending agent behavior.
- **Observability** — OpenTelemetry tracing, metrics, and logs wired throughout.
- **Flexible runtimes** — local CLI, long-running **daemon**, **remote**/server mode,
  and a **self-hosted runner** for headless automation.

## 🧱 Tech Stack

| Layer        | Choice                                                        |
|--------------|---------------------------------------------------------------|
| Language     | TypeScript (ES2022, `strict: false`), ESM                    |
| Runtime      | [Bun](https://bun.sh)                                         |
| TUI          | Ink + React 19 + `react-reconciler`                           |
| LLM clients  | `@anthropic-ai/sdk`, Bedrock, Vertex, Foundry                 |
| Protocols    | MCP (`@modelcontextprotocol/sdk`), `vscode-jsonrpc`           |
| Validation   | `zod`                                                         |
| Telemetry    | OpenTelemetry (`api` / `sdk-trace` / `sdk-metrics` / `sdk-logs`) |
| Bundler      | `bun build` (single-binary-ish CLI output)                    |

## 🚀 Getting Started

Requirements: **Bun ≥ 1.x** and Node typings are not required at runtime.

```bash
# install dependencies
bun install

# build the CLI into ./dist
bun run build

# run from source (no build step)
bun run src/entrypoints/cli.tsx --help
```

The build script targets Bun and emits to `dist/`:

```json
"build": "bun build src/entrypoints/cli.tsx --target=bun --outdir=dist"
```

## 🗂️ Project Structure

```
src/
├── entrypoints/      # CLI entry, init, mcp, sdk glue
├── query/            # the agent loop & query engine
├── tools/            # tool implementations + registry
├── services/         # backing services (models, filesystem, git…)
├── skills/           # skill loading & execution
├── hooks/            # lifecycle hooks
├── components/       # Ink TUI components
├── ink/              # low-level Ink helpers
├── cli/              # command parsing & REPL
├── commands/         # slash / built-in commands
├── daemon/           # long-running background mode
├── remote/ server/   # remote & server transports
├── sdk/              # SDK & provider adapters
├── utils/            # shared helpers
└── types/            # type shims & path aliases
```

Path aliases (see `tsconfig.json`): `src/*`, plus shims for native modules and
optional MCP servers.

## ⚙️ Configuration

Configuration is loaded from a layered set of sources (env → project → user →
defaults). See [`docs/config-loading.md`](docs/config-loading.md) for the full
resolution order, and [`docs/spec/`](docs/spec) for design notes.

## 🛠️ Development

```bash
bun install
bun run build          # production bundle
bunx tsc --noEmit      # type-check (note: strict mode is off)
bun run src/entrypoints/cli.tsx   # dev entry
```

Contributions follow the project's reconstructed layout — keep new modules under
their logical `src/` subdirectory and register tools/skills via their respective
registries.

## 📄 License

Internal reconstruction / research use. Not affiliated with or endorsed by
Anthropic. All trademarks are property of their respective owners.
