# Claude Code 源码重建：完整设计文档

**日期：** 2026-09-19
**目标：** 从原始 TypeScript 源码转储重建可运行的 Claude Code CLI 工具
**运行时：** Bun
**策略：** 核心路径完整还原，边缘功能功能桩标记

---

## 1. 项目现状分析

### 1.1 当前状态

| 维度 | 状态 |
|------|------|
| 源文件数 | 1,911 个 TS/TSX |
| 代码行数 | ~205,000 行 |
| 构建系统 | **无**（无 `package.json`、`tsconfig.json`、构建脚本） |
| 运行时 | Bun（198 个文件用 `bun:bundle` feature gate，22 个文件用 `Bun.*` API） |
| 外部依赖 | ink、react、@anthropic-ai/sdk、lodash-es、chalk、commander、react-reconciler、strip-ansi、figures 等 |
| 自定义层 | `src/ink/` 完整重实现（非 wrapper） |
| 缺失文件 | `src/main.js` 被 `cli.tsx` 引用但不存在（只有 `main.tsx`） |
| 宏定义 | `MACRO.VERSION` 是构建期注入，无定义来源 |
| 零字节文件 | 0 个（文件内容完整） |

### 1.2 运行链路

```
src/entrypoints/cli.tsx  (入口，fast-path 分发)
    │
    ├─ --version → 直接输出 MACRO.VERSION
    ├─ --daemon-worker → daemon worker
    ├─ remote-control / bridge → bridgeMain
    ├─ daemon → daemonMain
    ├─ ps/logs/attach/kill → bg 会话管理
    ├─ new/list/reply → template jobs
    ├─ environment-runner → headless runner
    ├─ self-hosted-runner → self-hosted runner
    ├─ --tmux --worktree → exec into tmux
    └─ 默认 → main.tsx
            │
            ▼
        init()  (配置、OAuth、代理、遥测初始化)
            │
            ▼
        launchRepl()  (启动 Ink TUI REPL)
            │
            ▼
        QueryEngine  (Agent 执行内核)
            │
            ├─ API 调用 (Anthropic Messages API)
            ├─ Tool 分发 (BashTool, FileEditTool, etc.)
            └─ 多轮对话管理
```

---

## 2. 技术方案

### 2.1 构建工具

**选择：** Bun 原生构建 (`bun build` + `tsconfig.json`)

理由：
- 代码深度依赖 `bun:bundle` feature gate
- 使用 `Bun.*` API（`Bun.spawn`、`Bun.file`、`Bun.CryptoHasher` 等）
- `bunfig.toml` 可配置宏注入（`MACRO.VERSION`）
- 无需 webpack/rollup 额外复杂度

### 2.2 宏定义策略

**`MACRO.VERSION`：**
- 默认值：运行时 fallback `"1.0.0-dev"`
- 构建期通过 `bunfig.toml` 的 `define` 覆写
- 类型声明在 `src/types/node-globals.d.ts`

**`bun:bundle` feature()：**
- Phase 1-3：运行时 shim，默认全部返回 `true`（开发模式）
- Phase 4+：可选接入构建期 DCE

### 2.3 外部依赖

**必需（核心路径）：**
- `ink` / `react` / `react-reconciler` — TUI 渲染
- `@anthropic-ai/sdk` — API 调用
- `chalk` — 终端颜色
- `lodash-es` — 工具函数
- `commander` / `@commander-js/extra-typings` — CLI 参数解析
- `strip-ansi` — ANSI 转义处理
- `figures` — 终端符号
- `react/compiler-runtime` — React compiler

**Phase 5+ 补全（当前可暂不安装）：**
- `@opentelemetry/api` — 遥测
- `@grpc/grpc-js` — gRPC 导出器
- `brotli` / `zstd` — 压缩
- `node-pty` — 伪终端
- `tree-sitter` — 代码解析
- `pacote` — npm 包管理
- `ws` / `eventsource-parser` — WebSocket / SSE
- `sudo-prompt` — 权限提升
- `clipboardy` — 剪贴板

### 2.4 内部 Import 修复

**问题：**
- 部分文件使用 `src/...` path alias（需 tsconfig paths 配置）
- 部分文件引用不存在的模块（`src/main.js`、`src/utils/config.js` 等）

**解决方案：**
- `tsconfig.json` 配置 `paths` 别名：
  ```json
  {
    "paths": {
      "src/*": ["./src/*"],
      "bun:bundle": ["./src/types/bun-bundle.d.ts"]
    }
  }
  ```
