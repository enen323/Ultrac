# CLAUDE.md

本文件为在使用本仓库代码时提供 Claude Code (claude.ai/code) 操作指引。

## 仓库用途

Claude Code 源码泄露事件（2026 年 3 月）的静态分析参考仓库。`src/` 目录包含从泄露 npm 包中提取的 1,911 个原始 TypeScript 源文件——**不是可构建项目**。没有 `package.json`、`tsconfig.json`、测试套件或构建步骤。除非用户明确要求，否则不要尝试运行、构建、lint 或重构代码。

同级仓库 `D:/develop/Project/claude-code/` 包含中文分析文档（README + 18+ 章节）。本仓库仅包含裸源码。

## CodeGraph

`.codegraph/` 位于仓库根目录。定位符号或追踪流程时，先在 grep/read 之前使用 `codegraph_explore`。Shell 等价命令：`codegraph explore "<query>"`。如果 `.codegraph/` 被删除，则完全跳过它。

## 运行时

Bun 运行时。代码使用 `bun:bundle` 特性门控进行构建时死代码消除，同时直接调用 `Bun.*` API（`Bun.spawn`、`Bun.file`、`Bun.CryptoHasher`、`Bun.password`、`Bun.randomUUIDv7`、`Bun.TOML` 等）。

## 导入约定

- 以相对导入为主（`./`、`../`、`../../`）。
- `src/...` 路径别名用于跨目录跳转（如 utils → services）。路径解析方式参考同级仓库 `claude-code/` 中的 `tsconfig.json`——本仓库不包含 tsconfig。
- 组件从内部 `../../ink` 抽象层导入，绝不直接从 `ink` 或 `react` 导入。

## `src/` 目录结构

- `entrypoints/cli.tsx` — CLI 启动入口，包含快速路径（`--version`、`--daemon-worker`、bridge/remote-control 模式）。仅在标志分发后加载完整初始化。
- `bootstrap/state.ts` — 全局状态单例（成本计数器、回合计时、遥测属性）。头部警告："DO NOT ADD MORE STATE HERE."
- `commands/` — 约 100+ 个斜杠命令实现，每个命令一个目录。每个目录包含 `index.ts` 入口以及 `.tsx`/`.ts` 实现文件。
- `tools/` — 工具实现。多文件工具使用目录（`AgentTool/`、`BashTool/`、`FileEditTool/`、`FileReadTool/`、`GrepTool/`、`MCPTool/`、`LSPTool/`、`REPLTool/`、`PowerShellTool/`）。单文件工具为扁平 `.ts` 文件。
- `services/` — 后台服务和 API 客户端：`api/`（Anthropic Messages API）、`mcp/`（MCP 传输与认证）、`oauth/`、`analytics/`、`compact/`（上下文压缩）、`SessionMemory/`、`extractMemories/`、`policyLimits/`、`plugins/`、`lsp/`、`settingsSync/`、`teamMemorySync/`，以及其他工具服务。
- `components/` — React TUI 层：REPL 消息、PromptInput、权限对话框（Bash、File、WebFetch、PlanMode、MCP）、设置 UI、设计系统、buddy 精灵。
- `ink/` — 内部 Ink 抽象层（`src/ink/components`、`ink/layout`、`ink/termio`）。组件从此处导入，不直接导入原生 Ink。
- `bridge/` — 远程桥接 / 跨设备会话连续性（JWT、轮询、WebSocket/SSE 传输、可信设备）。
- `cli/` — 非交互式 CLI 输出、传输（SSE、WebSocket、Hybrid）、后台守护进程模式。
- `utils/` — 大型工具集合：`bash/`（shell 执行、沙箱）、`git/`、`github/`、`mcp/`、`sandbox/`、`swarm/`（Tmux/pane 多代理后端）、`telemetry/`、`settings/`、`permissions/`、`skills/`、`memory/`、`ultraplan/`、`computerUse/`、`claudeInChrome/`、`nativeInstaller/`。
- `query/` — 核心代理执行循环（QueryEngine）。
- `screens/` — 全屏 TUI 界面（会话列表等）。
- `types/` — 生成的 protobuf 类型（`events_mono/`、`google/protobuf/`）、hook 类型、node-globals。
- `daemon/` — 长期运行的监控进程及各 worker 逻辑。
- `hooks/` — hook 系统（通知、工具权限处理器）。
- `memdir/` — 基于 memdir 的内存存储实现。
- `plugins/` — 插件加载 + 内置插件。
- `skills/` — 技能发现 + 内置技能。
- `keybindings/` — Vim 风格键位状态机。
- `vim/` — Vim 编辑模式操作符与转换。
- `voice/` — 语音输入支持。
- `remote/` — 远程环境 / 托管设置客户端。
- `server/` — 本地 HTTP 服务组件。
- `self-hosted-runner/` — 自托管 runner 支持。
- `migrations/` —  schema/迁移逻辑。
- `outputStyles/` — 输出样式定义。
- `buddy/` — 伴生精灵与通知。
- `native-ts/` — 原生 TS 插件（color-diff、file-index、yoga-layout）。
- `upstreamproxy/` — 上游代理支持。
- `tasks/` — 任务运行时后端（LocalShell、LocalAgent、RemoteAgent、InProcessTeammate、DreamTask）。

## 测试

`src/tools/testing/` 是**测试框架实现**（ToolTestRunner、runPluginEval、test-ai）——不是 `*.test.ts` 文件套件。本仓库不存在任何测试套件。

## MCP

`.mcp.json` 仅注册了 `codegraph`（stdio）。
