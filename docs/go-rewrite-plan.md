# Ultrac Go 重写计划书

> 版本：v1.0（草案）
> 日期：2026-10-07
> 基线：`claude-code-reconstructed@1.0.0-dev`
> 适用范围：把 `src/` 下 1929 个 TypeScript 文件、513,602 行代码迁移到 Go

---

## 0. 结论摘要（先看这节）

**本项目不建议"一次性全量重写"。** 建议采用**三阶段渐进式迁移**，先换引擎、后换外壳：

| 结论要点 | 内容 |
|---|---|
| 代码量 | `src/` 共 **1929 文件 / 513,602 行** TS/TSX |
| 最大障碍 | 其中 **101,405 行（488 文件）是 React/Ink TUI**，Go 生态无等价物，价值最低、成本最高 |
| 最大机会 | 项目**已经内建 headless 控制协议**（`SDKControl*` schema + `--output-format=stream-json` + NDJSON），core 与 TUI 本就是可分离的 |
| 最大风险 | **当前测试文件数为 0**。没有任何回归基线，重写等于盲改 |
| 推荐架构 | Go 二进制承担 **engine / tool / llm / permission / mcp**；TUI 保留 TS，通过 stdio JSON-RPC 通信 |
| 推荐节奏 | Phase 0 建基线 → Phase 1 Go 核心影子运行 → Phase 2 灰度切换 → Phase 3（可选）Go TUI |
| 现实工期 | 达到核心功能等价：**全职约 4–5 个月；业余约 10–12 个月**。TUI 全量替换再加 3–4 个月全职 |
| 首要动作 | 不是写 Go 代码，是**先补回归测试基线** |

**一句话**：这个仓库当前最缺的不是 Go，是测试。先有安全网，再谈重写。

---

## 1. 项目背景与现状说明

### 1.1 项目定位

Ultrac 是一个**终端原生的 AI 编码 Agent**。它读取代码库、执行工具（shell / 文件读写 / 搜索）、调用大模型，以 **think → act → observe** 循环推进任务。技术上是从 Claude Code 的源码布局重建的项目，当前处于 `1.0.0-dev`，API 与 CLI 表面不稳定。

核心能力（README + 代码实测）：

- **Agent 循环**：`src/QueryEngine.ts`（1295 行）、`src/query.ts`（1729 行）
- **工具系统**：45 个工具定义 `inputSchema`，位于 `src/tools/`
- **多模型**：Anthropic 直连、Bedrock、Vertex、Foundry 四家，经 `src/services/api/client.ts` 统一抽象
- **MCP**：客户端 + 服务端 + `mcpb` 包，`src/services/mcp/`
- **TUI**：Ink（React 19）+ `react-reconciler`，vim 键位、对话框
- **Hooks & Skills**：`PreToolUse` / `PostToolUse` 等生命周期钩子；可组合技能系统
- **可观测**：OpenTelemetry trace / metrics / logs
- **多运行形态**：本地 CLI、常驻 daemon、remote/server、self-hosted runner

### 1.2 量化现状（实测数据）

按 `src/` 顶层目录统计（仅 `.ts` / `.tsx`）：

| 目录 | 文件数 | 代码行数 | 占比 | 说明 |
|---|---:|---:|---:|---|
| `utils/` | 566 | 180,494 | 35.1% | 最大块，含权限、会话存储、平台适配 |
| `components/` | 390 | 81,559 | 15.9% | Ink TUI 组件树 |
| `services/` | 132 | 53,696 | 10.5% | api / mcp / lsp / oauth / analytics |
| `tools/` | 190 | 50,980 | 9.9% | 45 个工具实现 + shared + testing |
| `commands/` | 193 | 26,768 | 5.2% | 斜杠命令与内置命令 |
| `ink/` | 98 | 19,846 | 3.9% | 底层 Ink 辅助 |
| `hooks/` | 104 | 19,204 | 3.7% | React hooks + 生命周期钩子 |
| `bridge/` | 31 | 12,613 | 2.5% | REPL bridge / 会话运行器 |
| `cli/` | 23 | 12,369 | 2.4% | 参数解析、结构化 IO |
| `screens/` | 3 | 5,977 | 1.2% | 全屏界面 |
| `native-ts/` | 4 | 4,081 | 0.8% | 原生模块 TS 侧 |
| `skills/` | 20 | 4,066 | 0.8% | 技能加载执行 |
| `entrypoints/` | 11 | 4,064 | 0.8% | cli / init / mcp / sdk |
| `types/` | 23 | 3,775 | 0.7% | 类型契约与 shim |
| `tasks/` | 12 | 3,286 | 0.6% | 任务管理 |
| `keybindings/` | 14 | 3,159 | 0.6% | 键位绑定 |
| `constants/` | 21 | 2,648 | 0.5% | 常量 |
| 其余 20 个目录 | 114 | 24,522 | 4.8% | bootstrap / vim / memdir / migrations 等 |
| **合计** | **1929** | **513,602** | 100% | |

**TUI 相关合计**：`components/` + `ink/` + `screens/` + `keybindings/` + `vim/` ≈ **112,000 行 / 513 文件**，占全仓 **21.8%**。

**核心引擎合计**：`QueryEngine.ts` + `query.ts` + `Tool.ts` + `tools.ts` + `query/` + `tools/` ≈ **55,000 行**，仅占 **10.7%**。

> 这个比例关系是整份计划的地基：**重写成本最高的部分，恰恰是最不值得重写的部分。**

### 1.3 技术栈现状

| 层 | 现状 | Go 侧替代难度 |
|---|---|---|
| 语言 | TypeScript（ES2022，**`strict: false`**），ESM | — |
| 运行时 | Bun | 无（Go 直接编译原生二进制） |
| TUI | Ink + React 19 + `react-reconciler` | **极高**（范式不同，非语法翻译） |
| LLM 客户端 | `@anthropic-ai/sdk`、Bedrock、Vertex、Foundry | 低（有官方 Go SDK） |
| 协议 | MCP SDK、`vscode-jsonrpc` | 低（有官方 Go SDK） |
| 校验 | `zod`（132 文件引用，331 处 `z.object`） | 中（需换 JSON Schema 语义） |
| 遥测 | OpenTelemetry JS | 低（官方 Go SDK 对等） |
| 打包 | `bun build` | 低（`go build` + goreleaser） |

**并发特征实测**：`async` 函数 2371 处、`AbortController` 334 处、`Promise.all`/`allSettled` 285 处。这说明代码是重度异步 + 可取消的，迁移到 Go 的 `context` + `errgroup` 是**语义增益**，不是等价替换。

### 1.4 现状中的关键资产与关键缺口

**已有的高价值资产（重写必须复用，不要重造）：**

| 资产 | 位置 | 为什么重要 |
|---|---|---|
| **控制协议契约** | `entrypoints/sdk/controlSchemas.ts` | 已定义 `SDKControl*` 全套消息（initialize / interrupt / permission / setPermissionMode / setModel / setMaxThinkingTokens / mcpStatus / getContextUsage / rewindFiles / cancelAsyncMessage / seedReadState / hookCallback）。**这就是 core 与 UI 的现成分界线。** |
| **流式 JSON 输出** | `cli/print.ts`（`--output-format=stream-json`）、`cli/ndjsonSafeStringify.ts` | 已有 NDJSON 行协议与安全序列化 |
| **结构化 IO** | `cli/structuredIO.ts`、`cli/remoteIO.ts` | 已有传输层抽象 |
| **VCR 录制回放** | `services/vcr.ts` | 按请求哈希缓存 API 响应，`NODE_ENV=test` 或 `FORCE_VCR` 启用。**可改造成 Go 实现的黄金回归夹具。** |
| **配置加载规范** | `docs/config-loading.md` | 分层解析顺序已有文档化契约 |
| **会话存储布局** | `utils/sessionStorage.ts` | `~/.ultrac/projects/<slug>/*.jsonl`，`MAX_TRANSCRIPT_READ_BYTES = 50MB` |

**关键缺口（必须先补）：**

| 缺口 | 实测 | 后果 |
|---|---|---|
| **测试基线为零** | `*.test.ts` / `*.spec.ts` 数量 = **0** | 没有任何行为等价性校验手段 |
| 类型严格性关闭 | `tsconfig.json` → `"strict": false` | 大量隐式契约只存在于运行时行为里 |
| 上游仍在漂移 | 版本 `1.0.0-dev`，README 明示 API 不稳定 | 重写期间基线会移动 |
| 无接口文档 | 仅 `docs/config-loading.md` + `docs/spec/stubs-registry.md` | 契约靠读代码反推 |

---

## 2. 重写目标与范围界定

### 2.1 重写目标（可度量）

| 编号 | 目标 | 验收指标 |
|---|---|---|
| G1 | **消除运行时依赖** | 交付单一静态二进制，目标机无需 Node/Bun/任何运行时 |
| G2 | **行为等价** | 在 VCR 回放集上，Go 引擎与 TS 引擎的事件流差异为 0（或差异可逐条解释并归档） |
| G3 | **对外契约 100% 兼容** | CLI argv、`settings.json` schema、NDJSON 事件 schema、MCP 版本、`~/.ultrac/` 目录布局、hooks JSON 全部不变 |
| G4 | **并发与性能提升** | 见 §7.6 基准门禁 |
| G5 | **交叉编译分发** | `linux/amd64`、`linux/arm64`、`darwin/arm64`、`windows/amd64` 四个目标产物 |
| G6 | **可灰度可回滚** | 单一环境变量即可在 TS 引擎与 Go 引擎间切换 |

### 2.2 非目标（明确不做）

