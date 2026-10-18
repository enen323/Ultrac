# pi 核心 harness → Ultrac Go 侧改造计划书

> 目标蓝本：`D:\develop\Project\pi`（earendil-works/pi，MIT）——只取其**核心 harness**
> 落地位置：**`D:\develop\Project\Ultrac` 仓库内，就地改造**（不新建项目）
> 已决策路线：**替换核心 + 彻底去掉权限层**（2026-10-12 用户拍板）
> 依据：2026-10-12 对两个仓库的逐文件实测（文件数、行数来自 `find`/`wc`，非估算）
>
> **当前进度：P0 ✅ 已完成（2026-10-13）；P1 ✅ 已完成（2026-10-13，provider 流式，真实端点双 dialect 验收通过）；P2 ✅ 已完成（2026-10-13，agent loop + Agent 状态机，假 provider 全套事件序列断言通过）；P3 ✅ 已完成（2026-10-14，pi 8 工具落地，133 个测试全绿）；P4 ✅ 已完成（2026-10-15，session pi 形状 + config pi 路径 + 分段式提示词，43 个新测试全绿，顺手修掉旧 Store 并发丢行 bug）；P5 ✅ 已完成（2026-10-16，CLI + print/json，真实端点四场景验收通过，抓出并修掉 anthropic 适配器两个真 bug）；P6 ✅ 已完成（2026-10-16，RPC 模式，18 命令 + 严格 JSONL，真实端点驱动脚本 38/38 断言通过）；P7 ✅ 已完成（2026-10-18，退场清理 44 文件 / 13,255 行 + E1–E7 全过，双跑抓出并修掉两个真协议 bug）。** 详见 §0.4 总览与 §3.1 / §3.2 / §3.3 / §3.4 / §3.5 / §3.6 / §3.7 / §3.8 落地记录。

---

## 0. 结论摘要（先看这节）

### 0.1 一句话结论

pi 的"能干活"核心只占它全仓的 **6.2%**；而 Ultrac 的 Go 侧**已经有 60% 的可用地基**。把两者接起来的做法是：**复用 Ultrac 的基建与工具实现逻辑，把契约层、引擎、工具契约换成 pi 的语义**——改动面约 5,200 行，复用面约 5,600 行。

### 0.2 落地路线（已决策，含代价）

| 决策项 | 选择 | 后果（必须知道） |
|---|---|---|
| 现有 Claude Code 形状的 Go 引擎 | **替换核心** | `internal/contract/*` + `internal/engine` + 工具契约就地改写为 pi 语义；现有实现退场（有 git 可回退） |
| Ultrac 的权限体系 | **彻底去掉** | 删除 `internal/permission`（906 行）+ `internal/shell`（690 行）分类器。理由：pi 有意不内置权限系统（其 README 明说） |

**两个必须明说的连带后果**：

1. **原「方案 B：Go 引擎 + 复用 TS TUI」的前提失效。**
   准确说法不是"TS TUI 不能用"，而是**它从来没接上过 Go 引擎，而改造后接上的代价会显著上升**（2026-10-13 核实）：

   | 事实 | 证据 |
   |---|---|
   | TS 侧**目前没有任何代码调用 Go 引擎** | `grep -rn "bin/ultrac\|ultrac.exe\|spawn.*ultrac" src/` 零命中 |
   | Go 引擎**刻意**讲 Claude Code 的方言 | `internal/transport/ndjson.go`：`{"type":"system","subtype":"init","claude_code_version":…,"permissionMode":…}` / `content_block_delta` / `tool_use` / `session_id` / `can_use_tool` |

   即：现在的 Go 引擎是**照着 Claude Code SDK 的线协议**写的，就是为了将来让 Ultrac 的 TS TUI（Ink/React，`components/` 81,559 + `ink/` 19,846 ≈ 107k 行）能直接消费。换成 pi 语义后，这条边界上会**同时断三层**：

   | 层 | 断在哪 |
   |---|---|
   | **协议层** | pi 的 12 种流式事件 + pi 的消息模型 ≠ `content_block_delta` / `tool_use` 形状 |
   | **数据模型层** | Ink 组件不是消费"通用事件流"，而是直接吃 Claude Code 的 `Message` / `Query` 类型；要接 pi 事件得写一层翻译 |
   | **功能集合层**（最根本） | TUI 是照着 Claude Code 的能力集写的——权限弹窗、plan mode、Task 子 agent、todo、89 个斜杠命令；**pi 有意不做这些**（README 明说 skip sub-agents and plan mode，且无权限系统）→ 这些 UI 组件会变成没有事件源的空壳 |

   **所以这是一个取舍，不是技术不可能**：要 TS TUI → 引擎必须保持 Claude Code 形状 → 放弃 pi 语义（那这个项目就是另一件事）；要 pi 语义 → TS TUI 要么弃用、要么写适配层（TUI 里约一半组件成死代码，且适配器要跟着两侧演进维护）。
   **本次改造等于给 Ultrac 的 Go 侧换了一个产品定位**：从"Ultrac 的后端"变成"pi 的 Go 实现"。
2. **现有 12 个内置工具里，5 个不在 pi 的核心 8 个之内**（`web_fetch` / `web_search` / `todo` / `ask` / `agent`），默认移除。
   其中 `agent`（子 agent）与 pi 明确的设计取向冲突（pi 主动不做 sub-agents）。**若想保留，见 §2.2 的可选增补路径**——pi 的工具插件模型允许它们以自定义工具身份回来。

### 0.3 三个决定性数字

| 事实 | 数字 | 含义 |
|---|---:|---|
| 改造面（要动的） | **≈ 5,200 行** | `contract` 841 + `engine` 1,450 + 工具契约包装 ≈ 2,900 |
| 复用面（不动的） | **≈ 5,600 行** | `config` 1,616 + `session` 1,157 + `transport`+sink 688 + `logging` + `cmd` ≈ 950 |
| 删除面 | **≈ 1,600 行** | `permission` 906 + `shell` 690 |
| pi 核心 harness 对标面 | **≈ 25,000 行 TS**（占 pi 全仓 6.2%） | 8 个工具 4,619 / agent 核心 2,519 / ai 核心 ≈ 5,500 |

### 0.4 分阶段总览

| 阶段 | 内容 | 主要动作 | 验收关卡 |
|---|---|---|---|
| **P0** ✅ **已完成（2026-10-13）** | 契约冻结 | 新建 `internal/ai`、`internal/agent` 的类型骨架 | `go build ./...`；类型与 pi 逐字段对照 |
| **P1** ✅ **已完成（2026-10-13）** | provider 流式 | 改造 `internal/llm/anthropic`，事件升到 12 种 | 真实 endpoint 打出 12 种事件 |
| **P2** ✅ **已完成（2026-10-13）** | agent loop + Agent 状态机 | 替换 `internal/engine` | 假 provider 下事件序列与 pi 逐条一致 |
| **P3** ✅ **已完成（2026-10-14）** | 8 个工具 | 替换 `internal/tool/builtin` | 每工具有单测；截断/模糊匹配行为对齐 |
| **P4** ✅ **已完成（2026-10-15）** | 会话/配置/提示词 | 改造 `session`、`config`、`prompt` | 会话 JSONL 结构与 pi 对齐（双向 fixture 验收） |
| **P5** ✅ **已完成（2026-10-16）** | CLI + print/json | 改造 `cmd/ultrac` 与 `transport/sink` | `ultrac -p "..."` 与 pi 输出 diff |
| **P6** ✅ **已完成（2026-10-16）**| RPC | 新建 `internal/rpc` | 同一驱动脚本双跑 |
| **P7** | 退场清理 + 端到端 | 删 `permission`/`shell`/旧契约 | 双跑对照通过 + `staticcheck` 零告警 |

### 0.5 三条最重要判断

1. **先冻结契约，再动引擎。** pi 的语义（transcript 承载 prompt、12 事件、`AgentTool`）与 Ultrac 现有契约**不是子集关系**，是两种形状。先冻结，否则每个阶段都要返工。
2. **工具只换"契约包装"，不重写"实现逻辑"。** Ultrac 的 bash 临时文件输出、grep 调 `rg`、read 的双限截断、edit 的 diff 都是踩过坑的成果，**照抄实现、重写包装**。
3. **双跑对照仍是杀手锏验收。** Ultrac 和 pi 都是无头可驱动的；同一 prompt 双跑 diff 事件序列，比任何自测都有说服力。

---

## 1. 目标与现状

### 1.1 pi 的核心边界（蓝本）

pi 是 monorepo，全仓 **402,800 行 TS / 14 个 package**。但"能干活"的核心只有约 **25,000 行 = 6.2%**：

| 分块 | pi 位置 | 行数 |
|---|---|---:|
| Agent 循环与状态机 | `packages/agent/src/*` | 2,519 |
| LLM 类型与事件协议 | `packages/ai/src/types.ts` + `utils/event-stream.ts` | 1,028 |
| 模型目录与注册 | `model-catalog.ts` / `models.ts` / `models-store.ts` | ≈ 1,500 |
| Provider 适配（取 2 家） | `api/anthropic-messages.ts` / `api/openai-completions.ts` | ≈ 3,000 |
| 8 个内置工具 | `coding-agent/src/core/tools/*` | 4,619 |
| 会话管理 | `core/session-manager.ts` | 2,013 |
| 会话运行时（核心子集） | `core/agent-session.ts` | ≈ 2,000（全文件 4,423，含 extensions/compaction/分支） |
| 系统提示词 | `core/system-prompt.ts` | 229 |
| 配置（子集） | `core/settings-manager.ts` + `config.ts` | ≈ 1,000（全量 2,084） |
| 无头模式 | `modes/print-mode.ts` + `json-event.ts` + `rpc/*` | 1,352 |
| CLI（子集） | `cli/*` + `main.ts` | ≈ 1,500 |

**pi 的关键机制**（不移植这 10 条就只是"另一个 ReAct loop"）：

1. **transcript 承载 system prompt**：`AgentContext` 只有 `messages` + `tools`；system prompt 是 transcript 的**首条 system message**，其后每条 system message 都是补丁（`content` 追加 / `sections` 按名替换或置 `null` 删除）。
2. **工具装载变更要"宣告"**：每轮请求前 diff「transcript 已声明的工具」与「运行时实际可执行的工具」，差异写进 system message 的 `toolsAdded`/`toolsRemoved`（`agent-loop.ts:333-363`）。
3. **工具两段式执行**：先串行 preflight（查工具 → `prepareArguments` → schema 校验 → `beforeToolCall`），通过后并发执行；`tool_execution_end` 按**完成序**发出，`toolResult` 消息按**源序**落盘。
4. **`terminate` 批次语义**：只有该批**全部** tool result 都标 `terminate` 才提前结束循环。
5. **截断消息里的工具调用一律不执行**：`stopReason === "length"` 时全部判失败（参数可能被静默截断）——**安全设计，非可选**。
6. **steering / followUp 双队列**：`steer()` 在工具跑完后注入；`followUp()` 在本该停下时注入；各有 `one-at-a-time` / `all` 模式。
7. **三个控制流钩子**：`prepareRequest`（每次请求前重装上下文）、`prepareNextTurn`、`finishTurn`（`{action:"end"|"continue"}`）。
8. **流式事件 12 种**：`start` / `text_start|delta|end` / `thinking_start|delta|end` / `toolcall_start|delta|end` / `done` / `error`；`partial` 是共享的"到目前为止"对象。
9. **`streamFn` 契约：不许抛异常**，失败必须编码进流（`error` 事件 + `stopReason`）。
10. **工具错误靠 throw**，由循环转成 `isError: true` 的 toolResult。

### 1.2 Ultrac Go 侧现状盘点（实测）

`internal/` + `cmd/` 合计 **14,434 行非测试 + 5,766 行测试**（T0+T1 已落地，`make check` 绿，`staticcheck` 零告警）。

| 包 | 非测试行数 | 职责 | 本次处置 |
|---|---:|---|---|
| `internal/config` | 1,616 | 六层设置合并 + env 应用 | **复用**（改读 pi 的路径/字段） |
| `internal/session` | 1,157 | JSONL transcript + UUID 链 | **复用**（改目录布局与 entry 词表） |
| `internal/logging` | ≈ 400 | 结构化日志 + APIError 判定 | **全量复用** |
| `internal/transport` + `sink` | 688 | NDJSON 写出 + 4 种 sink | **复用骨架**（事件字段按 pi 对齐） |
| `cmd/ultrac` | ≈ 1,250 | CLI 参数 + 装配 | **复用骨架**（参数改 pi 的） |
| `internal/version` | ≈ 30 | ldflags 版本 | **全量复用** |
| `internal/llm/anthropic` | 1,203 | Anthropic SSE 流式客户端 | **改造**（事件粒度升到 12 种） |
| `internal/contract/{provider,tool,event}` | 841 | 三个契约 | **替换**为 pi 语义 |
| `internal/engine` + `prompt` | 1,450 | 循环 + 系统提示词 | **替换**（prompt 改分段式保留） |
| `internal/tool/builtin` | ≈ 2,900 | 12 个 Claude Code 工具 | **替换**为 pi 8 工具（实现逻辑照抄） |
| `internal/permission` | 906 | 权限判定（4 模式 + 规则集） | **删除** |
| `internal/shell` | 690 | mvdan/sh 命令分类器 | **删除** |

### 1.3 两者的语义鸿沟（必须知道差在哪）