- 逐步清理 `missing-modules.d.ts` 中的声明

---

## 3. 阶段划分

### Phase 1 — 项目脚手架（1-2 周）

**目标：** 项目能 `bun build` 不报错

**任务：**
1. 创建 `package.json`（依赖声明 + scripts）
2. 创建 `tsconfig.json`（路径别名、严格模式）
3. 创建 `bunfig.toml`（宏注入）
4. 安装核心外部依赖
5. 提供 `MACRO` 运行时定义
6. 提供 `bun:bundle` feature() shim
7. 修复断裂的 import 路径
8. 清理 `missing-modules.d.ts`

**交付物：**
- `bun build src/entrypoints/cli.tsx` 成功
- 零类型错误

### Phase 2 — 类型检查通过（1-2 周）

**目标：** `tsc --noEmit` 零错误

**任务：**
1. 补全缺失的类型定义
2. 修复所有内部模块引用
3. 为 `Bun.*` API 提供类型桩
4. 补充 `@anthropic-ai/sdk` 类型

**交付物：**
- `tsc --noEmit` 零错误
- 所有模块可解析

### Phase 3 — 最小启动路径（2-3 周）

**目标：** `--version` 和基础启动路径端到端跑通

**任务：**
1. 实现 `--version` fast-path
2. 实现 `init()` 桩版本（跳过遥测、OAuth、代理）
3. 实现 `launchRepl()` 基础版本（渲染空 Ink 框架）
4. 处理所有运行时错误

**交付物：**
- `bun run src/entrypoints/cli.tsx --version` 输出版本号
- `bun run src/entrypoints/cli.tsx` 启动 Ink TUI（空框架）

### Phase 4 — REPL 外壳（3-4 周）

**目标：** 交互式 REPL 可用（输入/输出流转）

**任务：**
1. 实现基础 REPL 组件（输入框、消息列表）
2. 处理 stdin 输入
3. 实现基础 slash 命令（`/help`、`/clear`、`/exit`）
4. 实现消息渲染（用户/AI 消息）

**交付物：**
- 可输入文本
- 可看到用户消息
- slash 命令有响应

### Phase 5 — Agent 主循环（4-6 周）

**目标：** 多轮对话可用（真实 API 调用）

**任务：**
1. 实现 `QueryEngine` 核心逻辑
2. 接入 `@anthropic-ai/sdk` 真实 API 调用
3. 实现最小工具集（EchoTool、BashTool stub）
4. 实现流式输出
5. 实现错误处理和重试

**交付物：**
- 输入问题 → 流式输出 AI 回答
- 多轮对话上下文保持

### Phase 6 — 功能补全（持续）

**目标：** 逐步替换功能桩为真实实现

**任务：**
1. 权限层（Tool Permission）
2. 沙箱（Sandbox）
3. MCP（Model Context Protocol）
4. Skills（技能扩展）
5. Memory（多层级存储）
6. Session（会话管理、恢复）

---

## 4. 功能桩清单（Phase 1-3 标记）

以下功能在 Phase 1-3 **必须使用功能桩**，核心路径完成后再还原。

### 4.1 遥测与监控

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| OpenTelemetry 初始化 | `src/utils/telemetry/instrumentation.ts` | 空函数，返回 `null` | Phase 6 |
| 1P 事件日志 | `src/services/analytics/firstPartyEventLogger.ts` | 空函数 | Phase 6 |
| gRPC 导出器 | `src/utils/telemetry/instrumentation.ts` | 动态 import 跳过 | Phase 6 |
| Beta session tracing | `src/utils/telemetry/betaSessionTracing.ts` | 返回 `false` | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原 OpenTelemetry 遥测
export async function initializeTelemetry(): Promise<Meter | null> {
  return null
}
```

### 4.2 OAuth 与认证

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| OAuth 客户端 | `src/services/oauth/client.ts` | 空函数，返回 `null` | Phase 5 |
| 密钥链访问 | `src/utils/secureStorage/keychain.ts` | 环境变量 fallback | Phase 5 |
| mTLS 配置 | `src/utils/mtls.ts` | 空函数 | Phase 6 |
| AWS/GCP 凭证预取 | `src/utils/auth.ts` | 空函数 | Phase 5 |

**标记方式：**
```ts
// TODO: Phase 5 - 还原 OAuth 认证流程
export async function populateOAuthAccountInfoIfNeeded(): Promise<void> {
  // STUB: 跳过 OAuth 初始化
}
```

### 4.3 网络与代理

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| 全局 HTTP 代理 | `src/utils/proxy.ts` | 直连，无代理 | Phase 6 |
| mTLS 证书 | `src/utils/mtls.ts` | 空函数 | Phase 6 |
| CA 证书配置 | `src/utils/caCertsConfig.ts` | 空函数 | Phase 6 |
| API 预热连接 | `src/utils/apiPreconnect.ts` | 空函数 | Phase 3 |
| Upstream proxy | `src/upstreamproxy/upstreamproxy.ts` | 动态 import 跳过 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原代理配置
export function configureGlobalAgents(): void {
  // STUB: 使用默认 HTTP agent
}
```

