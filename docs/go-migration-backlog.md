# Ultrac TS 功能清单与迁移优先级

> 用途：把 513,602 行 TypeScript 拆成**可逐个独立完成的功能包**，按重要程度分级。
> 依据：本仓实测统计（2026-10-08），非估算。
> 关联：[`go-rewrite-plan.md`](./go-rewrite-plan.md)（总体计划）、[`reasonix-go-migration-analysis.md`](./reasonix-go-migration-analysis.md)（参考项目）、[`spec/stubs-registry.md`](./spec/stubs-registry.md)（桩清单）

---

## 0. 先看这一节

### 0.1 三条分级原则

| 原则 | 含义 |
|---|---|
| **按依赖排，不按体积排** | 体积最大的是 TUI（11.2 万行），但它是最后才该动的。**先做被依赖的，再做依赖别人的。** |
| **每个包要有独立验收** | 一个功能包必须能单独证明"对了"，否则它不是包，是一摊活。验收方式写在每项的 **DoD** 里。 |
| **桩不算功能** | 仓库里有 **35 个桩文件**（实测，判据 `^// TODO: Phase`）。桩是要补的洞，不是要迁移的功能。见 §4。 |

### 0.2 三条容易踩的坑

1. **不要按目录名推断功能边界。** `utils/` 566 个文件横跨所有层，它不是"一个模块"，是五个模块的公共抽屉。
2. **不要从 TUI 开始。** Ink 是 React 协调器语义，bubbletea 是 Elm 架构——这不是翻译，是重新设计 488 个文件。
3. **不要先做 89 个斜杠命令。** 先做**命令框架**（一个），命令本身分批注入。

### 0.3 全局体量（实测）

| 目录 | 文件 | 行数 | 归属等级 |
|---|---:|---:|---|
| `utils/` | 566 | 180,494 | **横切全等级**（见 §5） |
| `components/` | 390 | 81,559 | T3 |
| `services/` | 132 | 53,696 | T0–T3 横切 |
| `tools/` | 190 | 50,980 | T1–T4 横切 |
| `commands/` | 193 | 26,768 | T2 框架 / T3 内容 |
| `ink/` | 98 | 19,846 | T3 |
| `hooks/` | 104 | 19,204 | T2 |
| `bridge/` | 31 | 12,613 | T4 |
| `cli/` | 23 | 12,369 | T1 |
| `screens/` | 3 | 5,977 | T3 |
| `native-ts/` | 4 | 4,081 | T5 |
| `skills/` | 20 | 4,066 | T2 |
| `entrypoints/` | 11 | 4,064 | T1 |
| `types/` | 23 | 3,775 | T5（shim）/ T1（契约） |
| `tasks/` | 12 | 3,286 | T2 |
| `keybindings/` | 14 | 3,159 | T3 |
| `constants/` | 21 | 2,648 | T1 |
| `bootstrap/` | 1 | 1,758 | T1 |
| `memdir/` | 8 | 1,736 | T3 |
| `vim/` | 5 | 1,513 | T3 |
| `buddy/` | 6 | 1,298 | T5 |
| `state/` | 6 | 1,190 | T1 |
| `remote/` | 4 | 1,127 | T4 |
| `context/` | 9 | 1,004 | T1 |
| `migrations/` | 11 | 603 | T0 |
| `query/` | 4 | 652 | T1 |
| `upstreamproxy/` | 2 | 740 | T4 |
| `coordinator/` | 1 | 369 | T3 |
| `server/` | 3 | 358 | T4 |
| 其余 14 个目录 | 30 | ~1,500 | 杂项 |
| **顶层文件** | 18 | **~11,000** | 见下 |
| **合计** | **1929** | **513,602** | |

顶层文件（不入任何目录，但都是主干）：`main.tsx` 4683、`query.ts` 1729、`QueryEngine.ts` 1295、`Tool.ts` 792、`commands.ts` 756、`history.ts` 464、`setup.ts` 477、`tools.ts` 389、`context.ts` 189、`Task.ts` 125 等。

---

## T0 — 地基（4 项，不完成则一切无从下手） （已完成）