| 维度 | Ultrac 现状 | pi | 处置 |
|---|---|---|---|
| system prompt | 请求字段 `provider.Request.System` | transcript 首条 system message + 分段补丁 | **换结构** |
| 工具装载变更 | 无 | 每轮 diff 并宣告 | **新增** |
| 流式事件 | 10 种粗粒度（`text_delta` / `tool_use` …） | **12 种细粒度**（含 `*_start` / `*_end`、`contentIndex`） | **提粒度** |
| 工具契约 | `Tool[In]` + 12 谓词 + `canUse` 回调 | `AgentTool` + `execute` + `before/afterToolCall` | **换形状** |
| 工具并发 | `IsConcurrencySafe(in)` 分组 + 12 并发上限 | `executionMode` + preflight/执行两段式 | **换语义**（保留并发上限这个好设计） |
| 结果事件顺序 | 批次完成后**统一按序**发出 | `*_end` 按**完成序**，消息按**源序** | **调整** |
| 循环控制 | `maxTurns` 单向 | steering / followUp 双队列 + `finishTurn` | **新增** |
| 批次终止 | 无 | `terminate` 集合语义 | **新增** |
| 截断处理 | 发 `notice` 提示不完整 | 该批工具全部不执行 | **改为安全优先** |
| 权限 | 有（4 模式 + 规则） | 无（有意不做） | **删除** |
| thinking | delta 展示但**不回灌**（代码里明写 T2 TODO） | 必须回灌 signature | **本次必须做** |
| 流式抽象 | `iter.Seq2[Chunk, error]` | async iterator + `EventStream` | **保留 Go 的 iter 形态**（已验证优于 channel） |

### 1.4 处置总表（复用什么 / 换什么 / 删什么）

```
复用（不改语义）        复用+改造（改路径/字段/粒度）      替换（改语义）           删除
────────────────      ───────────────────────────    ─────────────────     ────────
logging               config                         contract/provider     permission
version               session                        contract/tool         shell
                      transport + sink                contract/event
                      cmd/ultrac                      engine (+prompt)
                      llm/anthropic                    tool/builtin
```

---

## 2. 范围界定

### 2.1 目标（可度量）

| # | 目标 | 度量方式 |
|---|---|---|
| G1 | **可跑通真实编码任务** | 给一个真实需求，agent 自主 read→edit→bash 闭环，无需人工干预 |
| G2 | **与 pi 可对照** | 同一份 RPC 驱动脚本跑 pi 与 ultrac，事件序列 diff 一致（时间戳/ID 除外） |
| G3 | **provider 可替换** | 新增一家 provider = 新增一个文件 + `init()` 注册，`internal/agent` 零改动 |
| G4 | **新增工具不改引擎** | 新增工具 = 一个文件 + `init()` 注册，`internal/agent` 零改动 |
| G5 | **零外部运行时** | 单个静态二进制，不依赖 Node / Python |
| G6 | **无权限层** | `internal/permission`、`internal/shell` 从仓库消失；`go build` 无悬空引用 |

### 2.2 非目标（写进 README 防蔓延）

| 不做 | 原因 |
|---|---|
| interactive TUI（pi 侧 21,907 行） | 决定性工作量，且是"重新设计"不是"翻译"（Ink/React → bubbletea 语义不同） |
| pi 的 `durable` / `chord` / `server` / `client` / `protocol(CBOR)` | 分布式与持久化运行时 |
| pi 的 `mcp` / `codemode` / `extensions` / `skills` / `telemetry` / `evals` | 扩展生态与观测，放核心稳定之后 |
| OAuth 登录（pi 侧 3,832 行） | 只做 API key |
| 图像模型 / 分类器模型 | 与 chat 无关 |
| compaction（上下文压缩，pi 侧 1,671 行） | 策略判断多；先不做，超长上下文直接报错 |
| HTML 会话导出 | 纯展示 |
| **权限系统** | 已决策删除（pi 有意不做） |

**可选增补路径（不在本轮范围，但留口子）**：Ultrac 现有 5 个额外工具（`web_fetch` / `web_search` / `todo` / `ask` / `agent`）本轮移除。其中 `web_fetch` / `web_search` / `todo` 与 pi 的工具插件模型不冲突，可在 P7 之后以**自定义工具**身份回来（一个文件 + 注册）。`agent`（子 agent）与 pi 的设计取向冲突，不建议恢复。

### 2.3 范围边界表

| 能力 | pi 位置 | 本轮 | 备注 |
|---|---|---|---|
| agent loop / 状态机 / 事件 | `packages/agent` | ✅ 全量 | |
| LLM 类型与事件协议 | `ai/src/types.ts` | ✅ 全量 | 字段名对齐 |
| Anthropic Messages 适配 | `ai/src/api/anthropic-messages.ts` | ✅ | 改造现有 `internal/llm/anthropic` |
| OpenAI-compatible chat | `ai/src/api/openai-completions.ts` | ✅ | 新增 |
| 其余 provider（10+ 家） | `ai/src/api/*` | ❌ | 按需后补 |
| read / bash / edit / write / grep / find / ls | `core/tools/*` | ✅ | |
| powershell | `core/tools/powershell.ts` | ⚠️ 可选 | 67 行；Windows 环境建议留 |
| session JSONL | `core/session-manager.ts` | ✅ 子集 | 追加写 + 头行 + 读取重建；分支树延后 |
| system prompt 分段 | `core/system-prompt.ts` | ✅ | 分段机制对齐 |
| settings 解析 | `core/settings-manager.ts` | ⚠️ 子集 | API key / model / 工具开关 |
| print / json 模式 | `modes/print-mode.ts` | ✅ | |
| RPC 模式 | `modes/rpc/*` | ✅ | 命令集对齐 |
| 重试 / 退避 | `core/agent-session.ts` 内 | ✅ | 只在首个 delta 之前重试 |
| **Ultrac 的 12 个工具** | `internal/tool/builtin` | 4 个留、5 个移除、3 个换名 | 见 §3.4 映射表 |
| **Ultrac 的权限层** | `internal/permission` `internal/shell` | ❌ 删除 | |

### 2.4 契约冻结清单（P0 必须一次性做完）

**这四个文件是整份计划的"宪法"**，冻结后再动，代价是全线返工。

| # | 契约 | 冻结内容 | pi 出处 |
|---|---|---|---|
| C1 | **LLM 消息类型** | `Message` 联合体 4 成员的全部字段与 JSON 键名；`AssistantMessage.content` 的 3 种块；`Usage` 的 7 个计数 | `ai/src/types.ts:414-639` |
| C2 | **流式事件协议** | 12 种事件名 + 字段（尤其 `contentIndex`）；`done`/`error` 的 `reason` 枚举 | `ai/src/types.ts:801-817` |
| C3 | **工具契约** | `AgentTool` 字段集；`execute` 的 4 个参数；`AgentToolResult` 的 5 个字段；`StopReason` 枚举 | `agent/src/types.ts:423-499` |
| C4 | **对外线协议** | RPC 的 `RpcCommand`/`RpcResponse` 全部 variant；print `--mode json` 的事件类型 | `modes/rpc/rpc-types.ts`、`modes/json-event.ts` |

**冻结方式**：每个契约写一个 Go 测试，把 pi 的类型定义逐字段手工转成 Go 断言表，两边跑同一份 fixture。**不要靠"读起来一样"确认对齐。**

> **注意**：C1–C3 会**替换** Ultrac 现有的 `internal/contract/{provider,tool,event}`。建议做法是在同一包路径下改写，而不是新建包——避免长期两套契约并存。

---

## 3. 分阶段实施步骤

### 3.0 估算前提

- 估算只到"能跑通并验收"，不含打磨、文档、发布。
- 含单测，**不含测试的完成不算完成**。
- 每个阶段结束都能停下来；每个阶段结束都跑一次 `go build ./... && go vet ./...`。
- 改造期允许旧代码暂时与新代码并存，但**每个阶段必须编译通过**（避免一次性大爆炸）。

### 3.1 P0 — 契约冻结与包骨架 ✅ 已完成（2026-10-13）

**动作**

```
internal/
├── ai/                      ← 新建，承载 C1 + C2
│   ├── types.go                 Message / ContentBlock / ToolCall / Usage / StopReason
│   ├── event.go                 AssistantEvent 12 种 + EventStream
│   └── context.go               Context / TranscriptContext / normalize
├── agent/                   ← 新建，承载 C3
│   ├── types.go                 AgentContext / AgentEvent / AgentTool / 钩子
│   └── toolcall.go              工具契约与参数校验
└── contract/                ← 旧包，本阶段起标记 deprecated，P7 删除
    ├── provider/                （旧）
    ├── tool/                    （旧）
    └── event/                   （旧）
```

**DoD**

- [x] `go build ./...` + `go vet ./...` 通过（实际用 `./cmd/... ./internal/...`：仓库根有 `node_modules/`，`./...` 会 SIGTERM）
- [x] C1/C2/C3 的对照测试跑绿，fixture 取自 pi 源码里的真实常量
- [x] `internal/ai` 每个导出类型都有一条注释注明对应 TS 文件与行号（**三个月后你会靠它导航**）
- [x] 旧 contract 包加 `// Deprecated:` 注释并说明替代品

**关卡**：把 `internal/ai/types.go` 与 `ai/src/types.ts` 并排走一遍。**发现任何"我猜的"字段就停下来核对。**

> **P0 落地记录（2026-10-13）**：关卡已过。逐字段核对时发现计划书 §4.3 草案有两处与 pi 源码不符，已按源码修正：
> ① `StopReason` 草案漏了 `"deferred"`（pi `types.ts:469` 有 7 个值，且 `done` 事件的 reason 枚举含它）；
> ② 线上协议按 `json-event.ts:20-61` 裁剪 `partial`（message_start/deltas/message_end 各司其职），
> `toolcall_start` 额外带 `id`/`toolName`——这部分形态在 P0 就冻结进 `MarshalEvent`。
> 另有两个实现级决策：`Message`/`ContentBlock` 用「单 struct + 判别字段 + 自定义 MarshalJSON」
> （计划书 §9.1 的映射路线），system/user 的 `content` 字符串/数组二态在 Marshal/Unmarshal 里显式处理；
> `agent` 侧参数校验发现「校验重新编码后的类型化值会吞掉未知字段」，改为无 shim 时校验模型原始入参
> （`additionalProperties:false` 因此真正生效），有 shim 时校验 shim 输出。
> C4（RPC/print 线协议）按计划推到 P6 前，未在 P0 落地。

> **P0 复盘审计记录（2026-10-13 当天追加）**：完成声明发出后又做了一轮逐文件审计，抓出并修掉 5 个问题——
> ① `MessageText` 用 `""` 拼接多个 text 块，pi 的 `contentText` 用 `"\n"`（`utils/text.ts:6-12`）；
> ② `GetCurrentSystemPrompt` 漏了 sections——pi 的 `getCurrentSystemPrompt` 是
> `getCurrentSystemMessage` + `getSystemMessageText`（`text.ts:14-21`），prompt 含分段；
> ③ sections 回放顺序原先依赖 Go map 遍历（随机）——改为显式跟踪 first-declaration 顺序
> （pi 的 Map 语义：删除后重新声明移到最后），P4 的分段 diff 依赖这个顺序；
> ④ `removeString` 用 `s[:0]` 原地压缩，会污染调用方底层数组——改为分配新切片；
> ⑤ `toolcall_start` 的 partial 不变体破折时 pi 抛错，而我静默省略 id/toolName——改为返回错误。
> 另修正两处不诚实的注释：`TranscriptContext` 的 brand（Go 未导出字段挡不住外部 keyed literal，
> brand 只是文档不是强制）、`EventStream` 的死字段 `err`（错误走流内 `EvError`，已删）。
> 全部修复均有回归测试；新增并发 push/consume 冒烟测试（P2 循环的生产者/消费者形态；
> 本机无 C 编译器跑不了 `-race`，此测试是并发面的唯一兜底）。
> 依赖方向已验证：`internal/ai` 只依赖标准库；`internal/agent` 只依赖 `ai` + jsonschema；
> `go list -deps` 对 permission/shell/engine/builtin/contract 零命中。

### 3.2 P1 — provider 层（流式 12 事件）

**风险最高的一块**，单独成阶段单独验收。现有 `internal/llm/anthropic`（1,203 行）是**改造**不是重写——SSE 解析、按 index 分桶攒 tool 参数、重试只在首 chunk 前、`iter.Seq2` 这些都已踩过坑。

**动作**

```
internal/ai/
├── stream.go            事件流抽象（沿用 iter.Seq2）
├── context.go           normalizeContext（systemPrompt+tools 折进首条 system message）
├── model.go             Model / 内置目录 / 用户覆盖
├── registry.go          provider 注册表（沿用现有 provider.Register 的 init() 模式）
├── anthropic/           ← 从 internal/llm/anthropic 迁移改造
│   ├── request.go
│   ├── sse.go
│   └── stream.go        事件粒度从 5 种 Chunk 升到 12 种 AssistantEvent
└── openai/              ← 新增（chat completions + SSE）
```

**DoD**

- [x] 真实 endpoint 流式拿到 `text_start → text_delta* → text_end → done`
- [x] tool_use **按 content block index 分桶累积字符串**，只在 `content_block_stop` 发完整调用（现有实现已如此，**保持**）
- [x] `ctx` 取消能中断在途请求，无 goroutine 泄漏
- [x] 429/5xx 退避重试，**只在首个 delta 之前**允许重放（现有实现已如此，**保持**）
- [x] 流被截断报 `stopReason: "length"`，**不静默接受**
- [x] ⚠️ **thinking 块 signature 回灌**——现有代码在 `engine/loop.go:105-113` 明写着"不回灌，归 T2"，**本轮必须补上**（不回灌会让 API 拒绝整个请求）
- [x] OpenAI-compatible 适配至少跑通一家真实端点（DeepSeek 或同类）