- 不改动对外 CLI 参数与输出格式（`--print`、`--output-format=stream-json` 等语义冻结）
- 不改动 `settings.json` 字段语义与解析优先级
- 不重写 MCP 协议实现细节（直接采用官方 Go SDK）
- 不追求 TUI 像素级复刻（Phase 3 若启动，按最小可用集实现）
- 不在迁移期引入新的产品功能

### 2.3 范围界定：三个方案对比

| 维度 | 方案 A：全量重写 | **方案 B：Go 核心 + TS 外壳（推荐）** | 方案 C：只重写引擎、永久保留 TS |
|---|---|---|---|
| 重写范围 | 513k 行 | **~90–110k 行** | ~90–110k 行 |
| TUI 处理 | 用 bubbletea 重写 488 文件 | **保留 Ink，通过 RPC 连接** | 保留 Ink |
| 单项最大工作量 | TUI 重写（黑天鹅） | 权限子系统 | 权限子系统 |
| 行为等价风险 | 极高（含 UI 交互） | 中（UI 未动，风险隔离） | 中 |
| 交付形态 | 单二进制 | 二进制 + 既有 TS 包 | 二进制 + 既有 TS 包 |
| 消除运行时依赖 | ✅ 完全 | ⚠️ 部分（TUI 仍需 Bun） | ⚠️ 部分 |
| 可灰度回滚 | ❌ 难 | ✅ 易（env 开关） | ✅ 易 |
| 预估工期 | 12 个月+ 单人不可行 | **全职 4–5 个月 / 业余 10–12 个月** | 同 B，但放弃无运行时依赖目标 |
| 推荐度 | ❌ | **✅ 强烈推荐** | ⏸ 备选 |

**推荐方案 B 的核心理由**：项目的 `SDKControl*` schema 已经定义好了 core 与 UI 之间的完整消息契约（含中断、权限请求、模型切换、MCP 状态、上下文用量、文件回滚）。把 Go 引擎做成这个协议的服务端，TUI 侧只需把现在的进程内调用换成管道调用——**这是改造工程，不是架构重构**。而全量重写要求同时发明一个 Go 版的 React 协调器语义，这在投入产出上说不通。

### 2.4 契约冻结清单（Phase 0 必须完成）

以下契约在重写期间**只读、不可变**。建议在 Phase 0 生成 JSON Schema 快照并纳入版本控制，作为双向回归的比对基线：

| # | 契约 | 来源文件 | 冻结方式 |
|---|---|---|---|
| C1 | CLI 参数与 flag 语义 | `entrypoints/cli.tsx`、`cli/` | 快照 `--help` 输出 + argv 测试用例 |
| C2 | `settings.json` schema 与解析优先级 | `docs/config-loading.md`、`utils/managedEnv.ts` | Schema 快照 + 分层优先级测试 |
| C3 | NDJSON 流式事件协议 | `cli/print.ts`、`cli/ndjsonSafeStringify.ts` | 事件类型枚举 + 样例行 |
| C4 | 控制协议 | `entrypoints/sdk/controlSchemas.ts` | 由 zod schema 导出 JSON Schema |
| C5 | Hooks 输入/输出 JSON | `types/hooks.ts`、`entrypoints/agentSdkTypes.ts` | JSON Schema 快照 |
| C6 | 会话/transcript 存储布局 | `utils/sessionStorage.ts` | 目录结构 + JSONL 行格式测试 |
| C7 | 权限规则与模式 | `utils/permissions/`、`types/permissions.ts` | 规则语法 + 判定结果测试 |
| C8 | Skill `SKILL.md` frontmatter | `skills/` | 解析结果快照 |
| C9 | MCP 协议版本 | `services/mcp/` | 版本协商测试 |

---

## 3. 分阶段实施步骤与时间规划

### 3.0 估算前提（请先接受这个前提）

本计划的所有工期基于以下假设，如不成立请按比例缩放：

- 语义等价移植（非机械翻译）有效产出约 **300 行/天**（含阅读原实现、设计 Go 版、写测试）
- 全职：每周有效编码 **2.5 天**（其余为评审、联调、文档）
- 业余：每周有效编码 **0.8 天**
- 513,602 行 ÷ 300 行/天 ≈ **1,712 人日** → 单人全量重写约 **6.6 人年**

> 这就是为什么范围界定比技术选型更重要。方案 B 的 90–110k 行对应 **300–370 人日**，才是单人可完成的量级。

### 3.1 Phase 0：契约冻结与回归基线

**目标**：建立安全网。**这一步不做完，不允许开始写 Go。**

| 步骤 | 内容 | 产出 |
|---|---|---|
| 0.1 | 锁定基线 commit，打 tag `rewrite-baseline` | 不可变参照点 |
| 0.2 | 导出 C1–C9 契约的 JSON Schema 快照 | `schema/frozen/*.json` |
| 0.3 | **改造 `services/vcr.ts` 为回归夹具**：录制真实会话的 API 请求/响应 + 工具调用序列 | `testdata/vcr/*.jsonl` |
| 0.4 | 建立 CLI 端到端快照测试（`--print` 模式 + 固定 prompt + VCR） | `test/e2e/` 黄金文件集 |
| 0.5 | 补权限子系统判定测试（规则解析、路径校验、危险模式匹配） | `utils/permissions` 覆盖用例 |
| 0.6 | 补齐已知平台陷阱的回归用例（见下方"必测清单"） | 平台测试集 |
| 0.7 | 在 CI 中建立 Go 与 TS 双引擎比对流水线骨架 | CI 配置 |

**必测清单（来自实测代码路径，这些是"配置不生效"类 Bug 的高发区）：**

| 用例 | 对应实现 | 为什么必须测 |
|---|---|---|
| `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 置位时，provider env 是否被从**所有** settings 源剔除 | `utils/managedEnv.ts` | 见 `commands/provider/provider.tsx:216` 的显式警告；这是已知的"配置写了不生效"陷阱 |
| trust 弹窗前：项目级源只应用 `SAFE_ENV_VARS` 白名单，user/flag/policy 源应用全量 env | `utils/managedEnvConstants.ts`、`components/TrustDialog/utils.ts:216` | 顺序错了会导致启动期配置丢失 |
| `applyConfigEnvironmentVariables()` 的调用时机 | `entrypoints/init.ts:269` | env 必须在此之后才进入进程环境 |
| Windows 下 `CLAUDE_CODE_GIT_BASH_PATH` 未显式设置时的行为 | `utils/windowsPaths.ts:98`（`findGitBashPath`，含硬退出） | 实测该函数会走到 `process.exit(1)`，Go 版必须复刻或明确定义为"改进点" |
| `setShellIfWindows()` 与设置应用的先后顺序 | `entrypoints/init.ts:186` vs `:269` | 顺序错会导致 shell 解析行为不一致 |

> **这一节是本计划中投入产出比最高的部分。** 花 2–3 周建基线，能省下后面几个月的"改了不知道对不对"。

### 3.2 Phase 1：Go 核心影子实现

**目标**：Go 引擎能在**不接管**现有流程的前提下独立跑通一条完整链路。

| 步骤 | 内容 | 关键产出 |
|---|---|---|
| 1.1 | 搭骨架：`cmd/ultrac`、`internal/` 分层、配置加载、日志、版本 | 可编译的空壳 |
| 1.2 | 实现 `internal/transport`：NDJSON 编解码 + C4 控制协议服务端 | 能被现有 TS 端连上 |
| 1.3 | 实现 `internal/llm`：`Client` 接口 + Anthropic 直连（流式 SSE） | 单轮对话可流式输出 |
| 1.4 | 实现 `internal/engine`：query 循环、turn 状态机、工具调度 | think→act→observe 闭环 |
| 1.5 | 实现 `internal/tool`：Tool 契约 + 注册表 + **12 个核心工具** | 见下方工具分批表 |
| 1.6 | 实现 `internal/permission`：模式、规则解析、路径校验、危险模式、拒绝追踪 | 与 TS 判定结果逐条比对 |
| 1.7 | 实现 `internal/session`：transcript 读写（JSONL 兼容）+ resume | 可读写现有会话文件 |
| 1.8 | 影子模式：新老引擎并行处理同一请求，比对事件流差异 | 差异报告 |

**Phase 1 工具分批（45 个工具分三批）：**

| 批次 | 工具 | 理由 |
|---|---|---|
| **第 1 批（12 个，Phase 1 必做）** | `FileRead`、`FileWrite`、`FileEdit`、`Glob`、`Grep`、`Bash`、`TodoWrite`、`WebFetch`、`WebSearch`、`Agent`、`Task*` 系列、`AskUserQuestion` | 覆盖 90% 日常使用路径 |
| **第 2 批（~20 个，Phase 2）** | `MCP`、`McpAuth`、`ListMcpResources`、`ReadMcpResource`、`Skill`、`ToolSearch`、`EnterPlanMode`、`ExitPlanMode`、`NotebookEdit`、`LSP`、`Sleep`、`Config`…… | 扩展能力，涉及 MCP / 计划模式 |
| **第 3 批（~13 个，Phase 2 末或延后）** | `PowerShell`、`REPL`、`Workflow`、`Team*`、`RemoteTrigger`、`ScheduleCron`、`SuggestBackgroundPR`、`Tungsten`、`VerifyPlanExecution`、`Brief`、`SyntheticOutput`、`EnterWorktree`/`ExitWorktree` | 平台特定或实验性 |

### 3.3 Phase 2：等价验证与灰度切换

**目标**：Go 引擎成为默认实现，TS 引擎降级为回退路径。

| 步骤 | 内容 |
|---|---|
| 2.1 | 补齐第 2、3 批工具（含 MCP 桥接） |
| 2.2 | 实现 `internal/hook`（生命周期钩子，C5 契约） |
| 2.3 | 实现 `internal/skill`（技能加载执行，C8 契约） |
| 2.4 | 接入 Bedrock / Vertex / Foundry 三家 provider |
| 2.5 | 接入 OpenTelemetry（trace / metric / log） |
| 2.6 | 会话压缩与上下文折叠的等价实现（**高风险项**，见 R11） |
| 2.7 | 灰度：`ULTRAC_ENGINE=ts\|go` 开关，内部默认 `go`，保留 `ts` 回退 |
| 2.8 | 差异清零：影子运行 30 天，差异项逐条归档或修复 |
| 2.9 | 发布门禁：单二进制交叉编译产物 + 安装脚本 |

### 3.4 Phase 3：Go TUI 替换（可选，建议暂缓）

| 步骤 | 内容 | 备注 |
|---|---|---|
| 3.1 | 用 bubbletea v2 复刻最小可用 TUI：流式输出、输入框、工具调用展示、权限对话框 | 目标不是像素级复刻 |
| 3.2 | vim 键位、主题、多行编辑 | 工作量集中区 |
| 3.3 | 逐步替换 `components/` 下高频组件 | 按使用频率排序，而非按目录顺序 |

> **明确建议：Phase 3 暂不启动。** 理由见 R1。若最终目标必须是无运行时依赖，再单独立项评估。

### 3.5 里程碑与验收标准

| 里程碑 | 验收标准 | 卡点性质 |
|---|---|---|
| **M0** 基线就绪 | C1–C9 快照入库；VCR 夹具可回放；CI 跑通 | **硬门禁** |
| **M1** 骨架连通 | Go 引擎可被 TS TUI 连上并完成一次 `initialize` 握手 | 硬门禁 |
| **M2** 单轮对话 | 固定 prompt 下 Go 与 TS 的输出事件流一致 | 硬门禁 |
| **M3** 工具闭环 | 12 个核心工具的调用/结果与 TS 逐条一致 | 硬门禁 |
| **M4** 权限等价 | 权限判定测试集 100% 通过 | 硬门禁 |
| **M5** 影子运行 | 连续 30 天影子模式，差异率 < 0.1% 且全部有归档解释 | **Go/No-Go 决策点** |
| **M6** 默认切换 | `ULTRAC_ENGINE` 默认 `go`，回退路径验证可用 | 硬门禁 |
| **M7** 全量功能 | 第 1–3 批工具 + 3 家 provider + OTel 全部就绪 | 硬门禁 |

### 3.6 三种投入场景的时间表

| 阶段 | 单人全职 | 单人业余 | 双人全职 |
|---|---|---|---|
| Phase 0 基线与契约 | 2–3 周 | 6–8 周 | 1.5 周 |
| Phase 1 Go 核心（含 12 工具） | 8–10 周 | 5–6 个月 | 4–5 周 |
| Phase 2 等价与切换 | 8–10 周 | 5–6 个月 | 4–5 周 |
| **核心可用小计** | **约 4–5 个月** | **约 10–12 个月** | **约 2.5 个月** |
| Phase 3 Go TUI（可选） | +12–16 周 | +8–12 个月 | +6–8 周 |
| **合计（含 TUI）** | **约 8 个月** | **约 1.5–2 年** | **约 4 个月** |

> 时间表按 §3.0 假设推算。**不要把这些当作承诺日期，当作相对投入的标尺。**

---

## 4. 整体架构设计策略

### 4.1 分层结构

Go 侧采用**四层结构**，依赖方向严格自上而下，禁止反向引用：

```
┌─────────────────────────────────────────────────────────────┐
│ L0  接口层 (Transport / Entry)                              │
│     cmd/ultrac · internal/ipc · internal/transport          │
│     职责：CLI 参数解析、NDJSON/JSON-RPC 编解码、stdio/HTTP   │
│     与 TS TUI、SDK 客户端、CI 的对接边界                     │
└───────────────────────────┬─────────────────────────────────┘
                            │ 只依赖 L1 的公开接口