### 4.4 远程设置与策略

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| 远程托管设置 | `src/services/remoteManagedSettings/` | 返回空配置 | Phase 6 |
| 策略限制 | `src/services/policyLimits/` | 全部允许 | Phase 5 |
| GrowthBook 特性开关 | `src/services/analytics/growthbook.ts` | 全部返回 `false` | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原远程设置
export async function loadRemoteManagedSettings(): Promise<RemoteManagedSettings | null> {
  return null
}
```

### 4.5 会话与会话存储

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| 会话注册表 | `src/utils/sessionStorage.ts` | 内存存储，不持久化 | Phase 5 |
| 会话恢复 | `src/utils/sessionStorage.ts` | 返回 `null` | Phase 5 |
| Session Ingress | `src/services/api/sessionIngress.ts` | 空函数 | Phase 6 |
| 文件下载 | `src/services/api/filesApi.ts` | 空函数 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 5 - 还原会话持久化
export async function getSession(sessionId: string): Promise<Session | null> {
  return null // STUB: 内存存储未实现
}
```

### 4.6 插件与 Skills

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| 插件加载器 | `src/utils/plugins/pluginLoader.ts` | 返回空插件列表 | Phase 6 |
| 插件 CLI 命令 | `src/services/plugins/pluginCliCommands.ts` | 空命令列表 | Phase 6 |
| Skills 加载 | `src/skills/` | 返回空技能列表 | Phase 6 |
| Skill Change Detector | `src/utils/skills/skillChangeDetector.ts` | 空函数 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原插件系统
export async function loadPlugins(): Promise<Plugin[]> {
  return [] // STUB: 无插件
}
```

### 4.7 工具实现（Phase 4-5 逐步补全）

| 工具 | 文件 | Phase 1-3 实现 | Phase 还原 |
|------|------|----------------|------------|
| BashTool | `src/tools/BashTool/` | 完整实现（核心工具） | - |
| FileReadTool | `src/tools/FileReadTool/` | 完整实现 | - |
| FileEditTool | `src/tools/FileEditTool/` | 完整实现 | - |
| FileWriteTool | `src/tools/FileWriteTool/` | 完整实现 | - |
| GlobTool | `src/tools/GlobTool/` | 完整实现 | - |
| GrepTool | `src/tools/GrepTool/` | 完整实现 | - |
| LS Tool | `src/tools/LSTool/` | 完整实现 | - |
| AgentTool | `src/tools/AgentTool/` | 桩（返回假数据） | Phase 5 |
| TaskCreateTool | `src/tools/TaskCreateTool/` | 桩 | Phase 6 |
| TodoWriteTool | `src/tools/TodoWriteTool/` | 桩 | Phase 6 |
| WebFetchTool | `src/tools/WebFetchTool/` | 桩 | Phase 5 |
| WebSearchTool | `src/tools/WebSearchTool/` | 桩 | Phase 5 |
| SkillTool | `src/tools/SkillTool/` | 桩 | Phase 6 |
| MCPTool | `src/tools/MCPTool/` | 桩 | Phase 6 |
| PowerShellTool | `src/tools/PowerShellTool/` | 桩（Windows 专属） | Phase 6 |
| NotebookEditTool | `src/tools/NotebookEditTool/` | 桩 | Phase 6 |
| SentinelHookTool | `src/tools/SentinelHookTool/` | 桩 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 5 - 还原 AgentTool 完整实现
export async function executeAgentTool(params: AgentToolParams): Promise<ToolResult> {
  // STUB: 返回假数据
  return {
    content: [{ type: 'text', text: '[STUB] AgentTool not implemented yet' }],
    stop_reason: 'end_turn',
  }
}
```