> **P1 落地记录（2026-10-13）**：完成。实际落地与计划书的偏差及实现决策如下——
>
> **落地内容**：`internal/ai/stream.go`（`StreamFn` 契约 + `StreamRequest` 注册表入口）、`internal/ai/registry.go`（按 `Api` 注册，仿 pi `compat.ts` 的 `registerApiProvider`）、
> `internal/ai/cost.go`（`CalculateCost`，移植 pi `models.ts:1206`）、`internal/ai/estimate.go`（`EstimateContextTokens` + `ClampMaxTokensToContext`，移植 pi `utils/estimate.ts` + `simple-options.ts:18`）、
> `internal/ai/catalog.go`（手写起步模型目录，pi 的生成目录 JSON 不在本 checkout）、
> `internal/ai/anthropic/`（从 `internal/llm/anthropic` 迁移 + 12 事件翻译层）、`internal/ai/openai/`（新增，含 compat 矩阵压缩版）。
> `agent.StreamFn` 改为 `ai.StreamFn` 别名（契约归 ai 层，P0 时放在 agent 是权宜）。
>
> **真实端点验收**（凭据取自 `~/.ultrac/settings.json`，探针在 gitignore 的 `bin/probe/`，P5 会做成 `ultrac probe` 子命令）：
>
> | 端点 | 结果 |
> |---|---|
> | DeepSeek anthropic 兼容端点（`/anthropic`，`deepseek-flash`，默认 thinking） | `start → thinking_* → text_* → done`，usage 齐全 |
> | 同上 + 声明 `read` 工具 | 10 个 `input_json_delta` 分片累积成 `{"path":"go.mod"}`，`done{reason:toolUse}` |
> | DeepSeek 原生端点（`/chat/completions`，`deepseek-chat`） | `start → text_start → text_delta* → text_end → done`，usage 来自收尾 chunk |
> | 同上 + 工具 / `deepseek-reasoner` | tool_calls 按 stream index 分桶 ✓；`reasoning_content` → thinking 事件 ✓ |
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **provider 级重试默认 0 次**（跟随 pi `provider-retry.ts:111` 的 `maxRetries ?? 0`），不再沿用旧客户端的 10 次——重试*策略*归 settings/循环层（P2/P4），"只在首事件前重放"的边界原样保留；
> ② **`cache_control` 默认不发**（`Options.SendCacheControl`  opt-in）：pi 默认开，但未知兼容端点可能因缓存标记返 400，真实端点连通性优先于缓存优化；
> ③ **adaptive thinking 的 `effort` 参数未移植**，只做 budget 式 thinking，`xhigh`/`max` 按 pi 的 `clampReasoning` 降为 `high`；
> ④ **SSE 行解析与重试循环在两个适配器各一份**（pi 同样是每适配器自带），已在注释标注 P7 抽取候选；
> ⑤ **`internal/llm/anthropic` 保留不动**——旧 engine 仍在用它，P7 统一退场。
>
> **测试**：anthropic 30 个用例 / openai 16 个用例全绿（含 12 事件序列逐条断言、分桶累积、截断报错、signature 回灌往返、取消、重试边界、consumer 早退不阻塞）；
> `go build` / `go vet` / `staticcheck` 零告警；旧包测试无回归。

### 3.3 P2 — agent loop + Agent 状态机（替换 engine）

**动作**

```
internal/agent/
├── types.go       AgentContext / AgentEvent / AgentLoopConfig / AgentTool（P0 已建，此处补全）
├── loop.go        agentLoop / runLoop / streamAssistantResponse
├── toolcall.go    prepareToolCall / executePreparedToolCall / finalizeExecutedToolCall
└── agent.go       Agent：Subscribe / Prompt / Continue / Steer / FollowUp / Abort / WaitForIdle
```

**可保留的既有设计**（来自 `internal/engine/tools.go`，不要丢）：

- `maxParallelTools = 12` 的并发上限（注释说得很对："二十个并行读会变成两万个打开的文件句柄"）
- 进度事件经 channel 汇集到循环 goroutine 再发（worker 直接 yield 是数据竞争）
- 「未知工具也要产出一条 tool_result」——API 要求 tool_use 与 tool_result 一一对应

**DoD**

- [x] 单轮：`message_start(user) → message_start → message_update* → message_end → message_stop`
- [x] 多轮工具：含 `tool_execution_start → tool_execution_end → toolResult 消息`，`turn_start` 重现
- [x] **并行模式**：`tool_execution_end` 按**完成序**、toolResult 消息按**源序**（专门的并发时序测试）
- [x] 任一工具 `executionMode: "sequential"` 时整批退化串行
- [x] `stopReason === "length"` 时该批 tool call **全部失败、不执行**
- [x] `terminate` 批次语义：全 terminate 才提前结束
- [x] `steer` 在工具跑完后注入；`followUp` 在本本该停止时注入；两种队列模式都测
- [x] **工具装载变更宣告**（§1.1 第 2 条）——transcript 内工具集与运行时不一致时发 system message
- [x] `Abort()` 后无 goroutine 泄漏，事件序列以 `agent_end` 收束

**关卡**：用**假 provider**（返回预置事件序列）跑全套测试，**不打真实 API**。pi 自己就是这么测的。

> **P2 落地记录（2026-10-13）**：完成。落地内容与计划书的偏差及实现决策如下——
>
> **落地内容**（`internal/agent/`，实现 4 文件 + 测试 4 文件，共 5,818 行）：
>
> | 文件 | 内容 | pi 出处 |
> |---|---|---|
> | `loop.go`（新增 ≈700 行） | `RunAgentLoop` / `RunAgentLoopContinue` / `runLoop` / `streamAssistantResponse` / `declareToolChanges` / `failToolCallsFromTruncatedMessage` | `agent-loop.ts` 全篇 |
> | `toolcall.go`（追加 ≈660 行） | `prepareToolCall` / `executePreparedToolCall` / `finalizeExecutedToolCall` / `executeToolCalls{Sequential,Parallel}` / `RunToolCall` / `agentEventQueue` | `agent-loop.ts:508-949` |
> | `agent.go`（新增 ≈750 行） | `Agent` 状态机：Subscribe / Prompt / Continue / Steer / FollowUp / Abort / WaitForIdle / Reset / State、双队列、`processEvents` 状态归约、`handleRunFailure` | `agent.ts` 全篇 |
> | `fakes_test.go` 等（4 个测试文件） | 假 provider（预置事件脚本）+ 事件录制器 + 行为工具；**47 个测试函数**（loop 19 / toolcall 12 / agent 16） | — |
>
> **保留的既有设计**（计划书 §3.3 要求，均已落实并在注释中标明来源）：
> `maxParallelTools = 12` 信号量；「未知工具也要产出 tool_result」；进度/结束事件经**无界队列**汇集到循环 goroutine 再发
> （worker 直接 yield 是数据竞争——consumer 不保证 goroutine 安全；完成序由队列 FIFO 保证，与 pi 单线程事件循环等价）。
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **`EventSink` 是同步 `func(ev) error`**（pi 的 emit 是 async）。emit 错误即中止 run 并上抛；Agent 层把它转成 `handleRunFailure`
> （合成失败消息序列），与 pi 的 promise rejection 被 `runWithLifecycle` catch 等价。
> ② **`Agent.Prompt` 吞掉 run 级错误**（流失败、abort、listener 抛错都经事件 + `State().ErrorMessage` 上报，不返回 error）——
> 镜像 pi `agent.ts:523-530` 的 catch 语义；返回 error 仅限编程错误（并发 run、空 prompt、非法 continue）。
> ③ **`handleRunFailure` 的 `agent_end` 只带失败消息**（不带整段 transcript）——pi `agent.ts:547` 原文如此，镜像而非"修正"。
> ④ **provider 特定选项不进 `AgentOptions`**（transport / onPayload / maxRetryDelayMs 缺席）：契约层只放共识字段，
> 供应商旋钮留在 provider 包（P1 决策的延续）。
> ⑤ DoD 里的 "message_stop" 在 pi 的 `AgentEvent` 全集（`types.ts:516-539`）中不存在——流的终止（done/error）由
> `message_end` 承载，每轮由 `turn_end` 收束；测试按 pi 真实事件集断言。
> ⑥ **steering 预入队的注入点是首个请求之前**（loop 启动即 poll，`agent-loop.ts:176`），不是"工具跑完后"——
> "跑完后注入"指轮间 poll（`:295`）。两种队列模式的差别在**两条消息入队时的顺序**上体现（one-at-a-time 第二条落到下一个排放点）。
>
> **过程中抓出并修掉的两个真 bug**：
> ① **P0 存量**：`internal/ai/types.go` 的 `ContentBlock.MarshalJSON` 在 `BlockToolCall` 分支把 `Type` 写成了 `string(BlockImage)`——
> toolCall 块线上形状是 `"type":"image"`。P2 的 message wire 断言首次跑到该路径时暴露（该 bug 随 P0 提交进入仓库，
> `TestMessageWireShape` 实际是红的）。已修，ai 包测试转绿。
> ② **P2 自身**：并行批次**全部 immediate**（未知工具 / 校验失败 / 全被 block）时没有 worker 被 spawn，
> `inFlight == 0` 导致没人关闭事件队列 → drain 死锁。修法：preflight 结束后若 `inFlight == 0` 由循环 goroutine 关队列
> （`inFlight` 只在 preflight 内增长，无竞态）。回归测试 `TestAllImmediateBatchDoesNotHang`。
>
> **测试**：47 个测试函数全绿（单轮/多轮完整事件序列逐条断言、并行完成序 vs 源序时序测试、12 并发上限实测、
> length 截断全批失败、terminate 全-or-无、两种队列模式、工具装载宣告 5 个子例、Abort 无泄漏 + goroutine 计数回归、
> listener 抛错的失败路径、sink 失败中止不挂 worker）。`go build` / `go vet` / `staticcheck` 零告警；
> `go test ./internal/agent -count=5` 连跑 5 遍无抖动（本机无 C 编译器跑不了 `-race`，并发面靠时序测试 + 重复跑兜底）。
> 旧包（engine / permission / shell / builtin 等）测试无回归。

### 3.4 P3 — 工具层（pi 的 8 个）

**关键原则：换契约包装，不重写实现逻辑。**

| pi 工具 | Ultrac 现有 | 动作 | 可照抄的实现 |
|---|---|---|---|
| `read` | `builtin/file_read.go`(259) | 换包装保留逻辑 | 双限截断、`offset=N` 续读提示 |
| `bash` | `builtin/bash.go`(391) | 换包装保留逻辑 | **临时文件收集输出**（`bytes.Buffer` 会挂死）、进程树超时 |
| `write` | `builtin/file_write.go`(120) | 换包装保留逻辑 | |
| `edit` | `builtin/file_edit.go`(142) + `diff.go`(186) | 换包装 + 补模糊匹配 | 现有是精确替换，**pi 有 556 行 `edit-diff.ts` 做模糊兜底** |
| `grep` | `builtin/grep.go`(514) | 换包装保留逻辑 | 调 `rg`、head_limit 多收一条判截断 |
| `find` | `builtin/glob.go`(153) | 换包装（pi 的 find 是 fd/路径匹配） | |
| `ls` | **无** | **新增** | |
| `powershell` | **无** | 可选新增 | |

**要移除的**：`web_fetch.go`(237)、`web_search.go`(207)、`todo.go`(244)、`agent.go`(314)、`ask.go`(200)、`match.go`(165)、`walk.go`(95) —— 共约 1,460 行（`match`/`walk` 若被 grep/find 依赖则保留其逻辑）。

**DoD**

- [x] 每个工具有单测；`read` 的截断续读提示文案与 pi 逐字一致
- [x] `edit` 覆盖：精确匹配、多处匹配拒绝、模糊匹配（空白/引号差异）、未找到时给出最接近片段
- [x] `bash` 用**临时文件**收集输出（继承现有实现，**不要退 regression 到 `bytes.Buffer`**）
- [x] `bash` 超时能杀掉子进程树（含孙进程）
- [x] `grep`/`find` 在 `rg`/`fd` 缺失时给清晰报错（**pi 会自动下载；本项目不下载**，直接提示安装）
- [x] 工具失败 `throw`/返回 error，而不是把错误塞进 `Content`
- [x] 新增工具 = 一个文件 + `init()`，引擎零改动（G4 验收）

> **P3 落地记录（2026-10-14）**：完成。新包 `internal/tool`（package tool，14 个实现文件 ≈3,300 行 + 10 个测试文件 ≈2,800 行，133 个测试函数），旧 `internal/tool/builtin` 原样保留至 P7（旧引擎仍在用）。
>
> **落地内容**：
>
> | 文件 | 内容 | pi 出处 |
> |---|---|---|
> | `tool.go` | 注册表（Register 工厂 + All(cwd)/Names/Lookup）+ 路径解析（`resolveToCwd`：unicode 空格、`@` 前缀、Git-Bash 盘符、`~`、file://） | `tools/path-utils.ts` + `utils/paths.ts` |
> | `truncate.go` | `truncateHead/Tail/Line`、`formatSize`、常量 2000 行/50KB/500 字符 | `tools/truncate.ts`（315 行全量） |
> | `read.go` | path/offset/limit、图片块、四类截断提示逐字、首行超限 sed 兜底 | `tools/read.ts` |
> | `write.go` | path/content、mkdir -p、文件变更队列 | `tools/write.ts` + `file-mutation-queue.ts` |
> | `edit.go` | edits[] 批量、PrepareArguments shim（JSON 字符串/单对象/旧式 flat pair）、BOM、CRLF 保序、details{diff,patch,firstChangedLine} | `tools/edit.ts` |
> | `edit_diff.go` | 模糊匹配全量（NFKC、智能引号/破折号/空格归一、`applyReplacementsPreservingUnchangedLines`、错误文案逐字）+ LCS diff 引擎 + 显示 diff | `tools/edit-diff.ts`（557 行） |
> | `bash.go` + `tree_{unix,windows}.go` | command/timeout(秒)、shell 解析、stdout/stderr 同一 `*os.File`、超时/取消杀进程树（Windows taskkill /T /F，Unix Setpgid + kill(-pgid)）、尾截断 + Full output 临时文件、退出码 isError、wall time | `tools/bash.ts` + `utils/shell.ts` |
> | `powershell.go` 逻辑并入 `bash.go` | pwsh/powershell 解析、UTF8 输出前缀 | `tools/powershell.ts` |
> | `grep.go` | spawn `rg --json` 解析 match 事件、500 字符行截断、context 行、limit 100、notice 逐字 | `tools/grep.ts` |
> | `find.go` | spawn `fd --glob`、git 仓库探测 `--no-require-git`、full-path 模式、relativize、limit 1000 | `tools/find.ts` |
> | `ls.go` | 排序（大小写不敏感）、`/` 后缀、limit 500、notice | `tools/ls.ts` |
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **`tool` import `agent`**：§4.2 硬约束"tool 不得 import agent"与 C3 冻结在 `internal/agent` 的事实冲突——工具要实现 `AgentTool[In]` 就必须引用该类型。落地取约：`tool` 只 import `agent` 的**契约类型**（`AgentTool`/`Erase`/`Result`），绝不调用引擎行为；依赖方向单向（tool → agent → ai），无回路。约束的立法本意（工具不回调引擎能力）完全成立。
> ② **grep/find 改为外部依赖**（shell out rg/fd）：计划书 §3.4 表格说"grep 调 rg 保留逻辑"，但现有 `builtin/grep.go` 实为纯 Go walker（不读 .gitignore、输出格式与 pi 不同）。按 DoD、§5.1、R12、E4 四条一致的解释，落地为 spawn rg/fd，缺失时报安装提示。本机 rg/fd 均未安装（winget 被 WattToolkit MITM 掐断），集成测试按存在性 skip，主路径用「测试二进制重exec 当假 rg/fd」全覆盖。
> ③ **edit 未命中给"最接近片段"**（DoD 增补，pi 没有）：两级实现——精确行投票（差分数组，线性）+ 单行模糊锚点（字符 bigram Dice ≥0.5），都给不出时明说"没有任何一行出现在文件里"。
> ④ **工具失败的返回机制**：bash 对 abort/timeout/spawn 返回 Go error（throw 语义，pi 同），对非零退出码返回 `IsError` 结果（pi 同）；read/write/edit/ls/grep/find 的预期失败（文件不存在等）返回 `IsError` 结果而非 throw——P0 冻结的 C3 契约明确允许两种（"must throw ... or return a Result with IsError set"），且模型侧结果逐字一致（都变成 isError toolResult）。
> ⑤ **保留的 Ultrac 防御**（pi 会做更糟的事）：非图片二进制文件拒绝入 transcript（pi 会当 UTF-8 文本吐乱码）；空文件返回注记（pi 返回空串，空 text block 有被 API 拒绝的风险）；write 保留既有文件权限位。
> ⑥ **`x/text` 提升为直接依赖**（NFKC 归一需要）：它本就以 indirect 身份在模块图里（jsonschema 依赖），无新增下载。
> ⑦ **bash 无默认超时、无实时输出流式**：跟随 pi（"optional, no default timeout"）；预置引擎的 120s 默认与 BASH_* 环境变量策略留给 P4 settings 层。流式快照需要轮询 goroutine，本期不做（loop 已有 tool_execution_start 渲染）。
>
> **测试**：128 个测试函数全绿（read 四类通知逐字、edit 精确/多处拒绝/模糊三型/最接近片段两级/BOM/CRLF/overlap/三种 shim、bash 临时文件+孙进程树杀+截断保留全量文件+无截断清理、grep/find 假二进制全链路+真实二进制集成 skip、ls、truncate 边界含 toFixed 舍入、G4 新工具零引擎改动）；`-count=3` 连跑无抖动。`go build` / `go vet` / `gofmt` / `staticcheck`（全仓）零告警；agent/ai/engine 及全部旧包测试无回归。旧包（engine/permission/shell/builtin）按 R9 原样保留至 P7。