| # | 功能 | TS 位置 | 体量 | Go 目标 | 依赖 |
|---|---|---|---|---|---|
| **0.1** | **配置分层解析** | `utils/settings/`（19 文件）、`utils/managedEnv.ts`、`utils/config.ts`、`utils/settings/mdm/` | ~20 文件 | `internal/config` | 无 |
| **0.2** | **会话 transcript 存储** | `utils/sessionStorage.ts`、`history.ts`、`utils/conversationRecovery.ts` | 3 文件 / ~1.2k | `internal/session` | 0.1 |
| **0.3** | **错误模型与日志** | `utils/errors.ts`、`utils/log.ts`、`utils/errorLogSink.ts`、`utils/toolErrors.ts` | 4 文件 | `internal/logging` | 无 |
| **0.4** | **Provider 客户端（含流式 SSE）** | `services/api/`（21 文件）、`services/rateLimitMessages.ts` | 21 文件 / ~5k | `internal/llm/anthropic` | 0.1 |

**0.1 落地说明（2026-10-08 实测）**：「Go 目标」列填的是目录 `internal/config`，允许同目录下按职责拆多个文件。
实际由 `config.go`（层定义与合并，含 `SourceDefault`）、`paths.go`（路径解析）、`keys.go`（env 键白名单 `safeEnvVars`）、
`env.go`、`loader.go` 五个源文件加 `config_test.go` 组成，全部落在 `internal/config/`，与文档一致，无偏离。