### 4.8 Daemon 与后台任务

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| Daemon 主进程 | `src/daemon/main.ts` | 直接退出 | Phase 6 |
| Daemon Worker | `src/daemon/workerRegistry.ts` | 空函数 | Phase 6 |
| Background Tasks | `src/utils/backgroundTasks.ts` | 空函数 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原 Daemon 后台任务
export async function runDaemonWorker(kind: string): Promise<void> {
  console.log(`[STUB] daemon worker: ${kind}`) // STUB
}
```

### 4.9 Bridge 与 Remote Control

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| Bridge Main | `src/bridge/bridgeMain.ts` | 返回禁用错误 | Phase 6 |
| Bridge UI | `src/bridge/bridgeUI.ts` | 空组件 | Phase 6 |
| Bridge API | `src/bridge/bridgeApi.ts` | 空函数 | Phase 6 |
| JWT 工具 | `src/bridge/jwtUtils.ts` | 空函数 | Phase 6 |
| Trusted Device | `src/bridge/trustedDevice.ts` | 空函数 | Phase 6 |
| Poll Config | `src/bridge/pollConfig.ts` | 返回默认值 | Phase 6 |

**标记方式：**
```ts
// TODO: Phase 6 - 还原 Bridge Remote Control
export async function bridgeMain(args: string[]): Promise<void> {
  console.error('[STUB] Bridge mode not implemented') // STUB
  process.exit(1)
}
```

### 4.10 其他边缘功能

| 模块 | 文件 | 桩实现 | Phase 还原 |
|------|------|--------|------------|
| Voice 输入 | `src/voice/` | 空函数 | Phase 6 |
| Computer Use | `src/utils/computerUse/` | 空函数 | Phase 6 |
| Chrome Integration | `src/utils/claudeInChrome/` | 空函数 | Phase 6 |
| Environment Runner | `src/environment-runner/` | 直接退出 | Phase 6 |
| Self-Hosted Runner | `src/self-hosted-runner/` | 直接退出 | Phase 6 |
| Coordinator Mode | `src/coordinator/` | 空函数 | Phase 6 |
| Assistant (KAIROS) | `src/assistant/` | 空函数 | Phase 6 |
| Buddy Sprite | `src/buddy/` | 空组件 | Phase 6 |
| LSP Manager | `src/services/lsp/` | 空函数 | Phase 6 |
| Analytics Sinks | `src/utils/sinks.ts` | 空函数 | Phase 6 |
| Startup Profiler | `src/utils/startupProfiler.ts` | 空函数（Phase 3 后可删） | Phase 6 |
| MDM 读取 | `src/utils/settings/mdm/rawRead.ts` | 空函数 | Phase 6 |
| Keychain 预取 | `src/utils/secureStorage/keychainPrefetch.ts` | 空函数 | Phase 6 |
| 剪贴板 | `src/utils/clipboard.ts` | 空函数 | Phase 6 |
| 编辑器集成 | `src/utils/editor.ts` | 空函数 | Phase 6 |
| 防休眠 | `src/services/preventSleep.ts` | 空函数 | Phase 6 |
| 通知 | `src/services/notifier.ts` | 空函数 | Phase 6 |
| 插件 Marketplace | `src/utils/plugins/marketplaceManager.ts` | 空函数 | Phase 6 |
| Swarm / 多 Agent | `src/utils/swarm/` | 空函数 | Phase 6 |
| Git 操作 | `src/utils/git/` | Shell 命令 fallback | Phase 5 |
| GitHub API | `src/utils/github/` | 空函数 | Phase 6 |
| Settings Sync | `src/services/settingsSync/` | 空函数 | Phase 6 |
| Team Memory Sync | `src/services/teamMemorySync/` | 空函数 | Phase 6 |
| Compact (上下文压缩) | `src/services/compact/` | 空函数 | Phase 6 |
| Session Memory | `src/services/SessionMemory/` | 空函数 | Phase 6 |
| Extract Memories | `src/services/extractMemories/` | 空函数 | Phase 6 |
| Native Installer | `src/utils/nativeInstaller/` | 空函数 | Phase 6 |
| Update Checker | `src/cli/update.ts` | 空函数 | Phase 6 |

---

## 5. 核心路径定义（Phase 1-5 必须完整还原）

### 5.1 核心路径清单

以下模块**禁止使用功能桩**，必须完整还原：

1. **CLI 入口**
   - `src/entrypoints/cli.tsx` — fast-path 分发
   - `src/main.tsx` — 主启动逻辑
   - `src/entrypoints/init.ts` — 初始化流程

2. **REPL 渲染层**
   - `src/replLauncher.tsx` — REPL 启动器
   - `src/screens/REPL.tsx` — REPL 主屏幕
   - `src/ink/` — Ink 渲染层完整实现
   - `src/components/PromptInput/PromptInput.tsx` — 输入组件
   - `src/components/` — 基础 UI 组件

3. **Agent 执行内核**
   - `src/QueryEngine.ts` — 查询引擎
   - `src/query.ts` — 查询入口
   - `src/Tool.ts` — Tool 基类
   - `src/Task.ts` — Task 基类

4. **API 调用**
   - `src/services/api/claude.ts` — Anthropic API 客户端
   - `src/services/api/errors.ts` — 错误处理
   - `src/services/api/withRetry.ts` — 重试逻辑

5. **核心工具**
   - `src/tools/BashTool/` — Bash 工具（完整实现）
   - `src/tools/FileReadTool/` — 文件读取
   - `src/tools/FileEditTool/` — 文件编辑
   - `src/tools/FileWriteTool/` — 文件写入
   - `src/tools/GlobTool/` — 文件匹配
   - `src/tools/GrepTool/` — 文本搜索
   - `src/tools/LSTool/` — 目录列表

6. **核心工具类**
   - `src/utils/config.ts` — 配置管理
   - `src/utils/messages.ts` — 消息格式化
   - `src/utils/envUtils.ts` — 环境变量工具
   - `src/bootstrap/state.ts` — 全局状态管理
   - `src/commands.ts` — 命令注册
   - `src/context.ts` — 上下文管理

### 5.2 核心路径验证标准

每个 Phase 完成后，必须通过以下验证：

**Phase 1 验证：**
```bash
bun build src/entrypoints/cli.tsx  # 成功
```

**Phase 2 验证：**
```bash
tsc --noEmit  # 零错误
```

**Phase 3 验证：**
```bash
bun run src/entrypoints/cli.tsx --version  # 输出版本号
bun run src/entrypoints/cli.tsx  # 启动 Ink TUI（空框架，无崩溃）
```

**Phase 4 验证：**
```bash
bun run src/entrypoints/cli.tsx  # 可输入文本，slash 命令有响应
```

**Phase 5 验证：**
```bash
bun run src/entrypoints/cli.tsx  # 输入问题 → 流式输出 AI 回答
```

---

## 6. 依赖关系图

### 6.1 外部依赖

```
核心依赖（Phase 1 必须安装）
├── ink
├── react
├── react-reconciler
├── @anthropic-ai/sdk
├── chalk
├── lodash-es
├── commander / @commander-js/extra-typings
├── strip-ansi
└── figures