### 3.5 P4 — 会话 + 配置 + 提示词

**动作**

| 包 | 现状 | 改造点 |
|---|---|---|
| `internal/session` | `~/.ultrac/projects/<encoded-cwd>/*.jsonl`，Entry 词表：`user`/`assistant`/`attachment`/`system`/`progress`/`summary`/`last-prompt`/`custom-title`/`queue-operation` | 目录改 `~/.ultrac/sessions/<encoded-cwd>/`（2026-10-16 起：自有项目不用 pi 目录，见 §3.5 偏差①）；加**头行**；entry 字段按 pi 对齐；`Store.Append/Flush/Read` 机制**保留** |
| `internal/config` | 六层合并（user<project<local<flag<policy>），`Settings.Extra` 原样保留未知字段 | 层级简化为 pi 的 user<project<flag；**保留 Extra 原样写回**（否则静默擦除用户自定义配置） |
| `internal/engine/prompt` → `internal/prompt` | 单块 text 提示词 | 改**分段式**：`preamble`/`tools`/`rules`/`project_context`/`skills`/`cwd`/`docs`；分段名与 pi 一致 |

**DoD**

- [ ] 会话文件结构对齐 pi：首行 header，其后每行一个 entry
- [ ] **只追加**，不改历史行（分支场景除外）
- [ ] 写出的 JSONL 能被 pi 读取、pi 写的能被本项目读取并重建等价 transcript（**双向**，G3 验收）
- [ ] transcript 重建**按显式父子关系**（`ParentUUID`，现有 `BuildChain` 已实现），不信文件顺序
- [ ] 配置：留空字段 = **删除该键**；**写回保留未知字段**（现有 `Extra` 机制保留）
- [ ] system prompt 分段名与 pi 一致；`diffSystemPromptSections` 的补丁语义对齐

> **P4 落地记录（2026-10-15）**：完成。落地内容、与计划书的偏差及顺手修掉的存量 bug 如下——
>
> **落地内容**：
>
> | 文件 | 内容 | pi 出处 |
> |---|---|---|
> | `internal/config/paths.go` + `keys.go`（改，2026-10-16 二次修订） | `DirName` = `.ultrac`（P4 曾短暂为 `.pi`）；`AgentDir()` 与 `ConfigHome()` 同指 `~/.ultrac`（`ULTRAC_CONFIG_DIR` 可整体搬迁 settings+sessions；P4 的 `PI_CODING_AGENT_DIR`/`PI_CODING_AGENT_SESSION_DIR` 已删）；`SessionsDir()` = `~/.ultrac/sessions`；user 层 settings 回 `~/.ultrac/settings.json`（真实凭据所在，无需迁移），project 层 `<cwd>/.ultrac/settings.json` | 用户 2026-10-16 拍板：见偏差① |
> | `internal/session/pi_format.go`（新 ≈480 行） | `Header`（type/version/id/timestamp/cwd/parentSession）、`SessionEntry`（type/id/parentId/timestamp/message + Extra 保真）、`ParseLine`/`MarshalHeader`/`MarshalEntry`、uuidv7 `NewSessionID`（48bit 时间戳 + version/variant + 进程内单调钳位）、8 hex `newEntryID`（100 次碰撞重试）、`EncodeCWD`（pi 算法逐字）、`SessionDirFor`/`SessionFileName` | `session-manager.ts:41-62, 264-284, 589-594, 1079-1080`、`ai/utils/uuid.ts` |
> | `internal/session/pi_manager.go`（新 ≈560 行） | `SessionManager`：`CreateSession`/`OpenSession`/`ContinueRecentSession`/`InMemorySession`；`AppendMessage`（raw `ai.Message` 落盘）/`AppendModelChange`/`AppendThinkingLevelChange`/`AppendSessionInfo`；**懒落盘**（首个 user/assistant 消息才建文件，`O_EXCL` 一次性写 header+全部 entry，之后每行一个 append + fsync）；`GetBranch`（parentId 回溯）；`ReadSessionFile`（坏行容忍、50MB 上限）；v1 迁移 | `session-manager.ts:1057-1196, 1404-1500, 1755-1806` |
> | `internal/session/pi_context.go`（新 ≈240 行） | 上下文投影：`buildPath`（leaf→root 回溯）、`contextEntries`（compaction 窗口：firstKeptEntryId 之前的摘要掉、保留段里的 system message 让位给 compaction 自带的 systemMessage）、`contextMessages`（message/custom_message/branch_summary/compaction 按 `convertToLlm` 转 user 消息，摘要框架常量逐字）、`contextSettings`（thinkingLevel 默认 off、model 取最后 model_change 或 assistant 消息） | `session-manager.ts:390-512`、`core/messages.ts:11-24, 100-196` |
> | `internal/prompt/`（新包，2 文件 ≈470 行） | 分段式提示词：`BuildSystemPromptSections`（preamble 无标签 / tools / rules / docs / addendum / project_context / cwd + 自定义段，非 preamble 包 `<name>` 标签）、`BuildSystemPromptState`（forceSystemPrompt → 不透明 content）、`BuildSystemPrompt`/`RenderSystemPrompt`（按 transcript 回放顺序渲染）、`DiffSystemPromptSections`（变更/新增→文本、缺失→nil 删除、无变更→nil）、8 工具 snippet/guidelines 表（pi 逐字）、`LoadContextFiles`（CLAUDE.md/AGENTS.md） | `core/system-prompt.ts` 全篇、`core/tools/*.ts` 的 *ToolSystemPromptContribution |
> | `internal/session/store.go`（改，bug 修复） | `openForWrite` 的"检查-打开-注册"整体纳入锁内；`session_test.go` 的并发测试补上 Append 错误断言 | — |
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **目录归属：Ultrac 是自有项目，不占用 pi 的目录**（2026-10-16 用户拍板，覆盖 P4 当日决定）。P4 曾按 pi 源码把 user settings 落 `~/.pi/agent/settings.json`、project 层落 `<cwd>/.pi/settings.json`、sessions 落 `~/.pi/agent/sessions/`。用户原话："我是需要把 pi 项目用 go 语言重写然后作为自己的项目，而不是保留 pi。"——因此全部回到 `~/.ultrac/`：settings 在 `~/.ultrac/settings.json`（**现有 DeepSeek 端点与凭据原地生效，零迁移**），project 层 `.ultrac/settings.json`，sessions 在 `~/.ultrac/sessions/`；`PI_CODING_AGENT_DIR`/`PI_CODING_AGENT_SESSION_DIR` 两个 env 删除，只保留 `ULTRAC_CONFIG_DIR` 一个搬迁旋钮（它同时搬 settings 与 sessions）。
> **保留的只有数据格式**：session 文件的 header + entry 树 + uuidv7 + `<encoded-cwd>` 编码仍是 pi 形状——那是已落地的数据布局（43 个测试 + fixture 建立在它上），与"目录归谁"无关；哪天要彻底断绝，改的是 `EncodeCWD` 一个函数和读写两侧，成本可控。**连带影响**：prompt 文案的产品名在 P4 已是 "ultrac"（preamble/docs 段），与此决定一致。
> ② **config 六层保留**（user<project<local<flag<policy），只改路径——§3.5 表格说"简化为 user<project<flag"，§6.2 说"local 与 policy 可保留"，取 §6.2：pi 只读上述两个文件，多层是纯增量不冲突，且这套合并机制有 20+ 测试兜底，删除只有风险没有收益。
> ③ **session 包双词表共存**：旧 `Entry`/`Store`（`~/.ultrac/projects`）原样保留并标 deprecated，新 `SessionManager` 写 pi 形状——R9 要求每阶段可编译，旧引擎（P7 删）仍在用旧 API。
> ④ **`internal/engine/prompt` 未删**，新建 `internal/prompt`；旧包同步标 deprecated。P4 的 prompt 包暂无 CLI 消费者（P5 接线），与 P2/P3 的自底向上节奏一致。
> ⑤ **prompt 文案两处偏离 pi 字面**：产品名 `pi`→`ultrac`（preamble 与 docs 段——跑着的是 ultrac，告诉模型"你在 pi 里"是谎言）；bash/powershell 的 "inspect PI_* environment variables" guideline 删除（P3 未移植 `exposeSessionEnvironment`，留着是指向不存在变量的谎言，代价是一次白白的工具调用）。分段名、顺序、标签、diff 语义与 pi 一致。
> ⑥ **分支写入 API 未做**（`branch`/`resetLeaf`/`createBranchedSession`/`forkFrom`）与 label/usage/context_edit/custom 的**写入**——§2.3 定的子集（"分支树延后"）；**读取侧全部支持**：这些 entry 经 Extra 原样保真，compaction 投影按 pi 算法重建，label 等状态 entry 不进上下文。
> ⑦ **v1→v2 迁移只在内存中做，不重写文件**（pi 的 `_loadEntries` 会 `_rewriteFile`）——追加-only 格式的存在意义就是避免重写；本进程内重建结果等价。v2→v3 的 `hookMessage`→`custom` 改名未实现（无写入方产生该角色，含它的文件按未知角色丢条目，与 pi 的 Unmarshal 容忍同结果）。
> ⑧ **docs 段不列 pi 的文档树**（本构建不随附 `docs/`+`examples/`），改为指向可执行文件旁的 `README.md`/`docs/` 并保留"绝对路径、别按 cwd 解析"的规则；分段机制本身一致。
> ⑨ **skills 段保留名不实现**（§2.2 非目标）；`Options.Sections` 可承载同名自定义段。
>
> **顺手修掉的存量 bug（原始树上复现，非本次改动引入）**：旧 `Store.openForWrite` 的"双重检查"在**锁外**并发 `OpenFile(O_CREATE)`——Windows 上同一路径的并发创建间歇性返回 `Access is denied`（8 个并发 writer 丢 5-7 行，丢的恰是每个 writer 的第一条），而 `Append` 的错误被调用方 `_ =` 吞掉，**静默丢转录行**。修法：检查-打开-注册整体纳入 `s.mu`（临界区只覆盖每路径首次打开）；旧并发测试补上错误断言，让这类 bug 不能再靠吞错误藏住。复现证据： stash 掉本次全部改动后 `TestConcurrentAppendsProduceWholeLines` 仍 193/200。
>
> **测试**：新增 43 个测试函数（prompt 13 / session 23 / config 路径 7）全绿——含 pi fixture 双向验收（手写 pi 形状文件：header+sections 系统消息+model_change+rewind 分支，按 parentId 重建等价 transcript；compaction fixture 验证 firstKeptEntryId 窗口与摘要框架逐字）、Go 写出文件逐条断言 pi 读取规则（parentId 恒在、根为 null、id 8 hex 唯一、首行 header version 3）、懒落盘/只追加、坏行容忍、v1 迁移、uuidv7 版本位与单调性；`go test ./internal/session -count=5` 连跑无抖动；`go build`/`go vet`/`gofmt`/`staticcheck`（全仓）零告警；agent/ai/engine/tool 等旧包测试无回归。

### 3.6 P5 — CLI + print/json 模式 ✅ 已完成（2026-10-16）

**动作**：改造 `cmd/ultrac`（现有 `runAgent` 装配流程可用）+ `internal/transport/sink`（NDJSON/JSON/Text 三 sink 保留）。

**DoD**