### 0.1 DoD
- 读 `~/.ultrac/settings.json` → 分层合并（default → user → project → flag → env）结果与 TS 侧逐项一致
- `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 置位时，provider 相关 env 从**所有**源消失
- 项目级源只应用 `SAFE_ENV_VARS` 白名单（对齐 `utils/managedEnvConstants.ts`）
- 留空字段 = 删除该 key，不是写空串

### 0.4 DoD
- 对 `ANTHROPIC_BASE_URL` 发一次真实请求，流式拿到 text delta 序列
- 流式 tool_use **按 index 累积**，只对外发出完整调用
- `ctx` 取消能中断在途请求（无 goroutine 泄漏）
- 429/5xx 有退避重试（对齐 `services/api/withRetry.ts`）

> **0.4 是风险最高的一块。** 建议先只做「单轮 + 文本」，把 tool_use 累积和重试放到 1.1 之后。

---

## T1 — 最小可用 Agent（7 项，能完成一次真实编码任务） （已完成）

| # | 功能 | TS 位置 | 体量 | Go 目标 | 依赖 |
|---|---|---|---|---|---|
| **1.1** | **query 循环 / turn 状态机** | `query.ts`(1729)、`QueryEngine.ts`(1295)、`query/`(652)、`Task.ts` | 3.7k | `internal/engine` | 0.4 |
| **1.2** | **系统提示词组装** | `utils/systemPrompt.ts`、`utils/claudemd.ts`、`constants/`(2648) | ~3k | `internal/engine/prompt` | 0.1 |
| **1.3** | **核心 12 工具** | 见下 | ~15k（估） | `internal/tool/builtin/*` | 1.1 |
| **1.4** | **权限引擎** | `utils/permissions/`（24 文件） | ~8k（估） | `internal/permission` | 1.1 |
| **1.5** | **CLI 入口 + headless** | `entrypoints/cli.tsx`、`entrypoints/init.ts`、`cli/print.ts` | ~2k | `cmd/ultrac` | 0.1 |
| **1.6** | **NDJSON 事件协议** | `cli/ndjsonSafeStringify.ts`、`entrypoints/sdk/controlSchemas.ts`、`cli/structuredIO.ts` | ~1k | `internal/transport` | 1.1 |
| **1.7** | **最小输出面** | —（新增） | — | `internal/transport/sink` | 1.6 |

### T1 落地说明（2026-10-09 实测）

7 项全部落地。本次新增 **29 个源文件 + 7 个测试文件**；`cmd/` 与 `internal/` 现在合计
**14,434 行非测试 + 5,766 行测试**（含 T0 的既有部分，`find cmd internal -name '*.go'` 可复现），
其中 **95 个测试函数**落在 T1 新增的七个包上。
「Go 目标」列写的包全部对上；**只多出一个文档里没有的包 `internal/shell`**，
它是 `permission` 与 `BashTool` 共享的 shell 命令分类器（L3 基础设施），拆分理由见下方偏离项 #3。

`make check`（vet + test）全绿，`staticcheck ./internal/... ./cmd/...` **零告警**。

| 功能包 | Go 包 | 实测行数 | 测试函数 | 验收证据 |
|---|---|---:|---:|---|
| 1.1 | `internal/engine` | 1,090 | 19 | 单轮/多轮工具闭环、maxTurns、取消、未知工具、工具级失败、并行时序、结果保序 |
| 1.2 | `internal/engine/prompt` | 360 | 13 | 段落齐备、只提存在的工具、CLAUDE.md/.ultrac/rules、override/custom/append 三级优先级 |
| 1.3 | `internal/tool/builtin` | 3,515 | 28 | 12 个工具全注册；每个 input schema 都是合法 JSON Schema；Read/Write/Edit/Glob/Grep/Bash/Todo 逐个边界 |
| 1.4 | `internal/permission` + `internal/shell` | 906 + 690 | 12 + 4 | 4 模式行为、规则解析/匹配/相对路径、路径校验、危险模式、AST 分类 |
| 1.5 | `cmd/ultrac` | 1,243 | **0** | 只有实机验收。见下方「已知缺口」 |
| 1.6 | `internal/transport` | 357 | 10 | 每种事件一行合法 JSON、U+2028/29 转义、block index、并发不串行 |
| 1.7 | `internal/transport/sink` | 331 | 9 | text/json 两种输出面，stdout 与 stderr 严格分离 |

合计 **95 个测试函数**。

> **已知缺口**：`cmd/ultrac` 的 flag 解析（`flags.go`）**没有单元测试**，只靠实机调用覆盖。
> 它是唯一一处"用户输入 → 行为"的解析代码，错误路径（缺值、未知 flag、`--setting-sources` 之类的取值校验）
> 目前只有人工跑过。补一个 `flags_test.go` 是下次动 CLI 时该做的第一件事。

**实机验收（真实 endpoint，~/.ultrac/settings.json 的 deepseek 配置）**

```
❯ ultrac -p "In .tmp/t1/greet.go, change the greeting from hi to hello. Then verify by reading
            the file back and say VERIFIED." --permission-mode acceptEdits
  → Read  file_path=...\greet.go
  ✓ Read       1	package main ...
  → Edit  file_path=...\greet.go
  ✓ Edit  Edited ...greet.go: replaced 1 occurrence(s)
          @@ -1,5 +1,5 @@
          -	return "hi"
          +	return "hello"
  → Read  file_path=...\greet.go
  ✓ Read       4		return "hello"
Verified. The file now returns "hello".VERIFIED
```

一次「读 → 改 → 读回验证 → 报告」的完整闭环，多轮工具调用、diff 渲染、最终答案全部正确。

**4 模式行为一致性（同一句写文件指令，逐模式实测）**

| 模式 | 结果 | 原因 |
|---|---|---|
| `default` | 文件未创建 | 无提问前端，ask → deny，并给出可执行的补救说明 |
| `acceptEdits` | **文件已创建** | 文件编辑自动批准 |
| `plan` | 文件未创建 | 计划模式只读，直接 deny |
| `bypassPermissions` | **文件已创建** | 除 deny 规则外全部批准 |

另外实测：`--add-dir` 确实扩大了作用域（同一路径，不加参数被拒、加了参数写入成功）；
`Read(go.mod)` 这类**相对路径规则**能从 settings.json 生效；模型被拒后没有绕道 Bash 重试（Bash 侧也被正确判为 write）。

### T1 的 8 处有意偏离（都不是"忘了做"）

| # | 偏离 | 为什么 | 影响面 |
|---|---|---|---|
| 1 | **Grep/Glob 不走 ripgrep**，用纯 Go 遍历 | 本机无 `rg`，TS 侧也没把它 vendor 进 `node_modules`。计划书说"优先保留 rg 子进程"，前提是有 rg | **`.gitignore` 不再被遵守**。改为按名字跳过 VCS 目录（`.git`/`.svn`/`.hg`…）与依赖目录（`node_modules`/`vendor`/`__pycache__`/`target`/`dist`…）。行为差异已知，T2 要做真 gitignore 语义得单独排一项 |
| 2 | **Bash 判定纯 AST，无模型分类器** | TS 的 `bashClassifier`/`yoloClassifier` 要模型调用，权限层拿不到。改按计划书 §1.4 DoD 的"判定读结构不读措辞"，用 `mvdan.cc/sh` 解析真实 AST | 无法识别"语义危险但语法平庸"的命令（例如 `python -c '...'` 只会被归为 interpreter → ask，而不会被判为 destructive）。**保守方向是安全方向**：判不出来一律 ask |
| 3 | **`internal/shell` 被拆成独立包** | `permission`（判"能不能跑"）和 `BashTool`（判"能不能并发/是否只读"）需要同一个答案，而 L2 之间禁止横向引用。放 L3 基础设施层两边都能用 | 无。反而消除了一个潜在循环依赖 |
| 4 | **thinking 块不回灌** | 回放 thinking 需要它的 signature，而 provider 累积了却没对外暴露。发一个没有 signature 的 thinking 块会让 API 拒绝整个请求 | 开了 thinking 时多轮会丢推理上下文，已发 `notice` 明说（`thinking_not_replayed`）。signature 打通是 T2 的事 |
| 5 | **`AskUserQuestion` 在没有前端时拒绝** | T1 没有交互前端。留了 `SetAskHandler` 注入点，GUI/SDK 接上就能用 | headless 下工具返回明确说明，让模型把问题写进回复而不是空等 |
| 6 | **`Agent` / `TaskOutput` 靠注入实现** | 跑子 agent 等于再跑一遍 query 循环，只有 engine 能做；`builtin` import `engine` 就是真循环依赖 | `SetAgentRunner` / `SetTaskOutputProvider` 两个注入点已就绪且已测，但 **engine 目前没有安装 runner**（子 agent 入口归 T2.5）。理由是宁可不做也不做半截 |
| 7 | **`WebSearch` 抓 DuckDuckGo HTML** | 没有托管搜索 API。抓解析会随对方改版失效 | 已在工具返回里明说"这不等同于搜到 0 条结果"，失败不会伪装成成功 |
| 8 | **headless 下按已信任处理** | 无信任弹窗可弹。这是**主动放宽**：项目级 settings.json 的 env 会全量应用 | 与 TS 非交互会话一致，也是项目级 provider 配置唯一能生效的路径。已在 `cmd/ultrac/agent.go` 注释里写明 |

**另外三处超出 8 个 flag 的最小追加**（不加则 agent 在某些常规场景下不可用）：
`--dangerously-skip-permissions`（等价 `--permission-mode bypassPermissions`）、`--max-turns`、
`--append-system-prompt` / `--system-prompt`。

**新增依赖**：`mvdan.cc/sh/v3`（纯 Go，免 CGO）。这是 T1 唯一一个新增的第三方依赖，
用途就是偏离项 #2——计划书 §5.2.4 把它列为"必须用 mvdan/sh"。

**T1 没做、留给后续的**：
- transcript 只记「用户提问 + 最终回答 + last-prompt」三条，**不可 replay**（完整回放归 T2.6）
- 后台命令只返回 PID，不捕获输出（任务注册表归 T2.5）
- 图片/PDF 不渲染，返回 base64 + 说明（多模态不属 T1）
- 没有沙箱：`--add-dir` 只管权限作用域，`dangerouslyDisableSandbox` 目前是空操作（归 T2.8）

### 1.3 核心 12 工具（分批建议）

| 批次 | 工具 | 理由 |
|---|---|---|
| **1.3a（3 个，最先）** | `FileReadTool`、`GlobTool`、`GrepTool` | **只读、无副作用、易测**。先证明工具契约在真实工具上成立 |
| **1.3b（4 个）** | `FileWriteTool`、`FileEditTool`、`BashTool`、`TodoWriteTool` | 有副作用，需配合 1.4 权限 |
| **1.3c（3 个）** | `WebFetchTool`、`WebSearchTool`、`AskUserQuestionTool` | 出网 + 交互 |
| **1.3d（2 个）** | `AgentTool`、`TaskOutputTool` | 子 agent 入口，牵出 T2 的 2.5 |

`BashTool` 单独警告（对应 `tools/BashTool/` 下有 `bashPermissions.ts`、`bashParser.ts`、`readOnlyValidation.ts` 等多个文件）：**Go 侧必须用 `mvdan.cc/sh` 解析成 AST 再判定，不要做字符串匹配。**

### 1.4 DoD
- 4 种模式（default / acceptEdits / plan / bypassPermissions）行为一致
- 规则解析、路径校验、危险模式匹配全部有测试
- **判定读结构不读措辞**：`bashClassifier` 类逻辑用 AST，不用正则匹配命令名

### 1.5 CLI flag 范围（实测 54 个 `--xxx`）
先只实现这 8 个，其余延后：
`--print`、`--output-format`、`--model`、`--permission-mode`、`--add-dir`、`--settings`、`--version`、`--help`

> 完整清单见 §6 附录。**不要一次全做。**

---

## T2 — 生产可用（8 项，日常真用会立刻感到缺）

| # | 功能 | TS 位置 | 体量 | Go 目标 | 依赖 |
|---|---|---|---|---|---|
| **2.1** | **上下文治理** | `services/compact/`、`services/contextCollapse/`、`utils/tokenBudget.ts`、`utils/tokens.ts`、`utils/analyzeContext.ts` | ~3k | `internal/engine/context` | 1.1 |
| **2.2** | **MCP 客户端 + 工具桥接** | `services/mcp/`、`utils/mcp/*` | ~8k | `internal/mcp` | 1.3 |
| **2.3** | **Hooks（9 事件）** | `utils/hooks/`（17 文件，实测）、`hooks/`(19,204) | ~20k | `internal/hook` | 1.4 |
| **2.4** | **Skills** | `skills/`(4,066)、`utils/skills/` | ~4.5k | `internal/skill` | 1.2 |
| **2.5** | **子 agent / Task 族** | `tasks/`(3,286)、7 个 `Task*Tool`、`utils/forkedAgent.ts` | ~4k | `internal/agent` | 1.1 |
| **2.6** | **会话恢复 / rewind / checkpoint** | `utils/fileHistory.ts`、`utils/sessionRestore.ts`、`commands/rewind` | ~2k | `internal/session/checkpoint` | 0.2 |
| **2.7** | **斜杠命令框架** | `commands.ts`(756)、`utils/slashCommandParsing.ts` | ~1k 框架 | `internal/command` | 1.5 |
| **2.8** | **沙箱** | `utils/sandbox/`、`commands/sandbox-toggle` | ~1k | `internal/sandbox` | 1.4 |

### 2.3 Hooks 的 9 个事件（实测）
`PreToolUse` · `PostToolUse` · `Notification` · `Stop` · `SubagentStop` · `UserPromptSubmit` · `PreCompact` · `SessionStart` · `SessionEnd`

### 2.7 命令框架 vs 命令内容
**框架**（1 个功能包）：注册表 + 参数解析 + 分类（内置 / 插件 / MCP 提供）。
**内容**（89 个命令目录 / 193 个文件）：按使用频率分批，建议顺序：

| 批次 | 命令 | 理由 |
|---|---|---|
| 2.7a | `/clear` `/compact` `/context` `/cost` `/help` `/exit` `/config` `/model` | 最常用，且不依赖新子系统 |
| 2.7b | `/permissions` `/hooks` `/skills` `/mcp` `/agents` `/tasks` | 配置类，依赖 T2 各子系统 |
| 2.7c | `/init` `/memory` `/vim` `/theme` `/output-style` `/status` | 个性化 |
| 2.7d | `/doctor` `/debug-tool-call` `/heapdump` `/stats` `/usage` `/release-notes` | 诊断 |
| 2.7e | 其余（含 `chrome`、`desktop`、`mobile`、`teleport`、`voice` 等） | 依赖 T4 |

---

## T3 — 完整体验（8 项）

| # | 功能 | TS 位置 | 体量 | 依赖 |
|---|---|---|---|---|
| **3.1** | **多 provider（Bedrock / Vertex / Foundry）** | `utils/model/`（17 文件）、`services/api/client.ts` | ~3k | 0.4 |
| **3.2** | **计划模式** | `utils/planModeV2.ts`、`tools/Enter*PlanMode`、`commands/plan` | ~1.5k | 1.1 |
| **3.3** | **记忆体系** | `memdir/`(1,736)、`utils/memory/`、`services/SessionMemory`、`services/extractMemories` | ~4k | 2.6 |
| **3.4** | **LSP 集成** | `services/lsp/`、`tools/LSPTool` | ~2k | 1.3 |
| **3.5** | **遥测（OTel）** | `utils/telemetry/`（9 文件，实测） | ~3k | 1.1 |
| **3.6** | **成本 / 用量统计** | `cost-tracker.ts`、`costHook.ts`、`utils/modelCost.ts`、`utils/stats.ts` | ~1k | 0.4 |
| **3.7** | **插件体系** | `utils/plugins/`（~44 文件，实测）、`services/plugins/`、`plugins/` | ~10k | 2.2 2.7 |
| **3.8** | **TUI（Ink 全量）** | `components/`(81,559)、`ink/`(19,846)、`screens/`(5,977)、`keybindings/`(3,159)、`vim/`(1,513)、`buddy/`(1,298) | **~113k** | 1.6 |

### 3.8 的关键判断

`components/` 390 文件 + `ink/` 98 文件 + `screens/` + `keybindings/` + `vim/` + `buddy/` ≈ **113k 行，占全仓 22%**。

**建议：这一项永远放在最后，且先做「够用」而不是「等价」。** 最小集是：流式输出区、输入框、工具调用展示、权限对话框。vim 键位、主题、多行编辑、buddy 精灵都可以不做。

---

## T4 — 平台特化（6 项，按需）

| # | 功能 | TS 位置 | 体量 |
|---|---|---|---|
| **4.1** | Bridge / 远程控制 | `bridge/`(12,613)、`remote/`、`upstreamproxy/` | ~14k |
| **4.2** | Daemon / server / 常驻 | `server/`、`cli/remoteIO.ts`、`daemon/` | ~1k（`daemon/` 大部分是桩） |
| **4.3** | IDE 集成 / ACP | `entrypoints/sdk/`、`utils/ide.ts`、`services/lsp/` | ~2k |
| **4.4** | Chrome 扩展 | `utils/claudeInChrome/`（7 文件，实测） | ~2k |
| **4.5** | Computer-use | `utils/computerUse/`（15 文件，实测） | ~3k |
| **4.6** | Voice | `services/voice*.ts`（3 文件）、`voice/` | ~1k |

---

## T5 — 建议不做（3 类）

| 类别 | 具体 | 为什么不做 |
|---|---|---|
| **Anthropic 内部遥测上报** | `services/analytics/`、`services/internalLogging.ts`、`utils/telemetry/bigqueryExporter.ts`、`utils/telemetry/perfettoTracing.ts`、`utils/undercover.ts` | 上报到 Anthropic 内部后端，对自建无意义 |
| **内部实验 / A-B / 风控** | `services/rateLimitMocking.ts`、`services/mockRateLimits.ts`、`commands/ant-trace`、`commands/good-claude`、`commands/bughunter`、`utils/classifierApprovals*` | 内部工具与实验开关 |
| **泄露源码专用兼容层** | `types/shims/*`（8 个）、`native-ts/`（4,081） | shim 存在的唯一理由是原包拿不到；Go 侧可以直接实现真货 |

---

## 4. 桩清单：35 个（不是功能，是洞）

**判据（唯一）：文件首行带 `// TODO: Phase N`。** 实测 35 个，与 [`spec/stubs-registry.md`](./spec/stubs-registry.md) 一致。

> ⚠️ 该注册表记录了一条重要教训：**2026-09-28 之前的版本列了 69 条"待还原清单"，而那些文件实际上全都是完整真实现。照那份清单去补等于白干。** 判据只有一个——首行有没有那个标记。

### 4.1 A 类 shim（8 个）—— 替代拿不到的外部包

| 文件 | 替代 | Go 侧处置 |
|---|---|---|
| `types/shims/claude-for-chrome-mcp.ts` | `@ant/claude-for-chrome-mcp` | T4.4 时真实现 |
| `types/shims/computer-use-mcp.ts` | `@ant/computer-use-mcp` | T4.5 时真实现 |
| `types/shims/computer-use-mcp-types.ts` | 同上（类型） | 同上 |
| `types/shims/computer-use-mcp-sentinelApps.ts` | 同上（应用清单） | 同上 |
| `types/shims/computer-use-swift.ts` | `@ant/computer-use-swift`（macOS 原生） | 不在 Windows 复现 |
| `types/shims/computer-use-input.ts` | `@ant/computer-use-input` | 同上 |
| `types/shims/color-diff-napi.ts` | `color-diff-napi`（npm 上是抢注空壳） | **纯 Go 实现，天然消除** |
| `types/shims/compiler-runtime.ts` | ~~`react/compiler-runtime`~~ | **已废弃**，不再被引用 |

### 4.2 B 类 本地缺失模块桩（27 个）

这一类的处置**必须在动手前先决定**：是补真实实现，还是判定不做。

| 文件 | 归类 | 建议 |
|---|---|---|
| `services/compact/snipCompact.ts` | T2.1 上下文治理 | **要补**——压缩链路的一环 |
| `services/compact/cachedMicrocompact.ts` | T2.1 | **要补** |
| `services/contextCollapse/index.js` | T2.1 | **要补** |
| `entrypoints/sdk/coreTypes.generated.ts` | **T1.6 控制协议契约** | **优先补**——它是对外契约的一部分 |
| `entrypoints/sdk/runtimeTypes.ts` | T1.6 | **优先补** |
| `entrypoints/sdk/toolTypes.ts` | T1.6 | **优先补** |
| `tools/REPLTool/REPLTool.ts` | T1.3 扩展 | 延后 |
| `tools/WorkflowTool/constants.ts` | T3 | 延后 |
| `tools/VerifyPlanExecutionTool/VerifyPlanExecutionTool.ts` | T3.2 计划模式 | 延后 |
| `tools/SuggestBackgroundPRTool/SuggestBackgroundPRTool.ts` | T4 | 延后 |
| `tools/TungstenTool/TungstenTool.ts` + `TungstenLiveMonitor.ts` | 内部 | **不做** |
| `daemon/main.ts` + `daemon/workerRegistry.ts` | T4.2 | 延后 |
| `environment-runner/main.ts` | T4.2 | 延后 |
| `self-hosted-runner/main.ts` | T4.2 | 延后 |
| `assistant/AssistantSessionChooser.tsx` | UI | 随 T3.8 |
| `commands/assistant/assistant.ts` | 命令 | 随 T2.7e |
| `commands/agents-platform/index.ts` | 命令 | 随 T2.7e |
| `components/agents/SnapshotUpdateDialog.tsx` | UI | 随 T3.8 |
| `cli/bg.ts` | T1.5 | 延后 |
| `cli/handlers/templateJobs.ts` | T1.5 | 延后 |
| `ink/devtools.ts` + `ink/global.d.ts` | TUI | 随 T3.8 |
| `types/connectorText.ts` | 杂项 | 延后 |
| `utils/filePersistence/types.ts` | 落盘 | 随 T0.2 |
| `utils/protectedNamespace.ts` | 杂项 | 延后 |

**注意**：`entrypoints/sdk/` 的三个类型文件是桩，意味着**计划书里冻结的控制协议契约（C4）本身有一部分是空的**。做 T1.6 前必须先补出这三个文件的真实形状，否则"冻结契约"冻的是空气。

### 4.3 桩的可达性

注册表记录了 `bun build --metafile` 的实测：35 个桩里 **17 个进 bundle（会被真实加载）**、**18 个被 DCE（当前永不执行）**。这个分类依赖 `bunfig.toml` 的 feature 旗标——改旗标后需要重新对账。

---

## 5. `utils/` 为什么不能当成一个模块

`utils/` 有 566 个文件、180,494 行，占全仓 35%，**横跨所有等级**。按归属拆开看：

| utils 子模块 | 文件数 | 归属 |
|---|---:|---|
| `utils/settings/` | 19 | T0.1 |
| `utils/permissions/` | 24 | T1.4 |
| `utils/hooks/` | 17 | T2.3 |
| `utils/plugins/` | ~44 | T3.7 |
| `utils/telemetry/` | 9 | T3.5 |
| `utils/computerUse/` | 15 | T4.5 |
| `utils/claudeInChrome/` | 7 | T4.4 |
| `utils/sandbox/` | 2 | T2.8 |
| `utils/mcp/` | 6 | T2.2 |
| `utils/model/` | 17 | T3.1 |
| `utils/shell/` | 11 | T1.3b |
| `utils/bash/` | 20+ | T1.3b T1.4 |
| `utils/swarm/` | 20+ | T2.5 |
| `utils/task/` | 6 | T2.5 |
| `utils/teleport/` | 6 | T4.3 |
| 其余约 350 个单文件工具 | 350 | **按调用方归属，不单独迁移** |

**结论**：`utils/` 的迁移不是一项任务，而是**跟着每个功能包顺带做的事**。判断标准是"谁在调用它"——被 T1.3 调用的就随 T1.3 做。

---

## 6. 推荐的首批 5 个功能包

选这几个的理由：**每一个都能独立完成、独立验证、且都在关键路径上。**

| 顺序 | 功能包 | 验收方式 | 预计新增 |
|---|---|---|---|
| **①** | **T0.1 配置层补全** | 给定一份 settings.json → 解析结果逐项比对；含 6 条平台陷阱用例 | ~400 行 + 测试 |
| **②** | **T0.4a Anthropic 流式客户端（单轮文本）** | 打真实 endpoint，`--print "hi"` 能流式吐出文本 | ~500 行 + 测试 |
| **③** | **T1.3a 三个只读工具**（Read / Glob / Grep） | 对真实仓库跑，输出与 TS 侧一致 | ~600 行 + 测试 |
| **④** | **T1.6 NDJSON 事件协议** | 事件序列能被 TS 侧的解析器读通 | ~300 行 + 测试 |
| **⑤** | **T1.1 query 循环（单轮 → 多轮工具）** | 完成一次"读文件 → 改文件 → 报告"的闭环 | ~800 行 + 测试 |

做完 ①–⑤，你就有**一个能真的干活的最小 Go agent**。此后每加一项都是在它上面长。

### 已经在手的部分

`internal/contract/` 提供两端都要用的契约：`Tool[In]`、`Provider` 注册表、`Event` 流、`config` 分层骨架、`version`。
①③⑤ 直接建在它们上面，没有改契约。

**这份清单里的 ①–⑤ 已全部做完，7 项 T1 也全部落地**（见上方「T1 落地说明」）。
现在加一项新能力，落点是明确的：

| 想加什么 | 落点 |
|---|---|
| 一个新工具 | `internal/tool/builtin/` 下加一个文件，`init()` 里 `RegisterBuiltin` |
| 一个新模型厂商 | `internal/llm/<vendor>/` 包 + `provider.Register`，engine 一行不改 |
| 一个新的输出/交互面 | 实现 `event.Sink`，engine 不知道它存在 |
| 一条新的权限策略 | `internal/permission` 的规则表或 `Check`，工具侧无感 |

### 建议的分支纪律

**本机 git 不能建带斜杠的分支名**（`refs/heads/feature/x` 会静默失败，2026-10-08 实测）。所以：

- **不要** `feat/tools-read` 这种名字
- 在 `go-rewrite` 这一个分支上，**一个功能包一个 commit**（提交信息用 `feat(config): ...` 这类前缀即可）
- 需要并行时才建扁平名分支：`tools-read`、`llm-anthropic`

---

## 7. 附录：CLI flag 全量（实测 54 个）

供 1.5 决定范围时参考。**T1 只做加粗的 8 个。**

`--add-dir` · `--agent` · `--agent-teams` · `--agents` · `--allow-dangerously-skip-permissions` · `--allowedTools` / `--allowed-tools` · `--auth-token` · `--available` · `--bare` · `--betas` · `--chrome` · `--claudeai` · `--clear-owner` · `--client-secret` · `--console` · `--dangerously-skip-permissions` · `--debug-file` · `--disable-slash-commands` · `--disallowedTools` / `--disallowed-tools` · `--dry-run` · `--email` · `--fallback-model` · `--file` · `--force` · `--fork-session` · `--from-pr` · `--host` · `--ide` · `--idle-timeout` · `--include-hook-events` · `--include-partial-messages` · `--json` · `--keep-data` · `--local` · `--max-sessions` · `--mcp-config` · `--mcp-debug` · **`--model`** · `--no-chrome` · `--no-session-persistence` · `--output` · **`--output-format`** · `--owner` · `--pending` · **`--permission-mode`** · `--plugin-dir` · `--port` · `--replay-user-messages` · `--safe` · `--scope` · `--session-id` · `--setting-sources` · **`--settings`** · `--sparse` · `--sso` · `--strict-mcp-config` · `--subject` · `--text` · `--tmux` · `--update` · `--upgrade` · `--version` · `--worktree`

POSIX 段中还有：`--background` / `--bg`、`--chrome-native-host`、`--claude-in-chrome-mcp`、`--computer-use-mcp`、`--daemon-worker`、`--dump-system-prompt`（调试用）。

---

## 8. 一句话总结

> **先做被依赖的（T0），再做能闭环的（T1），然后补齐日常（T2），TUI 永远最后。**
>
> 513k 行里真正阻塞你的只有 **4 项 T0 + 7 项 T1**。剩下的都是"什么时候想要什么时候做"。

**进度**：T0 已完成（4/4），**T1 已完成（7/7）**，最小可用 Go agent 已经在跑——

```bash
ultrac -p "把 .tmp/t1/greet.go 里的 hi 改成 hello，改完读回验证" --permission-mode acceptEdits
ultrac -p "总结 internal/engine" --output-format stream-json      # NDJSON，可被程序解析
ultrac -p "..." --permission-mode plan                            # 只读排查
```

下一站是 T2，其中 **2.1 上下文治理**是第一个必须做的：现在没有任何压缩，
一旦对话长了就会撞上下文窗口，`max_turns` 会以"停得莫名其妙"的形式暴露出来。