┌───────────────────────────▼─────────────────────────────────┐
│ L1  编排层 (Orchestration)                                  │
│     internal/engine                                         │
│     职责：查询引擎、turn 状态机、工具调度与并发策略、         │
│           会话压缩与上下文折叠、中断与取消                   │
└───────────────────────────┬─────────────────────────────────┘
                            │ 只依赖 L2 的接口（不依赖具体实现）
┌───────────────────────────▼─────────────────────────────────┐
│ L2  能力层 (Capabilities)                                    │
│  internal/llm   internal/tool   internal/permission          │
│  internal/hook  internal/skill  internal/agent  internal/mcp │
│  internal/session                                            │
│  职责：各家模型客户端、工具契约与实现、权限判定、            │
│        钩子、技能、子 agent 委派、MCP 桥接、会话状态         │
└───────────────────────────┬─────────────────────────────────┘
                            │ 只依赖 L3
┌───────────────────────────▼─────────────────────────────────┐
│ L3  基础设施层 (Infrastructure)                              │
│  internal/config   internal/fsx   internal/telemetry          │
│  internal/logging  internal/version  internal/platform        │
│  职责：分层配置解析、文件系统/glob/ignore/原子写/文件锁、      │
│        OTel、结构化日志、平台适配（Windows 路径与 shell）     │
└─────────────────────────────────────────────────────────────┘
```

**分层纪律（必须在 CI 中强制执行）：**

- L2 之间**禁止横向直接引用**。例如 `tool` 不得 import `permission`，权限判定通过编排层注入的回调接口完成（这正好对应 TS 侧 `canUseTool` 参数的语义）。
- L3 不得感知 L1/L2 的存在（例外：`config` 可被所有层依赖，但自身不依赖任何上层）。
- 用 `internal/` 前缀保证外部不可导入；对外稳定 API 单独放在 `api/` 包。

### 4.2 模块划分：TS 目录 → Go 包映射

| TS 位置 | LOC | Go 包 | 处理方式 |
|---|---:|---|---|
| `QueryEngine.ts`、`query.ts`、`query/` | ~3.4k | `internal/engine` | 重写（核心） |
| `Tool.ts`、`tools.ts`、`tools/` | ~51k | `internal/tool` + `internal/tool/builtin/*` | 重写（分批） |
| `services/api/` | ~5k | `internal/llm`（含 `anthropic/`、`bedrock/`、`vertex/`、`foundry/`） | 重写，优先官方 SDK |
| `services/mcp/` | — | `internal/mcp` | 用官方 Go SDK 重写 |
| `services/lsp/` | — | `internal/lsp` | 重写（JSON-RPC 已有 Go 实现） |
| `utils/permissions/`（24 文件） | — | `internal/permission` | **重点重写**，语义等价要求最高 |
| `utils/sessionStorage.ts`、`history.ts` | — | `internal/session` | 重写，保持文件格式兼容 |
| `memdir/` | 1.7k | `internal/session/memory` | 重写 |
| `hooks/`（生命周期部分） | — | `internal/hook` | 重写 |
| `skills/` | 4.1k | `internal/skill` | 重写 |
| `tasks/` | 3.3k | `internal/agent` | 重写 |
| `utils/managedEnv.ts`、`utils/windowsPaths.ts`、`constants/` | — | `internal/config` + `internal/platform` | 重写（陷阱最多） |
| `cli/`、`entrypoints/cli.tsx` | ~12k | `cmd/ultrac` | 部分重写（cobra） |
| `entrypoints/sdk/` | — | `internal/ipc` + `api/` | 契约保持，实现重写 |
| `services/`（analytics / oauth / voice / autoDream 等） | — | — | **延后或不做** |
| `components/`、`ink/`、`screens/`、`keybindings/`、`vim/` | **~112k** | — | **保留 TS** |
| `commands/`（193 个斜杠命令） | 26.8k | — | **保留 TS**（UI 层） |
| `bridge/` | 12.6k | — | 延后至 Phase 3 |

### 4.3 关键技术决策

每条决策给出**推荐 + 理由 + 代价**，不做模棱两可的"都可以"。

#### D1 — 重写范围：不包含 TUI

- **推荐**：TUI 保留 TS/Ink，不进入重写范围（Phase 3 可选且仅做最小集）
- **理由**：TUI 占 21.8% 代码量，是唯一在 Go 生态**没有等价范式**的部分。Ink 是 React 协调器（声明式 diff + 组件树），bubbletea 是 Elm 架构（Model/Update/View + 消息驱动）。这不是语法翻译，是重新设计 488 个文件的状态模型。
- **代价**：无法完全消除 Bun 运行时依赖。若这是硬性目标，需接受工期翻倍。

#### D2 — 进程模型：单二进制 core + stdio JSON-RPC

- **推荐**：Go 引擎作为独立进程，与 TS TUI 通过 **stdio 上的 JSON-RPC 2.0** 通信，复用现有 `SDKControl*` schema
- **理由**：`entrypoints/sdk/controlSchemas.ts` 已定义完整控制消息；`cli/structuredIO.ts` 已有传输抽象；`--output-format=stream-json` 已有 NDJSON 实践。走既有契约是改造，不是发明。
- **代价**：多一次序列化开销（可忽略，NDJSON 吞吐远高于 LLM 流式速率）；需要处理子进程生命周期与僵尸进程回收。

#### D3 — 流式抽象：`async generator` → `iter.Seq2`

- **推荐**：TS 的 `async function* query()` 映射为 Go 的 `iter.Seq2[Event, error]`（Go 1.23+ range-over-func）
- **理由**：这是**语义最接近的一一对应**。TS 生成器天然支持 `for await`、惰性求值与中途 `return`（取消）；Go 的 `iter.Seq2` 配合 `range` 提供同样的惰性拉取语义，且 `err` 作为第二个值避免了"忘了检查"的经典问题。
- **代价**：需要 Go 1.23+；调用方必须用 `range` 而非手写 `Next()` 循环。

#### D4 — 类型与校验：`zod` → Go struct + JSON Schema

- **推荐**：工具输入用 Go struct + `jsonschema` tag 声明，**生成**真 JSON Schema 发给模型；运行时用纯 Go 校验器做入参校验
- **理由**：`zod` 的双重职责是"运行时不变量检查 + 生成 JSON Schema"。Go 侧应拆开：结构体标签负责 schema 生成（`internal/tool` 统一处理），运行时校验用独立的 JSON Schema 校验库。这样工具定义更声明式，也天然满足 MCP 需要真 JSON Schema 的要求。
- **代价**：`zod` 的 `.refine()` / `.transform()` 等自定义逻辑无法靠标签表达，需要为每个工具补一个显式校验函数。实测 331 处 `z.object`，需要逐个评估其中多少带自定义逻辑。

#### D5 — 并发与取消：`AbortController` → `context.Context`

- **推荐**：所有长任务、工具调用、流式读取都以 `context.Context` 为第一参数；`Promise.all` → `errgroup.WithContext`；`Promise.allSettled` → `errgroup` + 收集各任务错误
- **理由**：334 处 `AbortController` 说明项目已重度依赖取消语义。Go 的 `context` 是语言级设施，比手动 `controller.abort()` + `signal` 更不容易漏。goroutine 密度也远高于 event loop 单线程模型，工具批处理天然并行。
- **代价**：**必须严格复刻工具的并发安全语义**——TS 里 `isConcurrencySafe(input)` 决定哪些工具可并行、哪些必须串行。Go 版若简单地全部 `errgroup.Go()`，会破坏有状态工具的顺序性。这是正确性级别的要求，不是优化。

#### D6 — 错误模型：`throw` → `error` 值（高风险）

- **推荐**：定义 sentinel error 集合（`ErrPermissionDenied`、`ErrToolNotAvailable`、`ErrContextOverflow`、`ErrCancelled` 等），用 `errors.Is` / `errors.As` 判定；工具级失败通过 `ToolResult.IsError` 字段表达（不是 Go error），与 TS 语义对齐
- **理由**：TS 用异常 + `try/catch` 做控制流，`catch` 里能拿到 `error.message` 做字符串匹配。Go 没有异常，必须显式建模。
- **代价**：**这是最容易被低估的风险。** 需要逐处审计 TS 的 `try/catch`，判断哪些是"可恢复的业务分支"（→ sentinel error + `errors.Is`），哪些是"真正的异常"（→ 返回 error 并终止该 turn）。建议 Phase 0 就产出一份 `try/catch` 清单。

#### D7 — 状态管理：React state → 显式引擎状态机

- **推荐**：引擎状态定义为单一结构体 + 明确的状态转换函数；所有对 UI 的通知通过事件流单向发出
- **理由**：TS 侧状态分散在 React state、Zustand、模块级变量中（如 `state/AppState.ts`、`bootstrap/state.ts`）。Go 侧必须收敛为一个显式状态机，否则并发下必然出现竞态。
- **代价**：需要一次完整的状态盘点，找出所有隐式共享可变状态。

#### D8 — 配置分层：保持语义，换实现

- **推荐**：用轻量配置库实现 `env → flag → project → user → defaults` 的分层合并，**优先级语义与 `docs/config-loading.md` 逐条对齐**
- **理由**：配置是"改了不生效"类 Bug 的温床。实测代码中有多处微妙规则（项目级源只应用白名单 env、`PROVIDER_MANAGED_BY_HOST` 会剔除所有源的 provider 配置）。这些必须原样复刻，不能"顺手优化"。
- **代价**：必须为每条优先级规则写回归测试（已列入 §3.1 必测清单）。

#### D9 — 会话存储：JSONL 保持为事实源 + SQLite 仅作索引

- **推荐**：transcript 仍以 `~/.ultrac/projects/<slug>/*.jsonl` 为主存储（格式不变）；额外引入纯 Go SQLite 作为**可重建的索引**，用于快速检索与统计
- **理由**：JSONL 是对外契约（C6），换存储会破坏兼容性。索引是可选的加速层，损坏可重建，风险可控。纯 Go SQLite 免 CGO，交叉编译友好。
- **代价**：需处理索引与 JSONL 的一致性（建议索引带内容哈希 + 启动时校验）。

#### D10 — MCP：直接采用官方 Go SDK

- **推荐**：`github.com/modelcontextprotocol/go-sdk`（v1.7.0+，支持 MCP spec 2026-07-28，Apache-2.0）
- **理由**：官方与 Google 共同维护，协议覆盖完整（stdio / HTTP / SSE 传输、OAuth 原语、协议版本自动协商），避免自研 JSON-RPC 帧处理。
- **代价**：SDK 对 Go 版本有要求（新版本仅支持仍在维护的 Go 版本），需确认与项目 Go 版本基线一致。

#### D11 — 遥测：OpenTelemetry Go 官方实现

- **推荐**：`go.opentelemetry.io/otel` + OTLP exporter
- **理由**：与 TS 侧语义一一对应，SDK 名称、span 命名、属性键可直接沿用，保证后端看板无需改动。
- **代价**：需逐项核对属性键名，避免出现两套指标。

#### D12 — 分发：静态二进制 + goreleaser

- **推荐**：`goreleaser` 交叉编译四目标（linux/amd64、linux/arm64、darwin/arm64、windows/amd64），产物为静态链接单文件
- **理由**：这是选 Go 最直接的收益——安装即一个文件，无需运行时。
- **代价**：Windows 下涉及 shell 解析（Git Bash 路径探测）需保留平台分支逻辑。

### 4.4 核心接口设计：Tool 契约的 TS → Go 映射

`Tool` 是整份代码里最关键的契约（45 个工具实现它）。TS 定义约 30 个成员，Go 版应保留全部语义。以下是推荐形态：

```go
package tool

// Tool 是工具契约。In/Out 为具体工具的输入输出类型（值层），
// 而非 interface{}，以便编译期校验与 schema 生成。
type Tool[In any, Out any] interface {
	// —— 标识 ——
	Name() string
	Aliases() []string        // 对应 TS 的 aliases?: string[]
	SearchHint() string       // 对应 TS 的 searchHint?: string，供 ToolSearch 关键词匹配

	// —— 描述与模式 ——
	// 对应 TS 的 description(input, options): Promise<string>
	Description(ctx context.Context, in In, opts DescriptionOptions) (string, error)
	// 对应 TS 的 readonly inputSchema —— Go 侧由 struct tag 生成
	InputSchema() *jsonschema.Schema
	OutputSchema() *jsonschema.Schema // 可选，对应 outputSchema?

	// —— 执行 ——
	// 对应 TS 的 call(args, context, canUseTool, parentMessage, onProgress?): Promise<ToolResult<Output>>
	Call(ctx context.Context, in In, tc *UseContext, canUse CanUseFunc, onProgress ProgressFunc) (*Result[Out], error)

	// —— 语义标注（引擎据此决定调度策略）——
	IsConcurrencySafe(in In) bool                   // 决定能否与其它工具并行
	IsReadOnly(in In) bool
	IsDestructive(in In) bool                       // 默认 false，仅不可逆操作置 true
	InterruptBehavior(in In) InterruptBehavior      // Cancel | Block，默认 Block
	IsSearchOrRead(in In) (search, read bool, list bool)
	IsOpenWorld(in In) bool                         // 访问外部世界（网络等）
	RequiresUserInteraction() bool

	// —— 状态 ——
	IsEnabled() bool

	// —— 类型来源 ——
	IsMCP() bool   // 是否为 MCP 桥接工具
	IsLSP() bool
}
```

对应的辅助类型：

```go
type Result[Out any] struct {
	Data     Out
	IsError  bool      // 关键：工具级失败用此字段，而非 Go error（对齐 TS 语义）
	Metadata map[string]any
}

type InterruptBehavior int
const (
	InterruptBlock InterruptBehavior = iota // 默认：工具继续跑，新消息排队
	InterruptCancel                          // 新消息到达时取消工具并丢弃结果
)

// 对应 TS 的 canUseTool 回调 —— 让 tool 包不直接依赖 permission 包（L2 横向解耦）
type CanUseFunc func(ctx context.Context, name string, input any) (PermissionResult, error)

// 对应 TS 的 onProgress 回调 —— 流式进度上报
type ProgressFunc func(p Progress) 
```

**注册表**（对应 `tools.ts` 的 `Tools` 数组）：

```go
type Registry struct {
	tools  []Tool[any, any]
	byName map[string]Tool[any, any] // 含 alias 索引
}

// 泛型注册：把 Tool[In, Out] 擦除为 Tool[any, any] 并做输入反序列化适配
func Register[In any, Out any](r *Registry, t Tool[In, Out]) 
```

> **设计要点**：`CanUseFunc` 由编排层注入，使 `tool` 包不 import `permission` 包。这既满足 §4.1 的分层纪律，又精确对应 TS 侧 `call()` 第 3 个参数 `canUseTool` 的既有设计——原代码本来就这么解耦了，Go 版应继承而不是破坏。

### 4.5 流式与并发的抽象映射

**流式：TS 生成器 → Go 迭代器**

TS 侧（`query.ts:219`）：

```ts
export async function* query(params: QueryParams) { /* yield 事件 */ }
```

Go 侧推荐形态：

```go
// 事件类型延续 TS 侧语义
type Event struct {
	Type EventType  // message_start / content_block_delta / tool_use / tool_result / ...
	Raw  json.RawMessage
}