- [x] `ultrac -p "prompt"` 输出助手文本，退出码正确
- [x] `ultrac --mode json "prompt"` 每行一个 JSON，类型名对齐 `json-event.ts`
- [x] 会话落盘
- [x] `SIGTERM` 清理子进程并退出（现有 `cmd/ultrac/signal.go` + `cmd/ultrac/agent.go` 已有信号处理，**保留**）
- [x] ⚠️ **U+2028 / U+2029 转义**——现有 `transport/ndjson.go:331` 的 `escapeLineTerminators` **必须保留**

**关卡**：**同一句 prompt，pi 与 ultrac 各跑一次，`diff` 输出。** 性价比最高的验收。

> **P5 落地记录（2026-10-16）**：完成。落地内容、与计划书的偏差及真实端点抓出的两个 bug 如下——
>
> **落地内容**：
>
> | 文件 | 内容 | pi 出处 |
> |---|---|---|
> | `cmd/ultrac/print.go`（新 ≈430 行） | print/json 主路径：settings→session→model→tools→分段提示词→`agent.NewAgent`→订阅→`Prompt`→退出码；system **sections 补丁**逐轮 diff（首发全量、resume 后为空不重复发）；message_end 落盘（system/user/assistant/toolResult 四角色）；`--cwd` 真实生效（tools 解析基目录、session 归属、project 层读取三处都随它走） | `modes/print-mode.ts` 全篇、`core/agent-session.ts:1147-1166`（落盘规则）、`core/agent-session.ts:1742-1754`（`_preparePromptAndToolLoadout`）、`core/sdk.ts:411-458`（Agent 装配 + 新会话 metadata） |
> | `internal/transport/pi_json.go`（新 ≈290 行） | `PiJSONWriter`：agent 事件→pi `AgentSessionEvent` 线型（10 种 discriminant）；`message_update` 剥离 `partial`、`usage` 上提、`toolcall_start` 补 `id`/`toolName`（复用 P0 的 `ai.MarshalEvent`）；`durationMs` 缺席而非 null；U+2028/29 转义（与旧 writer 同一规则） | `modes/json-event.ts` 全篇、`agent/src/types.ts:516-539`、`agent-loop.ts:919-928` |
> | `cmd/ultrac/model.go`（新 ≈160 行） | settings env + `--model provider/id` → `ai.Model`：flag>settings 定 id；provider 取 flag/catalog/host 推断；dialect 默认 anthropic-messages（`ANTHROPIC_*` 约定）；baseUrl settings>catalog>provider 默认；catalog 条目采纳费率但强制回 resolved dialect | `core/models-store.ts` 的合并语义 |
> | `cmd/ultrac/probe.go`（新 ≈330 行） | `ultrac probe` 子命令（P1 承诺落地）：bin/probe 原样移植，含 12 事件覆盖报告与分场景 VERDICT | — |
> | `cmd/ultrac/{flags,main,signal}.go`（改） | flag 面换 pi 语义：`-p/--print`、`--mode text|json`（rpc 拒绝并提示 P6）、`--model`、`--api`、`--cwd`、`-c/--continue`、`--no-transcript`、`--debug`；删 `--permission-mode`/`--dangerously-skip-permissions`/`--add-dir`/`--output-format`；`printSignalContext` 记录信号名 → 130/143/129 退出码 | `cli/args.ts`、`main.ts:112-127`（resolveAppMode）、`print-mode.ts:50-66` |
> | `cmd/ultrac/agent.go`（**删除** 482 行） | 旧 `runAgent`（engine+permission+contract 装配）随 P5 退场——CLI 主路径已换 pi 语义；旧包本身按 R9 留到 P7 | — |
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **不写 `os.Setenv`**：旧路径把 settings env 应用到进程环境是因为旧 provider 客户端从环境读；新栈经 `GetAPIKey` 钩子与 `resolveModel` 取用，print 跑完不污染子进程环境（bash 工具继承的是干净环境）。
> ② **`--api` flag（计划外增补）**：settings 只有 `ANTHROPIC_*` 一族，dialect 默认 anthropic 够用，但 openai-completions 端点无法从 env 表达；一个显式 flag 比猜 base URL 形状诚实。`--max-turns`/`--system-prompt`/`--append-system-prompt` **未移植**（pi 的 print 模式没有这些 flag；`agent.AgentLoopConfig` 也无 maxTurns 字段）。
> ③ **session metadata 时机**：model_change + thinking_level_change 在新会话**开跑前**写（懒落盘保证它们和 header 一起首批落盘），resume 时缺 thinking entry 才补——与 `sdk.ts:453-458` 一致。
> ④ **`transport` 包双协议共存**：旧 `ndjson.go`（Claude Code 方言，旧引擎 P7 前仍在）与新 `pi_json.go` 同包两个 writer，而非一个带 mode 开关的 writer——两种协议不是彼此变体，混在一个 switch 里迟早串味。
> ⑤ **诊断命令（config/sessions/ask/doctor）原样保留**：它们检查的是 T0 地基（旧 provider 客户端、旧 Store），与 agent 路径无关；P7 随旧包一起处置。
>
> **真实端点验收（DeepSeek anthropic 兼容端点，凭据取自 `~/.ultrac/settings.json`）**：
>
> | 场景 | 结果 |
> |---|---|
> | `-p "Reply with exactly: P5-OK"` |  stdout `P5-OK`，exit 0 |
> | `--mode json` 纯文本轮 | 21 行：`session → agent_start → turn_start → [system patch] → [user] → [assistant + 9×message_update] → turn_end → agent_end`，与 pi 事件序列逐一同名 |
> | `-p` 工具轮（read go.mod） |  read 调用→结果回灌→终答，exit 0 |
> | `--mode json` 工具轮 | 40+ 行含 `tool_execution_start`/`tool_execution_end`（`toolCallId`/`toolName`/`args`/`result.content`/`isError`/`durationMs` 齐全） |
> | `-c` 跨进程续话 |  第二轮答出第一轮的 "42"（transcript 落盘+回放验证） |
> | `probe` |  8/12 事件，VERDICT PASS |
> | SIGTERM 中途杀 |  bash 子进程树被清（无残留 sleep），退出码路径见下 |
>
> **关卡（双跑 diff）未执行**：本机 pi 无 `node_modules`/`dist`（未 install/build），且双跑需 pi 侧单独配 `~/.pi`（§10.5 已注明两边不共享路径）。**替代验收**：事件 discriminant 与 pi `AgentSessionEvent` 联合体逐一对照、`json-event.ts` 投影逐字段断言（单测）、真实端点跑出与 pi 同构的完整序列。**pi 安装后的双跑留作 P6/P7 前置**。
>
> **过程中抓出并修掉的两个真 bug**（都是真实端点暴露的，单测全绿期间不存在）：
> ① **`content_block_start` 的块文本被丢弃**（`internal/ai/anthropic/stream.go`）：旧实现注释断言"API 会在紧随的 delta 里重复 start 文本"，实测 DeepSeek 的 `/anthropic` 端点把**整段 thinking 文本放在 start 事件、不发任何 delta**——产出 `thinking: ""` + signature 的块，落盘后第二轮回放被端点以 `missing field 'thinking'` 拒掉整个请求。修法按 pi 原文（`anthropic-messages.ts:696-715`）：text/thinking 都从 start **播种**、delta **追加**（一方 API start 为空串，播种是无操作；DeepSeek 形态保住全文）。旧测试 `TestTextInContentBlockStartIsNotDuplicated` 编码的是这个错误假设，已按 pi 语义重写，并补 `TestBlockTextOnlyInContentBlockStart` 回归。
> ② **空 thinking 块（有 signature）原样回放**（`request.go`）：上面那个坏块进了会话文件后，`-c` 续话即复现 422。修法：非 redacted 且文本为空的 thinking 块**跳过**（signature 守着一段空推理，是流被截断的产物，不是内容；pi 对"丢 signature"的降级是转文本，对"空文本"无可转，只能丢）。补两个单测（跳过/仅空块则报错）。
>
> **测试**：新增 31 个测试函数全绿——transport 9（discriminant/message_update 投影/toolcall_start 身份/durationMs 缺席/header 行/U+2028 逃逸/非 assistant 报错/turn_end 载荷）、cmd 22（print 端到端 11：文本/错误退出码/json 序列/工具闭环/落盘条目/continue 三段/无凭据/无模型/--no-transcript/--cwd 生效与非目录拒绝/sections 注入请求；model 8；flags 6；signalExitCode 1）。`go build`/`go vet`/`gofmt`/`staticcheck`（全仓）零告警；agent/ai/session/tool 等全部既有包测试无回归。
>
> **遗留**：① `internal/agent/agent.go` 曾在会话中被意外写坏（NewAgent 主体被替换），已从 git 恢复并 gofmt（CRLF→LF）——**提交前需 `git diff internal/agent/agent.go` 确认为空**；② pi 侧双跑（§9.5）待 pi 安装后执行。

### 3.7 P6 — RPC 模式 ✅ 已完成（2026-10-16）

**动作**：新建 `internal/rpc/`（`types.go` + `jsonl.go` + `server.go`），复用 P0 冻结的 C4 类型与 P5 的 `PiJSONWriter`；`cmd/ultrac` 抽出共享装配 `assemble.go`，print 与 RPC 同链。

**DoD**

- [x] 命令集至少覆盖：`prompt` / `steer` / `follow_up` / `abort` / `new_session` / `get_state` / `set_model` / `get_messages`
- [x] 响应格式 `{id, type:"response", command, success, data|error}`
- [x] 事件推送与 print/json 复用同一套 writer（`PiJSONWriter.WriteObject`，互斥锁防半行交错）
- [x] 未知命令返回 `success:false` + 明确 error，不崩（解析失败同理，循环存活）

**关卡**：复用 pi 的 `rpc-client.ts` 逻辑写驱动脚本，同一脚本分别驱动 pi 与 ultrac。

> **P6 落地记录（2026-10-16）**：完成。落地内容、与计划书的偏差及验收如下——
>
> **落地内容**：
>
> | 文件 | 内容 | pi 出处 |
> |---|---|---|
> | `internal/rpc/types.go`（新 ≈230 行） | C4 线型：`RpcCommand`（判别 + 全命令字段）、`RpcResponse`（字段序与 pi 字面一致：id/type/command/success/data\|error）、`RpcSessionState`、`ImageContent`；`isCompacting`/`autoCompactionEnabled` 恒 false（compaction 是非目标，诚实常量） | `modes/rpc/rpc-types.ts`（303 行）逐字段 |
> | `internal/rpc/jsonl.go`（新 ≈130 行） | 严格 LF reader：只按 `\n` 分行、剥一个尾随 `\r`、EOF 冲尾行、U+2028/29 不拆分；可增长缓冲无记录上限（bufio.Scanner 的 64KB token 上限会被带图 prompt 打穿） | `modes/rpc/jsonl.ts` |
> | `internal/rpc/server.go`（新 ≈570 行） | `Server`：stdin 循环（reader goroutine + select，ctx 取消可打断阻塞读）→ 派发 18 命令 → 响应；事件泵订阅 agent（`new_session` 换绑）；run 活跃标志 + abort 后等待 settle（有界）；`Host` 接口（pi 的 `AgentSessionRuntime` 角色） | `modes/rpc/rpc-mode.ts`（819 行，去掉 extension_ui 半边） |
> | `cmd/ultrac/assemble.go`（新 ≈290 行） | 共享装配：settings→session→model→credential→tools；**每 prompt 重建 sections 并 diff**（`buildPrompt` 返回 patch+user，`PromptInput`）；`availableModels` = catalog + settings 模型；`resolveCWD`/`persistable`/`recordSessionMeta` 从 print.go 上收 | `core/agent-session.ts:1742-1754`（per-prompt diff）、`sdk.ts:411-458` |
> | `cmd/ultrac/rpc.go`（新 ≈230 行） | `rpcHost`（Host 实现）+ `runRPC`：EOF→0、信号→130/143/129、诊断走 stderr；`new_session` 先 abort 旧 run（**不持锁**等 settle，否则与事件泵死锁）再换新 agent+session | `rpc-mode.ts:366-380`（rebind）、`agent-session-runtime.ts:226-256`（newSession） |
> | `cmd/ultrac/print.go`（重构 ≈330 行） | 删内联装配，改走 `buildHarness`；行为不变（既有 22 个 cmd 测试无改全绿） | — |
> | `internal/agent/agent.go`（+4 方法 ≈65 行） | `SetModel`/`SetThinkingLevel`（pi 的 session 直改 state 的加锁版）+ `SteeringMessages`/`FollowUpMessages`（clear_queue 与 pendingMessageCount 需分队列窥视） | `agent-session.ts:2509`、`:2645` |
> | `internal/session/pi_manager.go`（+1 函数） | `CreateSessionWithParent`（`new_session` 的 parentSession 进 header） | `agent-session-runtime.ts:239-243` |
> | `internal/ai/model.go`（+2 函数 ≈80 行） | `SupportedThinkingLevels`（reasoning 模型才有非 off；xhigh/max 需显式 map 条目）+ `ClampThinkingLevel`（精确→向上就近→向下→首个） | `ai/src/models.ts:1228-1260` |
> | `internal/transport/pi_json.go`（+1 方法） | `WriteObject`：响应与事件同一 writer、同一 U+2028/29 规则出线（P5 注释预留的 P6 接口） | `rpc-mode.ts:60-63` |
>
> **与计划书的偏差（都是决策，不是遗漏）**：
> ① **命令串行处理**（pi 每行 fire-and-forget 并发）：驱动脚本本就是「发一条等一条」，串行无行为差异且无竞态；`prompt` 仍是即发即走（goroutine 跑 run，**响应先于事件**上线——handleLine 先写响应再 launch）。
> ② **abort 响应前等待 run settle**（有界 10s）：pi 的 `session.abort()` 是 await 的；先返回会让紧随的 prompt 撞 `errAlreadyProcessing`。
> ③ **`new_session` 换新 Agent 而非 `Reset()`**：Reset 保留 system 基线会让新会话 sections 补丁 diff 为空、文件里却没有 system message——resume 后对不上。镜像 pi 整机换新（新 agent 不带 seed messages，首发补丁重新全量）。
> ④ **`BuildPrompt` 返回 error**：per-prompt sections 重建失败时转错误响应，不静默降级发残缺提示词。
> ⑤ **未移植 16 个命令**（compact/set_auto_compaction/set_auto_retry/abort_retry/bash/abort_bash/export_html/switch_session/fork/clone/get_fork_messages/get_tree/get_session_stats/cycle_model/cycle_thinking_level/extension_ui_*）：compaction 与分支树是非目标或 P4 延后项；它们落未知命令分支，客户端得到明确 error 而非静默吞掉。`get_commands` 返回空表（无扩展/模板/skill，诚实答案）。
> ⑥ **RPC 不写 session header 行**：只有 print json 模式写（`rpc-mode.ts` 无 WriteHeader 调用），已核实。
>
> **测试**：新增约 40 个测试函数全绿——`internal/rpc` 21（jsonl 5：LF-only/U+2028 不拆/CRLF/尾行/跨读分片；派发 16：未知命令/坏行/响应形状/id 回声/prompt 响应先于事件/流式中入队与报错/steer+clear_queue/abort 后循环存活/get_state 全字段/new_session 换绑/get_messages+last_text/set_session_name/get_entries since+leaf/set_model 找到与找不到/thinking clamp 与列表/queue mode 校验/图片入 content/get_commands 空/EOF 返回/ctx 取消返回）+ agent 2 + session 1 + transport 2 + cmd 5（sections 补丁生命周期：首发全量→不变为空→AGENTS.md 改动再补/图片消息/availableModels 含 settings 模型/persistable/resolveCWD）。全部测试用真 Agent + 假 provider（P2 模式），无真实 API 调用。`go build`/`go vet`/`gofmt` 零告警；存量测试无回归（cmd/ultrac 22 个 print 测试未改一字）。
>
> **真实端点验收（DeepSeek anthropic 兼容端点，凭据取自 `~/.ultrac/settings.json`）**：驱动脚本 `bin/rpc-driver.ts`（自包含 TS，镜像 `rpc-client.ts` 的严格 JSONL + id 关联 + 事件收集；只等 agent 级 discriminant，pi 装好后可直接双跑）——**38/38 断言通过**：get_state 全字段（含诚实常量）、prompt→agent_end→`get_last_assistant_text` 拿到真实回答（"你好呀"）、transcript 为 system(tool 装载)→system(sections 补丁)→user→assistant（与 pi `createInitialSystemMessage` 的播种规则逐字一致）、队列三连、`set_model` 热切换（catalog 内 anthropic/claude-sonnet-4-5 实测可用，切回后继续答题）、abort 后循环存活、new_session 换 id 且 transcript 干净、session_info/entries/leafId、未知命令与坏行的 error 响应、7 种 agent 事件 discriminant 齐全、stdin EOF 退出码 0。信号路径：Windows 上 MSYS `kill -TERM` 是硬终止模拟（最小 Go 程序实证收不到），优雅路径与 print 模式共享 `printSignalContext`+`signalExitCode`，单测覆盖 cancel→返回→退出码链路。
>
> **关卡（双跑 diff）仍未执行**：本机 pi 无 `node_modules`/`dist`（未 install/build）。**替代验收**：协议逐字段对照 + 38 断言真实端点全过 + 驱动脚本已按双跑兼容形态落盘（`bin/rpc-driver.ts`，pi 安装后 `bun bin/rpc-driver.ts <pi-cli> <cwd>` 即可同脚本双跑，注意需按共同 discriminant 过滤 pi 多发的 session 级事件——脚本注释已写明）。
>
> **遗留**：① pi 侧双跑（§9.5）待 pi install/build 后执行；② `get_commands`/`get_entries` 的线型与 pi 的逐字段对照依赖 P4 的 fixture 规则，双跑时需一并验证。