可选依赖（Phase 5+ 按需安装）
├── @opentelemetry/api
├── @grpc/grpc-js
├── brotli
├── zstd
├── node-pty
├── tree-sitter
├── pacote
├── ws
├── eventsource-parser
├── sudo-prompt
├── clipboardy
└── react/compiler-runtime
```

### 6.2 内部依赖（核心路径）

```
src/entrypoints/cli.tsx
    ├── src/main.tsx
    │   ├── src/entrypoints/init.ts
    │   │   ├── src/bootstrap/state.ts
    │   │   ├── src/utils/config.ts
    │   │   ├── src/utils/telemetry/instrumentation.ts [STUB]
    │   │   ├── src/services/oauth/client.ts [STUB]
    │   │   └── src/utils/proxy.ts [STUB]
    │   │
    │   ├── src/replLauncher.tsx
    │   │   ├── src/ink/ (完整渲染层)
    │   │   ├── src/components/ (UI 组件)
    │   │   └── src/screens/REPL.tsx
    │   │
    │   ├── src/QueryEngine.ts
    │   │   ├── src/services/api/claude.ts
    │   │   ├── src/Tool.ts
    │   │   └── src/tools/ (工具集)
    │   │
    │   └── src/commands.ts (命令注册)
    │
    └── src/utils/earlyInput.ts
