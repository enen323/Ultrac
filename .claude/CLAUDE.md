# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Purpose

Static analysis reference repo for the Claude Code source leak (March 2026). `src/` holds 1,911 raw TypeScript source files extracted from the leaked npm package — **not a buildable project**. No `package.json`, no `tsconfig.json`, no test suite, no build step. Do not try to run, build, lint, or refactor code unless the user explicitly asks.

Sibling repo at `D:/develop/Project/claude-code/` contains Chinese-language analysis docs (README + 18+ chapters). This repo is the bare source slice only.

## CodeGraph

`.codegraph/` exists at the repo root. Use `codegraph_explore` before grep/read when locating symbols or tracing flows. Shell equivalent: `codegraph explore "<query>"`. If `.codegraph/` were ever removed, skip it entirely.

## Runtime

Bun runtime. Code uses `bun:bundle` feature gates for build-time dead-code elimination, plus direct `Bun.*` APIs (`Bun.spawn`, `Bun.file`, `Bun.CryptoHasher`, `Bun.password`, `Bun.randomUUIDv7`, `Bun.TOML`, etc.).

## Import Conventions

- Relative imports dominate (`./`, `../`, `../../`).
- `src/...` path alias used for cross-directory hops (e.g., utils → services). Check `tsconfig.json` in sibling `claude-code/` for path resolution — tsconfig is not in this repo.
- Components import from internal `../../ink` abstraction, never directly from `ink` or `react`.

## `src/` Structure

- `entrypoints/cli.tsx` — CLI bootstrap with fast-paths (`--version`, `--daemon-worker`, bridge/remote-control mode). Loads full init only after flag dispatch.
- `bootstrap/state.ts` — global state singleton (cost counters, turn timing, telemetry attrs). Header warns: "DO NOT ADD MORE STATE HERE."
- `commands/` — ~100+ slash-command implementations, one directory per command. Each has an `index.ts` entry plus implementation `.tsx`/`.ts`.
- `tools/` — tool implementations. Multi-file tools get a directory (`AgentTool/`, `BashTool/`, `FileEditTool/`, `FileReadTool/`, `GrepTool/`, `MCPTool/`, `LSPTool/`, `REPLTool/`, `PowerShellTool/`). Single-file tools are flat `.ts` files.
- `services/` — background services and API clients: `api/` (Anthropic Messages API), `mcp/` (MCP transport + auth), `oauth/`, `analytics/`, `compact/` (context compression), `SessionMemory/`, `extractMemories/`, `policyLimits/`, `plugins/`, `lsp/`, `settingsSync/`, `teamMemorySync/`, plus utility services.
- `components/` — React TUI layer: REPL messages, PromptInput, permission dialogs (Bash, File, WebFetch, PlanMode, MCP), settings UI, design system, buddy sprite.
- `ink/` — internal Ink abstraction (`src/ink/components`, `ink/layout`, `ink/termio`). Components import from here, not raw Ink.
- `bridge/` — remote bridge / cross-device session continuity (JWT, polling, WebSocket/SSE transports, trusted device).
- `cli/` — non-interactive CLI output, transports (SSE, WebSocket, Hybrid), background daemon mode.
- `utils/` — large utility surface: `bash/` (shell exec, sandbox), `git/`, `github/`, `mcp/`, `sandbox/`, `swarm/` (Tmux/pane backends for multi-agent), `telemetry/`, `settings/`, `permissions/`, `skills/`, `memory/`, `ultraplan/`, `computerUse/`, `claudeInChrome/`, `nativeInstaller/`.
- `query/` — core agent execution loop (QueryEngine).
- `screens/` — full-screen TUI screens (session list, etc.).
- `types/` — generated protobuf types (`events_mono/`, `google/protobuf/`), hook types, node-globals.
- `daemon/` — long-running supervisor and per-worker logic.
- `hooks/` — hook system (notifications, tool-permission handlers).
- `memdir/` — memdir-based memory storage implementation.
- `plugins/` — plugin loading + bundled plugins.
- `skills/` — skill discovery + bundled skills.
- `keybindings/` — Vim-style keybinding state machine.
- `vim/` — Vim editing mode operators and transitions.
- `voice/` — voice input support.
- `remote/` — remote environment / managed-settings client.
- `server/` — local HTTP server pieces.
- `self-hosted-runner/` — self-hosted runner support.
- `migrations/` — schema/migration logic.
- `outputStyles/` — output style definitions.
- `buddy/` — companion sprite and notifications.
- `native-ts/` — native TS addons (color-diff, file-index, yoga-layout).
- `upstreamproxy/` — upstream proxy support.
- `tasks/` — task runtime backends (LocalShell, LocalAgent, RemoteAgent, InProcessTeammate, DreamTask).

## Testing

`src/tools/testing/` is a **testing framework implementation** (ToolTestRunner, runPluginEval, test-ai) — not a suite of `*.test.ts` files. Zero test suites exist in this repo.

## MCP

`.mcp.json` registers only `codegraph` (stdio).