> **P6 复核审计记录（2026-10-18）**：按 DoD 逐项复验 + 重建二进制重跑驱动脚本，**38/38 断言仍全过**（真实端点）。抓到 3 处与落地记录"staticcheck 零告警"不符的问题，已修：
> ① `cmd/ultrac/rpc.go` `set_model` 未找到的 error 文案大写触发 ST1005——pi 线上 literal 就是 `Model not found: ${provider}/${modelId}`（`rpc-mode.ts:474`、`docs/rpc.md:78`），C4 逐字段对照不能改文案，按"协议保真优先于 Go 风格"加 `//lint:ignore ST1005` 并注明出处；
> ② `internal/rpc/rpc_test.go` `serverDriver` 两个未用字段（`mu`/`all`，U1000）删除。
> 修后 `go build`/`go vet`/`staticcheck` 全零，全量 20 包测试无回归。P6 维持 ✅ 完成判定；双跑关卡顺延 P7 不变。

### 3.8 P7 — 退场清理与端到端验收 ✅ 已完成（2026-10-18）

**要删的**（这一步不做完，计划不算完成）：

| 路径 | 行数 | 前置条件 |
|---|---:|---|
| `internal/permission/` | 906（+302 测试） | 新 `AgentTool` 无 `canUse` 引用；`cmd/ultrac/agent.go` 的 `resolvePolicy`/`policyFromSettings` 删除 |
| `internal/shell/` | 690（+112 测试） | `permission` 与新引擎均不再引用 `shell.Analyze` |
| `internal/contract/{provider,tool,event}/` | 841（+199 测试） | 全部引用改指 `internal/ai` + `internal/agent` |
| `internal/engine/` + `internal/engine/prompt/` | 1,450（+991 测试） | 由 `internal/agent` + `internal/prompt` 取代 |
| `internal/tool/builtin/` 的 5 个工具 | ≈ 1,460 | pi 8 工具落地 |
| `cmd/ultrac/agent.go` 的权限装配 | ≈ 90 | |

**DoD（端到端）**

| 验收项 | 方式 | 判据 |
|---|---|---|
| E1 真实编码任务 | 真实小需求，agent 自主 read→edit→bash 验证 | 产物正确、无需人工干预 |
| E2 事件序列对照 | 同一 prompt 双跑 | 事件类型序列 diff 为空 |
| E3 会话互操作 | 交叉读写 JSONL | 重建 transcript 一致 |
| E4 工具行为对照 | 8 个工具各一组输入 | 输出逐字一致（路径分隔符除外） |
| E5 取消与泄漏 | 中途 abort，goroutine 计数 | 无泄漏 |
| E6 静态检查 | `go vet` + `staticcheck` | 零告警 |
| E7 权限层消失 | `grep -r "internal/permission"` | 零命中 |

> **P7 落地记录（2026-10-18）**：完成。删除、端到端验收、与计划书的偏差如下——
>
> **一、退场清理（`git rm`，44 个文件 / 13,255 行删除，仓库可 review 可回退）**
>
> | 路径 | 说明 |
> |---|---|
> | `internal/permission/` | 4 文件，随 engine 删除后零消费者 |
> | `internal/shell/` | 2 文件，最后消费者是 permission/engine/builtin |
> | `internal/contract/{event,provider,tool}/` | 3 包 6 文件 |
> | `internal/engine/` + `prompt/` | 6 文件 |
> | `internal/llm/anthropic/` | 6 文件（P1 起已由 `internal/ai/anthropic` 取代） |
> | `internal/tool/builtin/` | **整包 16 文件**（计划书写"5 个工具 ≈1,460 行"；engine 删除后全包零 import，留 11 个死文件不符合"退场"本意，整包删） |
> | `internal/transport/ndjson.go` + `sink/` | 旧 Claude Code 方言 writer，生产零消费者（只剩自身测试），随 engine 退场 |
>
> `cmd/ultrac/main.go` 是唯一需要改代码的地方：删 `ask`（P5 偏差⑤的"诊断命令 P7 随旧包一起处置"，`probe` 已全面取代）、删 `doctor`（检查的是已删的 T0 地基）、**重写 `sessions`** 列新目录（`SessionDirFor` + `ReadSessionFile`，旧 Store 指向的 `~/.ultrac/projects` 已无人写入）；`signalContext` 随 ask 删除成了死代码一并移除。`go mod tidy` 去掉 `mvdan.cc/sh/v3` 及其 4 个传递测试依赖，`go.mod` 只剩 go-cmp / jsonschema / x/text。
>
> **二、端到端验收**
>
> | 项 | 结果 | 证据 |
> |---|---|---|
> | E1 | ✅ 两轮真实端点（DeepSeek）：轮一 edit→bash 验证；轮二不透露问题位置，agent 自主 ls→read×2→edit→bash×3 闭环，退出码 0，产物正确 | `~/.ultrac/sessions/<encoded-cwd>/`  transcript：1 system + 1 user + 5 assistant + 7 toolResult |
> | E2 | ✅ 双跑 diff 为空：`session → agent_start → turn_start → (message_start/message_end)×3 → message_update* → message_end → turn_end → agent_end → agent_settled` | `bin/double-run.sh`（本机可复现） |
> | E3 | ✅ 会话互操作 7 个 round-trip 测试（header/entry/Extra 保真/未知类型/自有文件重建） | `internal/session` 全绿 |
> | E4 | ✅ bash 产物逐字一致（`'go.mod\nmain.go\n'` 含行尾换行）；延迟输出双跑 partial 流**逐字节一致**（4 拍：`'' → 'start\n' → 'start\nmiddle\n' → 'start\nmiddle\nend\n'`） | 同上双跑脚本 |
> | E5 | ✅ abort 无泄漏 3 测试 + `-count=3` 连跑 | `internal/agent` 全绿 |
> | E6 | ✅ `go vet` + `staticcheck`（全仓）零告警，`gofmt` 干净 | — |
> | E7 | ✅ `internal/permission\|internal/shell\|mvdan.cc/sh` 在 Go 代码零命中（仅剩 6 处历史出处注释） | grep |
>
> **三、双跑抓出并修掉的两个真协议 bug**（这正是 E2/E4 存在的意义）：
>
> 1. **缺 `agent_settled` 终端事件**——pi 的 print json 流以 `agent_end` + `agent_settled{aborted}` 收尾（`agent-session.ts:203`，`_emitAgentSettled` 无条件转发），ultrac 只发到 `agent_end`。修复：`internal/agent` 加 `EvAgentSettled`（三条退出路径 + 失败路径都补发，aborted 取自 StopReason），`transport/pi_json.go` 加有线映射，7 个测试文件的终序断言同步更新。
> 2. **bash 工具缺 partial 流式**——pi 的 bash 运行中经 `onUpdate` 发节流快照（启动一个空 partial，之后仅在有新输出时发，100ms 节流，结束补发，`bash.ts:283-320`）；ultrac 的 Execute 一直把回调丢了（`_`）。修复：补 dirty 门控的 100ms 轮询快照（`bashPartialResult`），新增 `TestBashPartialUpdates`。文件头"偏离清单"里"无实时输出流式"一条随之改写。
>
> **四、pi 双跑环境搭建（本机从零，全部留痕于 `bin/double-run.sh` 注释）**：`npm install --ignore-scripts`（`canvas` 原生编译本机装不上——缺 C++ 负载，而它是 ai 包的**测试用** devDependency）；`packages/ai` 的 `build` 每次重新 `generate-models`，线上数据漂移撞碎 `opencode-go.ts` 的类型 union（models.dev 新增 `google-generative-ai` 模型），解法是过滤生成数据 + 用 pi 自己的 `createModelDataManifest` 重算清单，再手工链剩余 7 包；**provider id 撞车坑**：models.json 里的 `deepseek` 会被 pi 内置目录接管（路由打到 `/anthropic/chat/completions` → 404），必须改名（用 `dsanthropic`）；**pi 的 print 模式等 stdin**，必须 `</dev/null`，否则挂起被超时 SIGTERM。
>
> **五、P7 之后的 TS 树退场（2026-10-18 当晚，用户提议并执行）**：§0.2/R11 判决的落地——`src/`（1,953 个跟踪文件，~513k 行 TS）、`package.json`、`tsconfig.json`、`bunfig.toml`、`bun.lock` 经 `git rm` 删除（history 可恢复）；gitignored 产物 `node_modules/`（542MB）、`dist/cli.js`（29MB）、根目录旧 `ultrac.exe` 从磁盘清除。连带收尾：Makefile 的 `live` 目标（还在调 P7 已删的 `doctor`/`ask`）删除；README 重写为 Go 引擎视角；`.gitignore` 去掉 TS 时代死条目（node_modules/dist），并**移除 `docs` 忽略规则 + force-add 本计划书**（此前它不在版本控制内，而另外 5 个文档却在——不一致状态，且 README 链着它）。保留：`bin/` 下的本地工具（`double-run.sh`、`rpc-driver.ts`——自包含无 import，唯一的 RPC 双跑驱动）、`.ultrac/`、`.codegraph`、`eli5/`。删后 `go build/vet/test ./...` 全仓可跑（不再需要 node_modules 范围守卫），staticcheck 零告警。

### 3.9 里程碑

| 里程碑 | 完成判据 | 可演示的东西 |
|---|---|---|
| **M1 = P0+P1** | 能对真实 API 打出流式事件 | `ultrac probe` 打印 12 种事件 |
| **M2 = P0…P3** | agent 自主调工具完成多轮任务 | 假 provider 下的完整事件序列 |
| **M3 = P0…P5** | 无头模式完成真实编码任务 | `ultrac -p "..."` 与 pi 输出 diff 一致 |
| **M4 = P0…P7** | RPC 可被外部驱动，双跑通过，旧代码清空 | 驱动脚本同时驱动两个 harness |

### 3.10 三种投入场景

| 场景 | 覆盖 | 产出 |
|---|---|---|
| 最小验证 | P0–P2 | 证明 Go 能等价复刻 pi 的 agent loop |
| **推荐** | P0–P5 | 一个可用的无头 Go coding agent（pi 语义） |
| 完整 | P0–P7 | 可被外部集成的 harness + 仓库清理完毕 |

---

## 4. 架构设计

### 4.1 改造后的包布局