// iter.Seq2 提供与 async generator 等价的惰性拉取语义
func (e *Engine) Query(ctx context.Context, params QueryParams) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for {
			select {
			case <-ctx.Done():
				yield(Event{}, ctx.Err())
				return
			default:
			}
			ev, err := e.next(ctx, params)
			if !yield(ev, err) { // 调用方 break → 立即停止
				return
			}
			if err != nil {
				return
			}
		}
	}
}
```

消费侧与 TS 的 `for await (const ev of query(...))` 形态几乎一致：

```go
for ev, err := range engine.Query(ctx, params) {
	if err != nil { break }
	emit(ev)
}
```

**并发：Promise 组合子 → 结构化并发**

| TS 模式 | 出现次数 | Go 对应 |
|---|---:|---|
| `AbortController` + `signal` | 334 | `context.WithCancel` / `WithTimeout` + 首参 `ctx` |
| `Promise.all([...])` | 285（含 allSettled） | `errgroup.WithContext` + `SetLimit(n)` |
| `p-map`（并发映射） | — | `errgroup` + 有界 worker pool |
| `for await ... of` | — | `for ... := range iter.Seq` |
| `try/finally` 清理 | — | `defer` |

**工具并行调度的正确实现（对齐 `isConcurrencySafe` 语义）：**

```go
// 伪代码：必须在保留顺序语义的前提下并发
func (s *Scheduler) RunAll(ctx context.Context, calls []ToolCall) ([]Result[any], error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(s.maxParallel) // 有界并发，避免打爆文件描述符/内存

	var mu sync.Mutex // 非并发安全工具需要串行
	results := make([]Result[any], len(calls))

	for i, c := range calls {
		i, c := i, c
		if c.Tool.IsConcurrencySafe(c.Input) {
			g.Go(func() error { r, err := s.run(ctx, c); results[i] = r; return err })
		} else {
			mu.Lock() // 串行执行，保证副作用顺序
			r, err := s.run(ctx, c)
			mu.Unlock()
			results[i] = r
			if err != nil { return nil, err }
		}
	}
	return results, g.Wait()
}
```

> ⚠️ 上面这段是**语义敏感**的。原 TS 实现里并发安全判定与结果顺序保证是一套组合逻辑（可能与 `interruptBehavior` 联动）。移植时必须先读透 `Tool.ts` 的调度相关部分与 `engine` 的消费逻辑，不要凭直觉写。

---

## 5. Go 依赖与第三方库选型建议

### 5.1 选型原则

1. **标准库优先**：能用 `net/http`、`encoding/json`、`log/slog`、`iter`、`os/exec` 解决的不引第三方
2. **官方 SDK 优先**：LLM 与协议类走厂商官方 Go SDK，避免协议漂移
3. **纯 Go 优先**：避免 CGO（交叉编译与静态链接的硬要求）
4. **维护活跃度**：优先近 6 个月有提交、有 CI、有测试的项目
5. **依赖数量纪律**：每引入一个库需在 PR 中写明"为什么标准库不够"

### 5.2 主选型表（含推荐理由）

#### 5.2.1 CLI 与 TUI

| 领域 | **推荐** | 备选 | 推荐理由 |
|---|---|---|---|
| CLI 框架 | `github.com/spf13/cobra` | `urfave/cli/v3` | 生态最大、子命令组织能力强，可直接对应 193 个命令的层级结构；与 Charm 生态的 fang 包装器兼容，可自动生成 man page |
| TUI 框架 | `charm.land/bubbletea/v2` | `rivo/tview`、`jroimartin/gocui` | Go TUI 事实标准（38k+ stars）。2026-02 发布 v2，`View()` 改为返回 `tea.View`、终端特性声明式化、引入 Cursed Renderer（远程渲染带宽显著降低）、内置 colorprofile 降采样。**注意 v2 的 import path 已迁到 `charm.land/` 虚拟域，网上多数教程是旧的 `github.com/charmbracelet/` 路径** |
| TUI 样式 | `charm.land/lipgloss/v2` | — | 与 bubbletea v2 配套；v2 移除了 `AdaptiveColor`，明暗主题需从 `tea.BackgroundColorMsg` 显式选择 |
| TUI 组件 | `charm.land/bubbles/v2` | — | 现成的 input / list / table / viewport / spinner / progress；v2 把导出字段改为 getter/setter 方法 |
| 表单 | `charm.land/huh/v2` | — | 对应 TS 侧的权限对话框、配置向导类交互 |
| Markdown 渲染 | `charm.land/glamour/v2` | `yuin/goldmark` | 终端渲染 Markdown，替代 TS 侧 `marked` 的展示路径；v2 支持超链接 |
| 终端宽度 | `github.com/mattn/go-runewidth` | — | **必须**：East Asian Width 计算，中文对齐的唯一正解（TS 侧用 `get-east-asian-width` + `string-width`） |
| ANSI 处理 | `github.com/charmbracelet/x/ansi` | `github.com/muesli/ansi` | 替代 `strip-ansi` / `wrap-ansi`，处理 SGR 序列与宽度截断 |

#### 5.2.2 LLM 与协议

| 领域 | **推荐** | 备选 | 推荐理由 |
|---|---|---|---|
| Anthropic | `github.com/anthropics/anthropic-sdk-go` | 自研 SSE 客户端 | 官方维护，流式迭代器、tool use、`BetaToolRunner` 一应俱全；上下文取消用 `context` 原生表达。要求 Go 1.24+ |
| AWS Bedrock | `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` | — | 官方 v2 SDK，替代 `@aws-sdk/client-bedrock-runtime` |
| Google Vertex | `cloud.google.com/go/vertexai` 或 anthropic-sdk-go 的 Vertex 选项 | — | 官方；`google-auth-library` 对应 `golang.org/x/oauth2/google` |
| Azure/Foundry | `github.com/Azure/azure-sdk-for-go/sdk/azidentity` + 自定义 HTTP 客户端 | — | Foundry 为托管端点，可用 Anthropic 客户端 + 自定义 base URL 覆写实现，不必引入独立 SDK |
| MCP | `github.com/modelcontextprotocol/go-sdk`（v1.7.0+） | `github.com/mark3labs/mcp-go` | **官方 + Google 共同维护**。支持 MCP spec 2026-07-28（含 2025-11-25 / 2025-06-18 等旧版本自动协商）；提供 `mcp` / `jsonrpc` / `auth` / `oauthex` 四个包；`mcp.AddTool` 可从 struct tag 自动派生 JSON Schema |
| JSON-RPC | MCP SDK 内置的 `jsonrpc` 包 | `github.com/sourcegraph/jsonrpc2` | 已随 MCP SDK 提供，避免重复引入 |
| WebSocket | `github.com/coder/websocket` | `github.com/gorilla/websocket` | `coder/websocket`（原 nhooyr）API 更现代、零依赖、默认 context 化；对应 TS 侧 `ws` |
| LSP | `github.com/sourcegraph/jsonrpc2` + `github.com/go-language-server/protocol` | — | LSP 本质是 JSON-RPC，替代 `vscode-languageserver-protocol` |

> ⚠️ **重要提醒**：官方文档明确指出 **"Agent SDK is not yet available for Go"**。本项目实测仅在 `cli/print.ts:130` 以 `import type` 引用了 `@anthropic-ai/claude-agent-sdk` 的 `PermissionMode` 类型（**纯类型引用，无运行时依赖**），因此**不构成阻塞**。但若后续要复用 Agent SDK 的高层封装能力，Go 侧需自行实现——请在方案评审时确认这一点。

#### 5.2.3 数据、校验与 IO

| 领域 | **推荐** | 备选 | 推荐理由 |
|---|---|---|---|
| JSON Schema 生成 | `github.com/invopop/jsonschema` | `github.com/google/jsonschema-go` | 从 Go struct tag 生成 draft 2020-12 Schema；Anthropic 官方 Go 文档中的 tool 示例即用它 |
| JSON Schema 校验 | `github.com/santhosh-tekuri/jsonschema/v6` | — | 严格实现 draft 2020-12，支持 `$ref` 与 format 断言；替代 `zod` 的运行时校验职责 |
| 配置解析 | `github.com/knadh/koanf/v2` | `github.com/spf13/viper` | 轻量、无全局单例、多源 merge 语义显式可控。**配置优先级是本项目的高危区，显式的 merge 比 viper 的隐式行为更容易做等价性验证** |
| 结构化日志 | 标准库 `log/slog` + `github.com/lmittmann/tint` | `go.uber.org/zap`、`github.com/rs/zerolog` | 标准库足够，零依赖；tint 提供开发期着色输出 |
| 遥测 | `go.opentelemetry.io/otel` + `.../exporters/otlp/otlptrace/otlptracehttp` | — | 官方，与 TS 侧属性键可逐一对齐 |
| SQLite | `modernc.org/sqlite` | `github.com/mattn/go-sqlite3` | **纯 Go 实现，免 CGO**——交叉编译与静态链接的关键前提 |
| 目录规范 | `github.com/adrg/xdg` | — | 替代 `env-paths`；但 `~/.ultrac/` 是既有契约，此库仅用于辅助定位 |
| 文件监听 | `github.com/fsnotify/fsnotify` | — | 事实上标准，替代 `chokidar` |
| 文件锁 | `github.com/gofrs/flock` | — | 替代 `proper-lockfile`，跨平台文件锁 |
| 原子写 | 标准库 `os.Rename` + 自实现 tmpfile 流程 | `github.com/google/renameio/v2` | 会话写入与配置文件更新都需要原子性 |

#### 5.2.4 文本、搜索与匹配

| 领域 | **推荐** | 备选 | 推荐理由 |
|---|---|---|---|
| Glob 匹配 | `github.com/bmatcuk/doublestar/v4` | `github.com/gobwas/glob` | 支持 `**` 递归通配，语义与 `picomatch` 最接近 |
| gitignore 语义 | `github.com/sabhiram/go-gitignore` | 直接调用 `rg` 子进程 | **建议优先保留调用 `rg` 的路径**：`.gitignore` 语义复杂（嵌套、否定规则、global excludes），自研极易产生行为差异。若要纯 Go，必须用 §3.1 的测试集逐条对比 |
| Shell 解析 | `github.com/mvdan/sh` | `github.com/google/shlex` | **必须用 mvdan/sh**：`BashTool` 需要真实 bash AST 来判定安全性（TS 侧用 `shell-quote`），`shlex` 只做分词，撑不住权限判定 |
| 文本差异 | `github.com/sergi/go-diff`（Myers 算法） | `github.com/aymanbagabas/go-udiff` | 替代 `diff` v9；`FileEdit` 的 diff 展示依赖它 |
| 语法高亮 | `github.com/alecthomas/chroma/v2` | — | 替代 `highlight.js`，纯 Go |
| 模糊搜索 | `github.com/sahilm/fuzzy` | `github.com/lithammer/fuzzysearch` | 替代 `fuse.js`，用于 ToolSearch 与命令补全 |
| 行内 diff 着色 | `github.com/aymanbagabas/go-udiff` | — | 替代 `color-diff-napi`（**原生模块**，Go 侧无需原生依赖即可实现） |

#### 5.2.5 基础设施与工具链

| 领域 | **推荐** | 备选 | 推荐理由 |
|---|---|---|---|
| 结构化并发 | `golang.org/x/sync/errgroup` + `singleflight` | — | 官方扩展库，替代 `p-map` 与去重场景 |
| 重试退避 | `github.com/cenkalti/backoff/v5` | — | 替代 `withRetry.ts`，支持 context 取消 |
| SSE 客户端 | `github.com/tmaxmax/go-sse` | 自研 `bufio.Scanner` | Anthropic 流式为 SSE；若官方 SDK 已封装则无需单独引入 |
| JWT | `github.com/golang-jwt/jwt/v5` | — | 替代 `jwtUtils.ts`（bridge 鉴权用） |
| 测试断言 | `github.com/stretchr/testify` | — | 事实标准；配合标准库 `testing` |
| 深比较 | `github.com/google/go-cmp` | — | 黄金文件比对与结构体差异报告，比 testify 的 Equal 输出更可读 |
| Mock | `go.uber.org/mock`（原 golang/mock） | — | 为 LLM Client 与权限接口生成 mock |
| 黄金文件 | 标准库 `testing` + `-update` flag 惯例 | `github.com/gkampitakis/go-snaps` | 契约回归的核心手段 |
| 版本信息 | `runtime/debug.ReadBuildInfo()` | — | 注入构建元信息 |
| 发布 | `goreleaser` | 手写 Makefile | 交叉编译矩阵、checksum、Homebrew tap 一次配置 |
| 静态检查 | `golangci-lint` | — | 与 CI 门禁结合；权限/并发模块建议加 `-race` |
| Go 版本 | **1.24+**（Anthropic SDK 要求）；若用 `iter.Seq` 需 1.23+；MCP 新版本对 Go 版本有跟随要求 | | 建议直接锁定官方仍维护的最新稳定版 |

### 5.3 明确放弃或需要特别说明的选型

| 候选 | 决定 | 理由 |
|---|---|---|
| `tview` / `gocui` | 放弃 | 命令式渲染，无法承载本项目复杂的状态驱动 UI；bubbletea 的 Elm 架构与现有组件思维更接近 |
| `mattn/go-sqlite3` | 放弃 | 需要 CGO，破坏静态链接与交叉编译 |
| `viper` | 降为备选 | 全局状态与隐式优先级不利于配置等价性验证 |
| 自研 MCP 实现 | 放弃 | 官方 SDK 已覆盖协议协商与 OAuth，自研是负收益 |
| 自研 SSE 解析 | 降为备选 | 优先看官方客户端是否已封装流式；如无，`bufio.Reader` 自研也不难，但要注意 chunk 边界 |
| 自研权限分类器 | **延后评估** | `bashClassifier` / `yoloClassifier` 的实现性质需先读透（可能依赖模型判断）。若依赖模型，须保持调用契约一致，不能简单用规则替代 |
| `gorilla/websocket` | 降为备选 | 维护活跃度不及 `coder/websocket` |

### 5.4 依赖清单总量预估

核心引擎预计引入 **25–30 个直接依赖**（含官方 SDK）。作为对比，当前 `package.json` 有 **71 个直接依赖**——Go 侧依赖数会显著下降，主要因为标准库覆盖了 TS 侧需要外部库的部分能力（迭代器、结构化并发、日志、HTTP/2、TLS）。

---

## 6. 数据访问层与对外接口设计

### 6.1 数据分类与存储策略

本项目的"数据"不是传统业务库，而是**会话状态与配置**。分四类处理：

| 类别 | 现状位置 | 读写特征 | Go 侧策略 |
|---|---|---|---|
| **A. 会话 transcript** | `~/.ultrac/projects/<slug>/*.jsonl` | 追加写、顺序读、可达 50MB | 保持 JSONL 追加写为事实源；流式读（不整文件加载） |
| **B. 用户/项目配置** | `~/.ultrac/settings.json`、`~/.ultrac.json`、`<cwd>/.ultrac/` | 启动读、偶发写 | 分层解析 + 原子写；schema 冻结（C2） |
| **C. 记忆与规则文件** | `~/.ultrac/projects/<slug>/memory/`、`.ultrac/rules/*.md`、`CLAUDE.md` | 读多写少 | 直接文件读写，保持 Markdown 格式 |
| **D. 索引与统计** | 无（现为全量扫描 JSONL） | 只读分析 | **新增** 纯 Go SQLite 索引，可重建 |

### 6.2 会话存储设计

**主存储：JSONL（格式保持不变，C6 契约）**

```
~/.ultrac/projects/<slug>/
├── <session-id>.jsonl          # 会话 transcript（每行一条 Entry JSON）
├── memory/                     # 项目记忆
│   └── *.md
└── agents/                     # 子 agent 的 transcript
    └── <agent-id>.jsonl
```

**Go 侧实现要点：**

```go
type Store struct {
	dir string
	mu  sync.Mutex      // 追加写的进程内串行
	lk  *flock.Flock    // 跨进程锁（多实例并发访问同一项目）
}

// 追加写：单次 write + 换行，保证行原子性
// 关键：不要用 ioutil.ReadAll 读大文件 —— 常量 MAX_TRANSCRIPT_READ_BYTES = 50MB 存在是有原因的
func (s *Store) Append(ctx context.Context, e Entry) error

// 流式读：bufio.Scanner + 增大初始缓冲（单行可能很大，含文件内容）
func (s *Store) Stream(ctx context.Context, sessionID string) iter.Seq2[Entry, error]

// 尾部读取（resume 场景）：从文件末尾反向扫描定位最近 N 条
func (s *Store) Tail(ctx context.Context, sessionID string, n int) ([]Entry, error)
```

**索引层（新增，纯 Go SQLite）：**

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  project     TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  turn_count  INTEGER NOT NULL DEFAULT 0,
  content_hash TEXT NOT NULL            -- 用于与 JSONL 校验一致性
);
CREATE INDEX IF NOT EXISTS idx_sessions_project_updated
  ON sessions(project, updated_at DESC);
```

**一致性策略**：

1. JSONL 是唯一事实源，索引是派生数据
2. 每次追加后异步更新索引（失败仅记录日志，不影响主流程）
3. 启动时抽样校验 `content_hash`；不一致则**丢弃索引重建**，绝不反向修 JSONL
4. 索引用 `modernc.org/sqlite`（纯 Go），文件放 `~/.ultrac/index.db`

### 6.3 配置解析层设计

**必须精确复刻的语义（来自 `docs/config-loading.md` 与实测代码）：**

| 规则 | 实现位置 | Go 侧要求 |
|---|---|---|
| 来源优先级 | 分层解析 | env → flag → project → user → defaults，顺序不可变 |
| 项目级源的 env 白名单 | `utils/managedEnvConstants.ts` 的 `SAFE_ENV_VARS` | trust 弹窗前，项目级源**只**应用白名单内的 env |
| provider 配置剔除开关 | `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` | 置位时，provider 相关 env（BASE_URL / AUTH_TOKEN / API_KEY / MODEL / SMALL_FAST_MODEL）从**所有**源剔除 |
| 设置缓存失效 | `updateSettingsForSource` 内建 | 写盘后必须让缓存失效，否则读到的还是旧值 |
| env 应用时机 | `entrypoints/init.ts:269` 调用 `applyConfigEnvironmentVariables()` | env 必须在该点之后才进入进程环境 |
| shell 初始化顺序 | `entrypoints/init.ts:186` 的 `setShellIfWindows()` | 与上面那点的相对顺序要一致 |
| 字段留空语义 | `/provider` 命令 | 留空 = 从 settings.json 删除该 key，**不是**写入空字符串 |

**Go 侧形态：**

```go
package config

type Source int
const (
	SourceDefault Source = iota
	SourceUser
	SourceProject
	SourceFlag
	SourceEnv
)

// Resolver 显式表达优先级，便于测试每个来源的贡献
type Resolver struct {
	sources []Layer            // 按优先级升序
	mu      sync.RWMutex
	cache   map[string]*Settings
}

func (r *Resolver) Get(ctx context.Context, cwd string) (*Settings, error)
func (r *Resolver) Update(ctx context.Context, src Source, mutate func(*Settings) error) error
func (r *Resolver) Invalidate(source Source)  // 对应 TS 的 resetSettingsCache()
```

> 这一层的验收标准很简单：**§3.1 必测清单全部通过**。做不到就是 bug 复现，不是"设计不同"。

### 6.4 对外接口 A：CLI

- 参数与 flag 语义**完全冻结**（C1）
- 实现用 cobra，但**子命令树与参数名从现有实现反向导出**，不要凭 README 重设计
- 验收：对固定 argv 集比对 `--help` 输出与退出码

### 6.5 对外接口 B：控制协议（核心接口）

这是 Go 引擎与 TS TUI/SDK 之间的**唯一通道**，也是方案 B 成立的基础。

**协议选型：stdio 上的 JSON-RPC 2.0 + 行分隔（NDJSON）**

理由：与现有 `cli/structuredIO.ts`、`--output-format=stream-json` 实践一致；行分隔便于日志与人工排查。

**消息类别（沿用 `entrypoints/sdk/controlSchemas.ts`）：**

| 方向 | 消息 | 对应 schema |
|---|---|---|
| Client → Core | 初始化 | `SDKControlInitializeRequestSchema` |
| Client → Core | 中断当前回合 | `SDKControlInterruptRequestSchema` |
| Core → Client | 权限请求 | `SDKControlPermissionRequestSchema` |
| Client → Core | 切换权限模式 | `SDKControlSetPermissionModeRequestSchema` |
| Client → Core | 切换模型 | `SDKControlSetModelRequestSchema` |
| Client → Core | 设置最大思考 token | `SDKControlSetMaxThinkingTokensRequestSchema` |
| Client → Core | 查询 MCP 状态 | `SDKControlMcpStatusRequestSchema` |
| Client → Core | 查询上下文用量 | `SDKControlGetContextUsageRequestSchema` |
| Client → Core | 文件回滚 | `SDKControlRewindFilesRequestSchema` |
| Client → Core | 取消异步消息 | `SDKControlCancelAsyncMessageRequestSchema` |
| Client → Core | 预置读取状态 | `SDKControlSeedReadStateRequestSchema` |
| Core → Client | 钩子回调 | `SDKHookCallbackRequestSchema` |
| Core → Client | 流式事件 | NDJSON 事件（C3） |

**实现结构：**

```go
package ipc

type Server struct {
	engine Engine
	perm   PermissionBroker
	w      *bufio.Writer
	r      *bufio.Scanner
}

// 双向通道：Core 主动向 Client 发权限请求/钩子回调，等响应
type PermissionBroker struct {
	pending map[string]chan PermissionResult  // requestID → 响应通道
	mu      sync.Mutex
}

func (s *Server) Serve(ctx context.Context) error
```

**顺带解决的额外收益**：

1. 调试友好——所有交互可录制为 NDJSON，直接成为回归夹具
2. 天然支持 headless / CI 场景（不需要 TUI）
3. remote / daemon 形态可以在不改引擎的前提下，把 stdio 换成 socket / HTTP

### 6.6 对外接口 C：MCP Server

- 使用官方 Go SDK 的 `mcp.NewServer` + stdio transport
- 暴露的工具集由 `internal/tool` 注册表驱动，与内置工具共用同一份定义
- 验收：用现有 MCP 客户端连接到 Go 版 server，`tools/list` 输出与 TS 版一致

### 6.7 对外接口 D：Hooks IPC

- Hook 契约冻结（C5）：输入为 JSON（事件名、工具名、参数、上下文），输出为 JSON（allow / deny / ask + 修改后的参数）
- Go 侧通过 `os/exec` 调用用户配置的外部命令，stdin 写 JSON、stdout 读 JSON
- **注意**：TS 侧 hook 执行涉及 Git Bash 路径探测（`utils/hooks.ts:976` 调 `findGitBashPath`），Windows 下的行为需要专项测试

### 6.8 兼容性与迁移策略

| 项 | 策略 |
|---|---|
| 会话文件 | 双向兼容：Go 能读 TS 写的历史会话，TS 也能读 Go 写的 |
| 配置文件 | 完全兼容，不新增字段；新增字段须可选且有默认值 |
| 事件协议 | 版本化：`initialize` 握手中协商协议版本，老客户端连新引擎时降级 |
| 回滚 | `ULTRAC_ENGINE=ts` 环境变量切回原实现，两套实现共存至少一个大版本 |
| 数据迁移 | 无需迁移脚本（存储格式不变）；SQLite 索引可随时删除重建 |

---

## 7. 性能与并发优化要点

### 7.1 并发模型

| 要点 | 做法 |
|---|---|
| 取消传播 | 所有长任务首参 `ctx context.Context`，把现有 334 处 `AbortController` 语义收敛到一处 |
| 有界并发 | `errgroup.SetLimit(n)`，禁止无界 `go func()`（504 处工具的并行执行必须有上界） |
| 工具调度 | 严格区分 `isConcurrencySafe`：安全工具并行，不安全工具串行 + 保持副作用顺序（见 §4.5） |
| 防 goroutine 泄漏 | 所有 `go` 语句必须有退出路径；CI 加 `-race` 与 `goleak` 检测 |
| 共享状态 | 用 `sync.RWMutex` 或 channel 收敛；禁止散落的包级可变变量 |
| 去重 | 并发相同的搜索/读取用 `singleflight` 合并 |

### 7.2 流式管线

| 要点 | 做法 |
|---|---|
| SSE 解析 | `bufio.Reader` + 增量解析，**避免整块 accumulate 再 split** |
| 事件转发 | 从模型 → 引擎 → IPC → UI 全程流式，任一层不得缓冲全量 |
| 背压 | channel 带容量 + `select { case ch <- ev: case <-ctx.Done(): }`，UI 慢不阻塞引擎取消 |
| Token 估算 | 增量估算（每 chunk 累加），不要每次重算整个上下文 |
| 大输出截断 | 在流式过程中截断，不要先收全再判断 |
| 缓冲复用 | `sync.Pool` 复用 JSON 编解码的 `[]byte` 缓冲 |

### 7.3 文件 IO 与搜索

| 要点 | 做法 |
|---|---|
| Grep / Glob | 优先仍走 `rg` 子进程（**行为等价优先于性能**），`gitignore` 语义自研风险高 |
| 自研遍历 | 若走纯 Go：有界 worker pool（`runtime.NumCPU()`）+ `fsnotify` 缓存，注意 `.gitignore` 嵌套语义 |
| 大文件读 | 流式 + 分页（`FileRead` 已有 offset/limit 语义）；禁止 `os.ReadFile` 读未知大小文件 |
| Transcript 读 | 流式扫描 + 尾部反向读；`MAX_TRANSCRIPT_READ_BYTES = 50MB` 是既有上界，不要突破 |
| 原子写 | 临时文件 + `os.Rename`；会话追加写用 `O_APPEND` 保证行原子 |
| 并发锁 | 跨进程用 `flock`，进程内用 `sync.Mutex`；避免多实例同时写同一 transcript |

### 7.4 内存

| 要点 | 做法 |
|---|---|
| 内存上限 | 设 `GOMEMLIMIT`（建议为容器/机器内存的 70–80%），避免 GC 抖动导致 OOM |
| GC 参数 | 默认 `GOGC` 起步，压测后按 P99 延迟调优 |
| 大对象 | 工具输出、文件内容用切片 + 显式释放引用（Go 无手动 free，靠作用域 + 置 nil） |
| 避免复制 | 事件在管道内传引用而非深拷贝；JSON 反序列化直接到目标结构 |

### 7.5 启动与分发

| 要点 | 现状（Bun） | Go 目标 | 说明 |
|---|---|---|---|
| 冷启动 | 需加载运行时 + 解析模块 | **< 20ms** | 静态二进制，无运行时初始化 |
| 产物数量 | 需 Bun/Node + 依赖树 | **单文件** | 用户体验最直观的收益 |
| 交叉编译 | 需目标平台运行时 | `GOOS`/`GOARCH` 一次构建 | 配合 goreleaser |
| 二进制体积 | — | 目标 < 40MB（`-ldflags="-s -w"`） | 若超限，考虑剥离调试信息与裁剪依赖 |

### 7.6 基准与门禁（必须量化，否则优化是空话）

**基准集设计：**

| 基准 | 度量 | 目标 |
|---|---|---|
| B1 冷启动 | 进程启动到首屏可交互 | Go < 20ms（对比 Bun 基线，记录实测值） |
| B2 单轮流式首 token | 发出请求到收到首个 delta | 与 TS 持平（网络主导，允许 ±10%） |
| B3 工具批处理 | 20 个独立 `FileRead` 的墙钟时间 | **快于 TS ≥ 2×**（并发执行的核心收益） |
| B4 Grep 大仓 | 在 10 万文件仓库上执行 Grep | 与 `rg` 基线持平 |
| B5 Transcript 读 | 读取 50MB transcript 的峰值内存 | < 100MB（流式读，不整块加载） |
| B6 常驻内存 | 空闲 5 分钟后的 RSS | < 80MB |
| B7 会话恢复 | resume 一个 1000 轮会话 | < 500ms |

**门禁规则**：

1. 每个基准在 CI 中固定运行，结果归档
2. 任一基准比基线退化 > 15% → **阻塞合并**
3. 每两周输出一次基准趋势报告

---

## 8. 潜在风险与应对方案

### 8.1 风险登记表

| ID | 风险 | 概率 | 影响 | 等级 | 缓解措施 | 触发信号 |
|---|---|---|---|---|---|---|
| **R1** | **TUI 重写失控**：488 文件 / 112k 行 React 需重新设计为 Elm 架构，工期不可控 | 高 | 极高 | 🔴 | **移出范围**；Phase 3 单独立项，仅做最小可用集，且必须在核心切换完成后才评估 | Phase 1 工期超预期 30% |
| **R2** | **零测试基线**：当前测试文件数为 0，重写无安全网 | 已发生 | 极高 | 🔴 | **Phase 0 强制补基线**；VCR 夹具 + 契约快照 + 黄金文件；M0 未达成不允许进入 Phase 1 | 出现"改了不知道对不对"的情况 |
| **R3** | **行为等价性缺失**：权限判定、上下文压缩、中断语义、分类器行为漂移 | 高 | 极高 | 🔴 | 影子模式双跑 + 事件流差分；差异逐条归档；权限测试集 100% 通过作为 M4 门禁 | 影子运行差异率 > 0.1% |
| **R4** | **错误语义漂移**：TS `throw`/`try-catch` 控制流难以映射为 Go error 值 | 高 | 高 | 🟠 | Phase 0 产出 `try/catch` 清单，逐处分类为"业务分支（sentinel error）"或"异常（返回 error）"；工具级失败用 `Result.IsError` 而非 error | 出现错误被静默吞掉或提前终止 turn |
| **R5** | **权限子系统复杂度被低估**：`utils/permissions/` 24 文件，含 `bashClassifier`、`yoloClassifier`、`dangerousPatterns`、`shadowedRuleDetection` | 中 | 高 | 🟠 | 逐文件读透后再动手；先确认分类器是否依赖模型调用；先建判定测试集再实现 | 权限用例通过率卡在 80–90% |
| **R6** | **生态缺口**：Agent SDK 无 Go 版 | 低 | 中 | 🟢 | 已核实仅 `cli/print.ts:130` 类型引用，无运行时依赖 → **不阻塞** | — |
| **R7** | **原生/平台模块差异**：`color-diff-napi`、图像处理、语音 STT、computer-use 适配 | 中 | 中 | 🟡 | 原生模块在 Go 侧反而可去除（纯 Go 实现）；语音/computer-use 等能力**延后或不做** | 相关工具迁移时 |
| **R8** | **单人产能 vs 513k 行**：全量重写需约 6.6 人年 | 已发生 | 极高 | 🔴 | **严格范围裁剪**（方案 B 降至 90–110k 行）；分批交付；不追求一次对齐 | 任何阶段延期 > 50% |
| **R9** | **`strict: false` 的隐式契约**：类型系统未捕获的运行时假设 | 中高 | 中高 | 🟠 | Go 强制类型会暴露这些假设；每个暴露点都要对照原实现确认是"原代码 bug"还是"新代码误解" | 移植时频繁出现"这里 TS 为什么能跑" |
| **R10** | **上游漂移**：项目仍在活跃迭代（`1.0.0-dev`），基线会移动 | 高 | 高 | 🟠 | 冻结基线 commit + tag；每两周评估一次上游变更，只 cherry-pick 契约相关改动；重写分支与上游保持 rebase 节奏 | 上游改动触及 C1–C9 任一契约 |
| **R11** | **上下文压缩算法不等价**：compact / contextCollapse 逻辑复杂，等价性难验证 | 高 | 高 | 🔴 | 用长会话 VCR 回放对比压缩后的上下文；压缩结果允许不同，但**必须验证后续对话质量不退化** | 长会话下模型答非所问 |
| **R12** | **双运行时发布复杂度**：Go 二进制 + TS TUI 需协同发版 | 中 | 中 | 🟡 | 协议版本协商 + 兼容性矩阵；引擎与 TUI 可独立升级 | 用户升级后协议不匹配 |

### 8.2 Go/No-Go 决策门

| 门 | 位置 | 判断标准 | 不通过的后果 |
|---|---|---|---|
| **Gate 0** | Phase 0 结束 | C1–C9 契约快照入库；VCR 夹具可回放；CI 双引擎比对跑通 | **停止**，继续补基线。不允许带着无测试状态进入重写 |
| **Gate 1** | Phase 1 中段（单轮对话跑通） | Go 与 TS 在固定 prompt 下事件流一致 | 评估是否调整架构（如改为更小的重写范围） |
| **Gate 2** | Phase 1 结束 | 12 个核心工具 + 权限判定全通过 | 砍掉第 2、3 批工具，先交付最小可用引擎 |
| **Gate 3** | Phase 2 中段（影子运行 30 天） | 差异率 < 0.1% 且全部可解释 | **重新评估整个重写计划的必要性** |
| **Gate 4** | Phase 2 结束 | 切换默认引擎后 30 天无 P0 故障 | 回退，TS 引擎继续作为默认 |

> **Gate 3 是最重要的门。** 如果影子运行显示差异难以收敛，说明这个重写的收益不抵成本——**及时止损比坚持计划更专业**。

### 8.3 回滚策略

| 层级 | 回滚手段 | 恢复时间 |
|---|---|---|
| L1 引擎切换 | `ULTRAC_ENGINE=ts` 环境变量，立即回到原实现 | 秒级 |
| L2 版本回退 | 保留上一个 TS 版本的可执行包，二进制品直接替换 | 分钟级 |
| L3 数据回滚 | **不需要**：存储格式不变，无迁移脚本，SQLite 索引可删除重建 | 即时 |
| L4 分支回滚 | 重写工作完全在独立分支/目录，主分支不受影响 | 即时 |

**关键设计**：**双实现共存至少一个大版本**。两套引擎都保留在仓库中，共享同一份契约快照作为测试依据。这既是回滚保障，也是回归测试的对照组。

---

## 9. 附录

### 9.1 TS → Go 概念映射速查

| TypeScript / JS | Go | 备注 |
|---|---|---|
| `async function` | `func` + goroutine | — |
| `Promise<T>` | `(T, error)` | 显式错误 |
| `Promise.all` | `errgroup.WithContext` | 首个错误即取消 |
| `Promise.allSettled` | `errgroup` + per-task 错误收集 | — |
| `async function*` | `iter.Seq2[T, error]` | Go 1.23+ |
| `AbortController` | `context.Context` | — |
| `try/catch/finally` | `error` 值 + `defer` | **语义差异最大** |
| `zod` schema | struct + `jsonschema` tag → JSON Schema | 生成与校验职责拆开 |
| `Record<string, T>` | `map[string]T` | — |
| `T \| undefined` | `*T` 或 `(T, bool)` | — |
| `interface`（结构化） | `interface`（名义） | Go 接口更显式 |
| `import type` | 无需（无运行时代价） | — |
| `env-paths` | `adrg/xdg` | `~/.ultrac/` 仍为契约 |
| 闭包捕获 | 闭包 + 注意循环变量捕获 | Go 1.22+ 已修正 loopvar |

### 9.2 Phase 0 交付物 checklist

- [ ] 基线 tag 已打（`rewrite-baseline`）
- [ ] C1 CLI 参数契约快照
- [ ] C2 `settings.json` schema + 优先级测试
- [ ] C3 NDJSON 事件协议定义
- [ ] C4 控制协议 JSON Schema（由 `controlSchemas.ts` 导出）
- [ ] C5 Hooks JSON Schema
- [ ] C6 会话存储布局测试
- [ ] C7 权限规则与判定测试集
- [ ] C8 Skill frontmatter 解析快照
- [ ] C9 MCP 版本协商测试
- [ ] VCR 夹具改造完成，可回放历史会话
- [ ] §3.1 必测清单（6 条平台/配置陷阱）全部有对应用例
- [ ] CI 双引擎比对流水线骨架
- [ ] `try/catch` 清单（R4 的输入）
- [ ] 权限子系统逐文件分析报告（R5 的输入）

### 9.3 关键文件索引（重写时的高频参照）

| 用途 | 文件 |
|---|---|
| Agent 循环 | `src/query.ts`、`src/QueryEngine.ts` |
| 工具契约 | `src/Tool.ts`（`Tool` 类型定义） |
| 工具注册 | `src/tools.ts` |
| 模型抽象 | `src/services/api/client.ts`、`src/services/api/claude.ts` |
| 控制协议 | `src/entrypoints/sdk/controlSchemas.ts` |
| 流式输出 | `src/cli/print.ts`、`src/cli/ndjsonSafeStringify.ts` |
| 结构化 IO | `src/cli/structuredIO.ts`、`src/cli/remoteIO.ts` |
| 权限 | `src/utils/permissions/`（24 文件） |
| 会话存储 | `src/utils/sessionStorage.ts` |
| 配置应用 | `src/utils/managedEnv.ts`、`src/entrypoints/init.ts` |
| 平台适配 | `src/utils/windowsPaths.ts` |
| Hooks 契约 | `src/types/hooks.ts`、`src/entrypoints/agentSdkTypes.ts` |
| 录制回放 | `src/services/vcr.ts` |
| 配置规范 | `docs/config-loading.md` |

---

## 10. 下一步建议

1. **先做 Gate 0 的准备工作，不要先写 Go 代码。** 具体第一步：把 `services/vcr.ts` 改造成可回放的回归夹具，并用它录制 20–30 条覆盖核心路径的真实会话。
2. **读完 `src/utils/permissions/` 的 24 个文件**，产出一份分析报告，判断其中哪些是纯规则、哪些依赖模型调用。这决定 Phase 1 的权限模块能否按计划完成。
3. **导出 C4 控制协议的 JSON Schema**，这是方案 B 的技术前提——确认它真的能承载 core 与 TUI 的全部交互。
4. **明确一个决策**：最终交付是否要求"无 Node/Bun 运行时依赖"。
   - 是 → 必须包含 Phase 3，工期翻倍，建议先缩小 Phase 1/2 的功能范围
   - 否 → 方案 B 到此为止，Phase 3 不做

> 本计划书基于对当前仓库的实测数据。所有 LOC、文件数、并发统计均为实际统计结果；技术选型均核对了官方文档与当前版本状态（2026-10）。