```

---

## 7. 风险评估

### 7.1 高风险项

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| `bun:bundle` feature() 无法 100% 还原 | 某些条件编译逻辑失效 | Phase 1 用运行时 shim，Phase 4 接入构建期 DCE |
| `Bun.*` API 在 Node.js 不可用 | 部分功能无法测试 | 提供 Bun API polyfill 层 |
| 构建期宏（`MACRO.VERSION`）影响行为 | 版本检查逻辑失效 | 提供合理默认值 |
| Ink 完整重实现的兼容性 | 渲染层可能有 bug | Phase 4 逐步调试 |
| Anthropic API 密钥 | 无法真实调用 API | Phase 5 前提供 mock 层 |

### 7.2 中等风险项

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 1,911 个文件的编译时间 | 开发体验差 | 使用 Bun 快速编译 |
| 循环依赖 | 构建失败 | 逐步修复 |
| 类型定义不完整 | 类型检查失败 | 逐步补充 |

### 7.3 低风险项

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 外部依赖版本不兼容 | 运行时错误 | 精确版本锁定 |
| 路径别名配置错误 | 模块无法解析 | Phase 1 重点测试 |

---

## 8. 时间估算

| Phase | 时间 | 关键里程碑 |
|-------|------|------------|
| Phase 1 — 脚手架 | 1-2 周 | `bun build` 成功 |
| Phase 2 — 类型检查 | 1-2 周 | `tsc --noEmit` 零错误 |
| Phase 3 — 最小启动 | 2-3 周 | `--version` 跑通，TUI 启动 |
| Phase 4 — REPL 外壳 | 3-4 周 | 可输入，slash 命令响应 |
| Phase 5 — Agent 主循环 | 4-6 周 | 真实 API 调用，多轮对话 |
| Phase 6 — 功能补全 | 持续 | 逐步替换功能桩 |

**总计：** 2-3 个月达到可用 REPL，6 个月+ 完整还原

---

## 9. 附录

### 9.1 缺失模块清单（`missing-modules.d.ts`）

当前声明但未实现的模块：

```ts
declare module '../daemon/workerRegistry.js';
declare module '../daemon/main.js';
declare module '../cli/bg.js';
declare module '../cli/handlers/templateJobs.js';
declare module '../environment-runner/main.js';
declare module '../self-hosted-runner/main.js';
declare module '../main.js';  // 应该是 main.tsx
declare module '../utils/config.js';
declare module '../utils/sinks.js';
```

**处理计划：**
- `../main.js` → 改为 `../main.tsx`（Phase 1）
- 其余模块 → 逐步实现或删除声明

### 9.2 外部依赖完整列表

从源码提取的所有外部 import：

```
@anthropic-ai/claude-agent-sdk
@anthropic-ai/mcpb
@anthropic-ai/sandbox-runtime
@anthropic-ai/sdk
@anthropic-ai/sdk/error
@anthropic-ai/sdk/resources
@anthropic-ai/sdk/resources/beta/messages.js
@anthropic-ai/sdk/resources/beta/messages/messages.mjs
@anthropic-ai/sdk/resources/index.mjs
@anthropic-ai/sdk/resources/messages.js
@anthropic-ai/sdk/resources/messages.mjs
@anthropic-ai/sdk/resources/messages/messages.mjs
@anthropic-ai/sdk/streaming.mjs
chalk
ink
lodash-es
lodash-es/capitalize.js
lodash-es/cloneDeep.js
lodash-es/isEqual.js
lodash-es/isObject.js
lodash-es/isPlainObject.js
lodash-es/last.js
lodash-es/mapValues.js
lodash-es/memoize.js
lodash-es/mergeWith.js
lodash-es/noop.js
lodash-es/omit.js
lodash-es/partition.js
lodash-es/pickBy.js
lodash-es/reject.js
lodash-es/sample.js
lodash-es/setWith.js
lodash-es/sumBy.js
lodash-es/throttle.js
lodash-es/uniqBy.js
lodash-es/zipObject.js
react
react-reconciler
react-reconciler/constants.js
strip-ansi
```

### 9.3 Bun API 使用清单

从源码提取的所有 `Bun.*` API 调用：

```ts
Bun.spawn()
Bun.file()
Bun.CryptoHasher
Bun.password
Bun.randomUUIDv7()
Bun.TOML
// 共 22 处使用
```

**处理计划：**
- Phase 1：提供类型声明（`src/types/bun-api.d.ts`）
- Phase 3：提供 polyfill 层（Node.js 兼容）

---

## 10. 下一步行动

**立即执行（Phase 1）：**

1. 创建 `package.json`（依赖声明）
2. 创建 `tsconfig.json`（路径别名）
3. 创建 `bunfig.toml`（宏注入）
4. 安装外部依赖：`bun install`
5. 修复 `src/main.js` → `src/main.tsx`（`missing-modules.d.ts`）
6. 提供 `MACRO` 运行时定义
7. 提供 `bun:bundle` feature() shim
8. 验证：`bun build src/entrypoints/cli.tsx` 成功

**完成后进入 Phase 2。**