```
Ultrac/
├── cmd/ultrac/            入口：print / json / rpc / probe（沿用，参数改 pi 的）
└── internal/
    ├── ai/                ← 新建：pi 的 LLM 层（替代 contract/provider + llm/anthropic）
    │   ├── types.go           C1
    │   ├── event.go           C2：12 种事件 + EventStream
    │   ├── context.go         normalizeContext（P0）
    │   ├── model.go           Model 类型（P0）
    │   ├── stream.go          StreamFn 契约 + StreamRequest 注册表入口（P1）
    │   ├── registry.go        按 Api 注册 provider（P1）
    │   ├── cost.go            CalculateCost（P1）
    │   ├── estimate.go        上下文 token 估算 + max_tokens 收敛（P1）
    │   ├── catalog.go         手写起步模型目录 + 覆盖（P1）
    │   ├── anthropic/         ← 从 internal/llm/anthropic 迁移改造（P1）
    │   └── openai/            ← 新增（P1）
    ├── agent/             ← 新建：pi 的循环与状态机（替代 internal/engine）
    │   ├── types.go           C3
    │   ├── loop.go            runLoop / streamAssistantResponse
    │   ├── toolcall.go        prepare / execute / finalize
    │   └── agent.go           Agent 状态机
    ├── tool/              ← 改造：pi 的 8 个工具（替代 tool/builtin）
    ├── prompt/            ← 提升：从 engine/prompt 移出，改分段式
    ├── session/           ← 复用 + 改造
    ├── config/            ← 复用 + 改造
    ├── logging/           ← 全量复用
    ├── transport/         ← 复用 + 改造（含 sink/）
    ├── rpc/               ← 新建：RPC 模式
    └── version/           ← 全量复用
```

**被删除**：`internal/permission/`、`internal/shell/`、`internal/contract/*`、`internal/engine/`、`internal/llm/`（内容迁到 `internal/ai/anthropic`）。

### 4.2 分层与依赖方向（**只允许向下依赖**）

```
        cmd/ultrac
            │
   ┌────────┼────────┬─────────┐
   ▼        ▼        ▼         ▼
 rpc    transport  config   session
   │        │        │         │
   └────────┴────────┴────┬────┘
                          ▼
                      agent ──────► ai
                        │            │
                        ▼            ▼
                      tool     anthropic / openai
```

**硬约束**

- `tool` **不得** import `agent`（真回路）。工具需要的引擎能力走**注入点**。
- `agent` **不得** import 任何具体 provider；只依赖 `ai` 的 `StreamFn` 接口。
- provider 适配器 **不得** import `agent`。
- `session` / `config` / `prompt` 不得 import `rpc` / `transport`。
- **`agent` 不得 import `permission` / `shell`**——它们将被删除（G6）。

### 4.3 核心接口设计（Go）

**消息与内容**（C1）

```go
type Role string

const (
    RoleSystem     Role = "system"
    RoleUser       Role = "user"
    RoleAssistant  Role = "assistant"
    RoleToolResult Role = "toolResult"
)

type ContentBlock struct {
    Type        string // "text" | "thinking" | "image" | "toolCall"
    Text        string
    Thinking    string
    ThinkingSig string
    Redacted    bool
    Data        string // base64
    MimeType    string
    ToolCallID  string
    ToolName    string
    Arguments   map[string]any
}

type StopReason string

const (
    StopPending StopReason = "pending"
    StopStop    StopReason = "stop"
    StopLength  StopReason = "length"
    StopToolUse StopReason = "toolUse"
    StopError   StopReason = "error"
    StopAborted StopReason = "aborted"
)

type Message struct {
    Role         Role
    Content      []ContentBlock
    Sections     map[string]*string // nil 值 = 删除该分段
    ToolsAdded   []Tool
    ToolsRemoved []ToolRef
    ToolCallID   string
    ToolName     string
    IsError      bool
    API          string
    Provider     string
    Model        string
    Usage        Usage
    StopReason   StopReason
    ErrorMessage string
    TimestampMS  int64
}
```

> JSON tag 必须与 pi 字面一致（`"toolCall"` / `"toolResult"` / `"stopReason"`…），否则 C4 的对照验收不成立。

**流式事件**（C2）——12 种，全部带 `ContentIndex`（除 `start`/`done`/`error`）

```go
type AssistantEvent interface{ eventName() string }

type EvStart         struct{ Partial Message }
type EvTextStart     struct{ ContentIndex int; Partial Message }
type EvTextDelta     struct{ ContentIndex int; Delta string; Partial Message }
type EvTextEnd       struct{ ContentIndex int; Content string; Partial Message }
type EvThinkingStart struct{ ContentIndex int; Partial Message }
type EvThinkingDelta struct{ ContentIndex int; Delta string; Partial Message }
type EvThinkingEnd   struct{ ContentIndex int; Content string; Partial Message }
type EvToolCallStart struct{ ContentIndex int; Partial Message }
type EvToolCallDelta struct{ ContentIndex int; Delta string; Partial Message }
type EvToolCallEnd   struct{ ContentIndex int; ToolCall ContentBlock; Partial Message }
type EvDone          struct{ Reason StopReason; Message Message }
type EvError         struct{ Reason StopReason; Error Message }
```

**事件流**——沿用 Ultrac 已验证的 `iter.Seq2`（注释里写清了理由：消费者停读即取消，不会留下阻塞在无人接收 channel 上的 goroutine）

```go
type Stream interface {
    Events() iter.Seq2[AssistantEvent, error]
    Result() (Message, error) // 终止后才可调用
}
```

**Provider 契约**（= pi 的 `StreamFn`）

```go
type StreamFn func(ctx context.Context, model Model, tc TranscriptContext, opts StreamOptions) Stream

// 契约（写进包注释，违反即 bug）：
//   1. 返回的 Stream 永不为 nil，且永不 panic。
//   2. 请求/模型/运行时错误必须编码为 EvError + StopReason error|aborted。
//   3. 除首个 delta 之前，不得重放已发出的内容。
```

**工具契约**（C3）

```go
type Tool struct {
    Name        string
    Description string
    Parameters  json.RawMessage // JSON Schema
    Sampling    *ConstrainedSampling
}

type AgentTool[In any] struct {
    Tool
    Label         string
    ExecutionMode ExecutionMode // "parallel"（默认）| "sequential"

    PrepareArguments func(raw map[string]any) (In, error) // 可为 nil
    // 失败必须返回 error；不得把错误文本塞进 Result.Content
    Execute func(ctx context.Context, toolCallID string, in In, onUpdate func(Result)) (Result, error)
}

type Result struct {
    Content           []ContentBlock
    Details           map[string]any
    StructuredContent any
    IsError           bool
    Terminate         bool
}
```

> 与现有 `tool.Tool[In]` 的关系：**`AgentTool[In]` 是它的子集再塑造**。现有契约的 12 个谓词里，只有 `IsConcurrencySafe` 有 pi 的对应物（`executionMode`）；`IsReadOnly`/`IsDestructive`/`IsOpenWorld`/`IsSearchOrRead`/`RequiresUserInteraction`/`InterruptBehavior`/`SearchHint`/`Aliases` 的**唯一消费者是权限层与 UI 折叠**，两者本轮都不做，因此一并去掉。

**Agent 状态机**

```go
type Agent struct { /* state + 订阅者 + 两条队列 + 取消 */ }

func (a *Agent) Subscribe(fn func(ctx context.Context, ev AgentEvent) error) (unsubscribe func())
func (a *Agent) Prompt(ctx context.Context, msgs ...Message) error
func (a *Agent) Continue(ctx context.Context) error
func (a *Agent) Steer(msg Message)
func (a *Agent) FollowUp(msg Message)
func (a *Agent) Abort()
func (a *Agent) WaitForIdle(ctx context.Context) error
func (a *Agent) State() AgentState
```

### 4.4 并发与流式映射

| pi（TS） | Go | 现有 Ultrac 可否参照 |
|---|---|---|
| `AbortSignal` | `context.Context` | ✅ 现有已用 |
| `Promise.all(toolCalls)` | `sync.WaitGroup` + 索引数组 + 信号量 | ✅ `engine/tools.go` 的 `runBatch` 已是这个形状 |
| `for await (const ev of stream)` | `for ev, err := range seq` | ✅ 现有 `iter.Seq2` |
| `performance.now()` | `time.Since` | ✅ |
| 进度从 worker 发事件 | channel 汇集到循环 goroutine | ✅ `engine/tools.go:91-104` 已有正确做法 |
| `EventStream.push/end` | `iter.Pull` 或回调缓冲 | ⚠️ 需新建 |

**并行执行的关键实现**（对应 §1.1 第 3 条）：

1. **串行 preflight**：按源序逐个 `prepareToolCall`（查工具 → 参数转换 → schema 校验 → `beforeToolCall`）。
2. **并发执行**：通过 preflight 的调用各起一个 goroutine，**沿用现有 `maxParallelTools = 12` 信号量**。
3. **完成序发 `tool_execution_end`**：每个 goroutine 完成即通过加锁 sink 发出（区别于现有"批次完成后统一发"）。
4. **源序发 toolResult 消息**：全部结束后按 preflight 索引重建数组，顺序落盘（现有已如此）。

### 4.5 现有代码 → 新结构的文件级映射

| 现有文件 | 行数 | 去向 |
|---|---:|---|
| `internal/contract/provider/provider.go` | 245 | → `internal/ai/types.go` + `context.go`（换语义） |
| `internal/contract/event/event.go` | 195 | → `internal/ai/event.go` + `internal/agent/types.go`（拆分） |
| `internal/contract/tool/tool.go` | 401 | → `internal/agent/types.go`（`AgentTool`，大幅简化） |
| `internal/engine/engine.go` | 229 | → `internal/agent/agent.go` |
| `internal/engine/loop.go` | 191 | → `internal/agent/loop.go`（加 steering/followUp/terminate） |
| `internal/engine/turn.go` | 191 | → `internal/agent/loop.go` 的 `streamAssistantResponse` |
| `internal/engine/tools.go` | 479 | → `internal/agent/toolcall.go`（**去掉 permission 相关约 200 行**） |
| `internal/engine/prompt/prompt.go` | 360 | → `internal/prompt/prompt.go`（改分段式） |
| `internal/llm/anthropic/*.go` | 1,203 | → `internal/ai/anthropic/*.go`（事件粒度升级） |
| `internal/tool/builtin/{file_read,bash,file_write,file_edit,grep,glob,diff,match,walk}.go` | ≈ 2,100 | → `internal/tool/*.go`（换包装） |
| `internal/tool/builtin/spec.go` | 222 | → `internal/agent/types.go` 或 `internal/tool/registry.go` |
| `internal/tool/builtin/{web_fetch,web_search,todo,agent,ask}.go` | ≈ 1,200 | **删除**（可选增补） |
| `internal/permission/*.go` | 906 | **删除** |
| `internal/shell/*.go` | 690 | **删除** |
| `internal/session/*.go` | 1,157 | 保留，改目录与 entry 字段 |
| `internal/config/*.go` | 1,616 | 保留，简化层级 + 改路径 |
| `internal/logging/*.go` | ≈ 400 | 原样保留 |
| `internal/transport/*.go` | 688 | 保留，事件字段按 pi 对齐 |
| `cmd/ultrac/{main,flags,signal}.go` | ≈ 770 | 保留，参数改 pi 的 |
| `cmd/ultrac/agent.go` | 482 | 保留装配流程，**删掉权限相关 ≈ 90 行** |

---

## 5. 依赖选型

### 5.1 原则

1. **能用标准库就用标准库。** 核心 harness 的依赖应当少到能在一屏内列完。
2. **不为"未来可能需要"引入依赖。** 不做 TUI → 不引 bubbletea；删了权限 → 不需要额外的 shell 解析库。
3. **外部可执行文件（`rg` / `fd`）保持为外部依赖，不做内建。** pi 会自动下载，**本项目不下载**，缺失时报错并提示安装。

### 5.2 主选型表

| 用途 | pi 的依赖 | 推荐 | 理由 |
|---|---|---|---|
| CLI 参数 | — | **标准库 `flag`** | 现有 `cmd/ultrac/flags.go` 已是标准库风格，沿用 |
| JSON Schema 校验 | `typebox` | **`github.com/santhosh-tekuri/jsonschema/v6`** | 纯 Go、先编译后校验、错误可读；对比 `xeipuuv/gojsonschema`（更老、维护弱） |
| Schema 定义 | `Type.Object(...)` | **手写 JSON Schema 常量** | 避免反射从 struct 反推 schema（会把校验语义藏起来）；schema 是契约，应显式 |
| HTTP | `undici` | **标准库 `net/http`** | 现有 anthropic 客户端已用标准库 |
| SSE 解析 | 各家适配器内联 | **标准库 `bufio.Scanner` + 手写** | ✅ **现有 `internal/llm/anthropic/sse.go` 已实现，直接迁移** |
| 并发编排 | `Promise.all` | **标准库 `sync`** | ✅ 现有已用 |
| diff（edit 工具） | `diff@8` | **`github.com/sergi/go-diff`** | 现有 `builtin/diff.go` 的做法需核实，若已够用则不引 |
| glob 匹配 | `minimatch`/`ignore` | **`github.com/bmatcuk/doublestar/v4`** | 支持 `**`；现有 glob 实现若已够用则不引 |
| 日志 | 自研 | **标准库 `log/slog`** | ✅ 现有 `internal/logging` 已实现，沿用 |
| 测试 | `vitest` | **标准库 `testing`** + **`github.com/google/go-cmp`** | go-cmp 的 diff 输出对"逐字段对照"验收极有价值 |
| Lint | `biome` | **`staticcheck`** | ✅ 已在用；注意它**默认不报"值/指针接收器混用"** |
| 终端着色 | `chalk` | **`github.com/fatih/color`**（可选） | print 模式基本不需要 |

**移除的依赖**：`mvdan.cc/sh/v3`（唯一新增依赖，用于 `internal/shell` 的命令分类）——随权限层一起删除。

### 5.3 明确放弃

| 项 | 决定 | 说明 |
|---|---|---|
| `bubbletea` / `lipgloss` | **放弃** | 不做 TUI |
| `cobra` / `urfave/cli` | **放弃** | 参数面小 |
| `mvdan.cc/sh/v3` | **删除** | 权限层移除后无消费者 |
| `x/sync/errgroup` | **可选** | 仅当并行工具的错误聚合变复杂时 |
| OAuth 相关库 | **放弃** | 只做 API key |
| 自动下载 `rg` / `fd` | **放弃** | 改清晰报错 |

**依赖总量预估**：运行时 ≤ 3 个（jsonschema、diff、doublestar，其中后两个待核实现有实现后再定），测试 1 个（go-cmp）。**比现状净减少 1 个（mvdan/sh）。**

---

## 6. 数据与对外接口

### 6.1 会话存储

**改造为 pi 的格式**（G3 验收的前提）：

```
~/.ultrac/sessions/<encoded-cwd>/<时间戳>_<sessionId>.jsonl
```

> 目录根 2026-10-16 从 `~/.pi/agent/sessions/` 改回 `~/.ultrac/sessions/`：Ultrac 是自有项目，
> 不占用 pi 的目录。文件**格式**（下面的 header + entry 树）保留 pi 形状——那是数据布局，
> 不是品牌，且已有测试与 fixture 建立在它上。

- 首行：header（会话元信息）
- 其后每行：一个 entry
- 目录名由 cwd 编码（**必须与 pi 的算法一致**，否则同一项目的会话会分裂到两个目录）

**保留现有机制**（`internal/session/store.go`，868 行）：

- `Append` / `Flush` / `Read` / `List` 的分层设计
- 只追加（`O_APPEND`），不改历史行
- `BuildChain` 按 `ParentUUID` 回溯（不信文件顺序——rewind 后文件里留着被放弃的消息）
- `Entry.Extra` 原样保留未知字段

**需改**：目录常量、entry 类型词表、header 行、时戳命名。

### 6.2 配置解析

| 优先级 | 来源 | 现状 |
|---|---|---|
| 低 | `~/.ultrac/settings.json` | 现有六层里的 user 层 |
| 中 | `<cwd>/.ultrac/settings.json` | 现有 project 层 |
| 高 | 命令行 flag / 环境变量 | 现有 flag 层 |

> 路径 2026-10-16 定稿：Ultrac 自有 `~/.ultrac/`，不占用 pi 的 `~/.pi/`（见 §3.5 偏差①）。
> `ULTRAC_CONFIG_DIR` 是唯一搬迁旋钮，同时搬 settings 与 sessions。

**现有六层（user<project<local<flag<policy）中，`local` 与 `policy` 两层可保留**（多出来的能力，pi 没有，但不冲突）。**env 不是设置层，是设置层的应用对象**——现有实现已经这么建模，保留。

**DoD 要点**：留空字段 = 删除该键；**写回保留未知字段**（现有 `Extra` 机制，别丢）。

### 6.3 CLI 接口

```
ultrac                              # 保留现有交互入口（若有）
ultrac -p "prompt"                  # 单次打印
ultrac --mode json "prompt"         # JSON 事件流
ultrac --rpc                        # RPC 模式
ultrac probe                        # 自定义：打真实 endpoint 并打印 12 种事件
ultrac --model <provider>/<model>
ultrac --cwd <dir>
```

> 保留现有 `--output-format stream-json|json` 与 `--permission-mode` 之外的参数；`--permission-mode` / `--dangerously-skip-permissions` / `--add-dir` **删除**（权限层退场）。

### 6.4 print / json 事件协议

`--mode json` 每行一个 JSON 对象，类型名对齐 `modes/json-event.ts`。**这是对照验收的核心面。**

### 6.5 RPC 协议

| 方向 | 形态 |
|---|---|
| stdin | 每行一个 `RpcCommand` |
| stdout | 每行一个 `RpcResponse` 或事件 |

最小命令集：`prompt` / `steer` / `follow_up` / `abort` / `new_session` / `get_state` / `set_model` / `get_messages`。

---

## 7. 性能与并发

| 项 | 目标 | 做法 |
|---|---|---|
| 流式延迟 | 首 token 到达即输出 | 事件流不经缓冲层；`stdout` 立即 flush（现有 `transport` 已做） |
| 工具并行 | 读类工具真并发 | 两段式 + **沿用 12 并发上限** |
| 大输出 | 不撑爆内存 | 工具输出超阈值转临时文件（现有 `output-accumulator` 思路对应 pi 的 `output-accumulator.ts`） |
| 子进程 | 超时可杀 | `exec.CommandContext` + 输出重定向到 `*os.File`（**不要 `bytes.Buffer`**；现有 `bash.go` 已如此） |
| goroutine | 无泄漏 | 每阶段测试跑 goroutine 计数断言 |
| 启动 | 快 | 静态二进制；模型目录按需加载 |

**基准门禁**：记录 `ultrac probe` 的首 token 延迟与工具吞吐，作为回归基线。

---

## 8. 风险与应对

| # | 风险 | 症状 | 应对 |
|---|---|---|---|
| **R1** | tool_use 参数分片累积 | 事件里参数是半截 JSON，或 `O(n²)` 卡顿 | **按 content block index 分桶攒字符串**，只在 `content_block_stop` 发完整调用。**现有实现已正确，改造时别退化** |
| **R2** | system message 分段 / 工具装载宣告 | 简化后行为与 pi 不一致，会话互操作失败 | P2 **先实现**（不是先简化再补）；这是 pi 的核心设计之一 |
| **R3** | thinking signature 回灌 | 开 thinking 时 API 拒绝**整个**请求 | **本轮必做**（现有代码在 `engine/loop.go:105-113` 明写"未回灌"） |
| **R4** | 截断（`length`）时的工具调用 | 半截参数被执行，造成破坏 | 该批全部判失败、不执行（§1.1 第 5 条） |
| **R5** | edit 工具的模糊匹配 | 精确匹配失败时行为不可预期 | 对齐 pi `edit-diff.ts`（556 行）：多匹配拒绝 > 唯一模糊匹配 > 报错并附最接近片段。**现有 `file_edit.go` 只有精确替换** |
| **R6** | `bytes.Buffer` 收集子进程输出 | 超时杀不掉，`Wait()` 卡到子进程自然结束 | **输出重定向到临时文件**。这是 Windows/Unix 通病；**现有 `bash.go` 已正确，改造时别退化** |
| **R7** | 会话文件重写 | rewind 后被放弃的消息仍在文件里，按文件顺序重建会错 | transcript 重建**按 `ParentUUID` 回溯**（现有 `BuildChain` 已实现，保留） |
| **R8** | U+2028 / U+2029 | JS 读端从中间截断 | NDJSON 编码时转义（**现有 `transport/ndjson.go:331` 已实现，保留**） |
| **R9** | **改造期编译断裂** | 一次性大爆炸式替换导致长时间无法构建 | **每个阶段必须编译通过**；旧包加 `Deprecated` 标记而非立即删除，P7 统一清理 |
| **R10** | **丢失已验证的坑** | 重写时把 `bytes.Buffer`、文件顺序重建、U+2028 这些坑又踩一遍 | 本计划 §1.2 / §8 明确标注了 5 处"现有实现已正确，别退化" |
| **R11** | **TS 侧定位失配** | 改造后 Go 引擎不再服务 Ultrac TS TUI | **已在 §0.2 明示**；若将来仍需 Ultrac TS 兼容，另立里程碑 |
| **R12** | `rg` / `fd` 缺失 | grep/find 直接不可用 | 启动时探测，缺失给明确安装提示；**不自动下载** |
| **R13** | JSON Schema 语义差异 | typebox 的 `Type.Optional` 等在 Go 侧校验器里表现不同 | P0 冻结时写对照测试；必要时手写校验 |
| **R14** | 范围蔓延 | 想恢复权限层 / 想加 TUI / 想加 provider | **非目标写进 README**；每阶段结束对照 §2.2 |

---

## 9. 附录

### 9.1 TS → Go 概念映射速查

| TS | Go |
|---|---|
| `interface` / type union | struct + `Type` 判别字段（避免 `interface{}` + 断言散落） |
| `async function` | `func(ctx) (T, error)` |
| `Promise.all` | `sync.WaitGroup` |
| `AbortSignal` | `context.Context` |
| `async iterator` | `iter.Seq2[T, error]`（range-over-func） |
| `throw new Error` | `return err` |
| `null` vs `undefined` | `*T`（区分"没有"与"零值"） |
| `typebox` schema | 手写 JSON Schema + `jsonschema/v6` |
| `declare module` 声明合并 | 无对应：Go 用显式接口 + 注入点 |
| `performance.now()` | `time.Since`（单调时钟） |

### 9.2 pi 关键文件索引

| 要理解什么 | 读这个 |
|---|---|
| agent 循环全部逻辑 | `packages/agent/src/agent-loop.ts`（949 行，**整篇读完再动手**） |
| Agent 状态机与队列 | `packages/agent/src/agent.ts`（613）、`types.ts`（539） |
| LLM 消息与事件协议 | `packages/ai/src/types.ts`（重点 408-817 行） |
| 事件流实现 | `packages/ai/src/utils/event-stream.ts`（135） |
| Anthropic 流式适配 | `packages/ai/src/api/anthropic-messages.ts` |
| OpenAI 兼容适配 | `packages/ai/src/api/openai-completions.ts` |
| 工具契约与完整实例 | `coding-agent/src/core/tools/read.ts`（224）+ `index.ts`（224） |
| 精确编辑与 diff | `coding-agent/src/core/tools/edit.ts`（220）+ `edit-diff.ts`（556） |
| 截断策略 | `coding-agent/src/core/tools/truncate.ts`（314） |
| 会话存储 | `coding-agent/src/core/session-manager.ts`（2,013） |
| 系统提示词分段 | `coding-agent/src/core/system-prompt.ts`（229） |
| 配置解析 | `coding-agent/src/core/settings-manager.ts`（1,428） |
| 无头输出 | `coding-agent/src/modes/print-mode.ts`（169）、`json-event.ts`（61） |
| RPC 协议与实现 | `coding-agent/src/modes/rpc/rpc-types.ts`（303）、`rpc-mode.ts`（819） |

### 9.3 Ultrac 现有资产索引（复用参照，**改造前先读**）

| 现有实现 | 为什么先读 |
|---|---|
| `internal/llm/anthropic/{sse,stream,request,jitter}.go` | SSE 解析 + 按 index 分桶攒 tool 参数 + 重试边界，**直接迁移** |
| `internal/engine/tools.go` | 12 并发上限、进度经 channel 汇集、未知工具也产出 result |
| `internal/session/store.go` | 追加写 + `BuildChain` 按 `ParentUUID` 回溯 + `Extra` 原样保留 |
| `internal/tool/builtin/bash.go` | **临时文件收集输出**（不要退化成 `bytes.Buffer`） |
| `internal/tool/builtin/file_read.go` | 行数 + 字节双限截断 + `offset=N` 续读提示 |
| `internal/transport/ndjson.go` | `escapeLineTerminators`（U+2028/29）、block index 递增规则 |
| `internal/config/{config,loader,keys}.go` | 层级合并 + `Extra` 保真 + `Skipped` 显式上报 |
| `cmd/ultrac/{agent,flags,signal}.go` | CLI 装配顺序、信号处理、退出码约定 |

### 9.4 P0 交付物 checklist

- [x] `internal/ai/{types,event,context}.go` + C1/C2 对照测试（另落 `model.go`：`StreamFn`/`AgentState` 契约需要 `Model`）
- [x] `internal/agent/types.go` + C3 对照测试（另落 `toolcall.go`：erased `Tool` + `Erase` + jsonschema/v6 参数校验）
- [ ] `internal/rpc/types.go`（C4，可推到 P6 前）
- [x] 旧 `internal/contract/*` 加 `Deprecated` 注释，指向新包
- [x] `README.md`：写明目标与**非目标**（防蔓延）
- [x] `docs/pi-core-harness-plan.md`（本文件）

### 9.5 验收脚本（P5 之后可跑）

```bash
# 双跑对照：同一 prompt，pi 与 ultrac 输出应一致
PROMPT='只回答一个数字：当前目录有几个 go 文件'

pi          --mode json "$PROMPT" > /tmp/pi.jsonl
./bin/ultrac --mode json "$PROMPT" > /tmp/ultrac.jsonl

jq -r '.type' /tmp/pi.jsonl    > /tmp/pi.types
jq -r '.type' /tmp/ultrac.jsonl > /tmp/ultrac.types
diff /tmp/pi.types /tmp/ultrac.types && echo "事件序列一致"
```

### 9.6 退场清单（P7 必须清空）

```
internal/permission/            906 + 302 测试
internal/shell/                 690 + 112 测试
internal/contract/              841 + 199 测试
internal/engine/ + engine/prompt/  1,450 + 991 测试
internal/tool/builtin/{web_fetch,web_search,todo,agent,ask}.go  ≈ 1,200
cmd/ultrac/agent.go 的权限装配  ≈ 90
go.mod 的 mvdan.cc/sh/v3
```

验收：`grep -rn "internal/permission\|internal/shell\|mvdan.cc/sh" --include=*.go .` 零命中。

---

## 10. 下一步建议

1. **先接受 §0.2 的两个连带后果**（TS 侧失配、5 个工具移除）。这是本计划最需要你确认的地方。
2. **P0–P6 已落地**（契约冻结 → provider 流式 → agent loop + Agent 状态机 → pi 8 工具 → 会话/配置/提示词 → CLI + print/json → RPC 模式），每步都有假 provider/对照测试兜底；
   下一步 **P7 退场清理**：删 `internal/permission`、`internal/shell`、`internal/contract/*`、`internal/engine`、旧 5 工具与 CLI 权限装配（§3.8 的表与前置条件），然后做端到端验收 E1–E7。RPC 的双跑关卡（§3.7）**前提是 pi 侧先 install/build（本机尚无 node_modules/dist），P5/P6 的双跑因此顺延至 P7**——驱动脚本 `bin/rpc-driver.ts` 已按双跑兼容形态落盘。目录归属已定：Ultrac 自有 `~/.ultrac/`（2026-10-16），两边不共享任何路径。
3. **P1 的教训仍然有效**：改造既有代码时**优先迁移、不要重写**——那里有你踩过的坑。
4. **P5 之后立刻做双跑对照**（§9.5）。这是唯一能证明"等价"的手段。
5. **每阶段结束跑一次 `go build ./...`**。改造期最大的风险不是写错，是编译断了太久。
6. **范围蔓延是本计划最大的敌人。** 每完成一个阶段，回来对照一次 §2.2。
