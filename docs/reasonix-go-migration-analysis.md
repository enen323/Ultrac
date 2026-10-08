# DeepSeek-Reasonix 改造剖析

> 分析对象：`D:\develop\Project\DeepSeek-Reasonix`（分支 `studio`，Reasonix 2.x）
> 分析日期：2026-10-07
> 方法：实测代码 + git 历史 + 设计文档交叉验证
> 关联：本报告与 [`go-rewrite-plan.md`](./go-rewrite-plan.md)（Ultrac 的 Go 重写计划）配套阅读

---

## 0. 摘要

### 0.1 三条必须先纠正的前提

在展开分析之前，有三点与"它是由 Claude Code 改造而来、并从 TypeScript 迁移到 Go"这个描述不符，且都有硬证据：

| # | 常见描述 | 实测事实 | 证据 |
|---|---|---|---|
| 1 | 由 Claude Code 改造而来 | **不是 fork，是独立的 Go 实现**，但**刻意保持 Claude Code 兼容** | `REASONIX.md:5` 自述"Reasonix analog of Claude Code's CLAUDE.md"；`SPEC.md:185` 工具命名 `mcp__<server>__<tool>` 明写"to match Claude Code"；`CHECKPOINTS.md` 多处 "Claude Code parity" |
| 2 | 从 TypeScript 迁移到 Go | **本仓库历史里看不到这个过程**。`studio` 与 `main-v2` 两支同根，首个提交就是 Go | `git log --reverse` 首提交 = `32a4c02e5 chore: initialize v2 — ground-up rewrite`，次提交 = `feat: import Go implementation as v2 kernel (renamed duo → reasonix)`。历史是被重置过的，Go 之前的前身叫 **duo** |
| 3 | 是"TS → Go"的单向迁移 | 仓库里真实发生过的**有据可查的迁移方向相反**：桌面外壳从 **Wails（Go 桌面框架）迁到 Electron + Next.js（TS）** | `docs/STUDIO_SHELL_BOUNDARIES.md:14-16`："the migration off the Wails shell, which is done: `desktop/next`, the vendored WebView2 fork, the frontend's Wails surface and the Wails-only kernel entries are gone" |

**这不削弱它的参考价值——反而更强。** 因为它证明了一件对 Ultrac 至关重要的事：**在 Go 内核之上保留一个 TS 外壳，是经过验证的成熟架构**，而不是"没迁干净"的中间状态。

### 0.2 语言分布实测（`studio` 分支，排除 `node_modules`）

| 语言 | 文件数 | 主要位置 |
|---|---:|---|
| Go | **4,471** | `internal/`、`cmd/`、`desktop/cmd/`、`sdk/go/` |
| TS | 407 | `desktop/frontend-next/`（760 中的一部分） |
| TSX | 426 | 同上 |
| Python | 586 | `benchmarks/`（572 个，评测脚手架） |
| MD | 294 | 文档 |
| TOML | 153 | 配置与测试夹具 |

**TS/TSX 合计 833 个文件，其中 760 个（91%）在 `desktop/frontend-next/`** —— 也就是 Electron 渲染层。剩下的散在 `workers/`（crash-report / accounts / forum 三个边缘服务）。`internal/` 下只有一个 `.ts` 文件，还是 LSP 的测试样本。

> 一句话：**Go 是内核，TS 只是外壳。边界干净到可以用一句话概括。**

### 0.3 五条对 Ultrac 最有价值的结论

1. **架构门禁做成了可执行代码**：`tools/repolint/` 是自研的 ~40 个检查器，配 `baseline.json` ratchet 基线。其中 `layering`、`layer-order`、`wire-parity`、`refusal-path`、`window-state` 等 **12 条规则是零容忍（预算 = 0）**。
2. **测试比源码多**：`internal/` 下 1,985 个源码文件 vs **2,147 个测试文件**。对照 Ultrac 的 **0 个测试文件**——这就是"能不能重写"的分水岭。
3. **一个 `control.Controller` 支撑全部前端**（TUI / HTTP+SSE / Electron）。加功能加在 controller，不加在前端。
4. **依赖极度克制但并非只用标准库**：43 个直接依赖。SPEC 里"TOML 是唯一接受的依赖"这条原则**已经漂移**（现实中有 sqlite / tree-sitter / starlark / bubbletea 等）。
5. **关键接口只有两个**：`Provider` 和 `Tool`。其余一切（模型、工具、扩展）都是注册表 + 配置解析的产物。

---

## 1. 项目定位与规模

### 1.1 它是什么

一个 **MIT 协议的开源编码 Agent**，单一 Go 二进制，四条接入路径：

| 接入方式 | 实现 |
|---|---|
| 终端 TUI | `internal/frontend/tui` |
| 桌面 App（Reasonix Studio） | `desktop/`（Electron 主进程 + Next.js 渲染层） |
| 浏览器 | `internal/frontend/serve`（HTTP + SSE） |
| 编辑器 | `internal/frontend/acp`（Agent Client Protocol，stdio JSON-RPC） |

### 1.2 规模

| 指标 | 数值 |
|---|---|
| Go 文件 | 4,471 |
| Go 代码行 | 798,867 |
| `internal/` 源码文件（非测试） | 1,985 |
| `internal/` 测试文件 | **2,147** |
| Go 直接依赖 | 43 |
| Go 间接依赖 | 23 |
| 内置工具 | 29 个 |
| git 提交数 | 8,523（2026-05-29 → 2026-10-07，约 19 周） |

> 798,867 行 Go / 19 周 / 20 位贡献者。**这个规模不是靠一个人一次性翻译出来的，是靠门禁和分层维持住的。**

---

## 2. 目录结构组织

### 2.1 顶层层：12 个，每个一层职责

真实结构（`SPEC.md §2` 里写的还是旧版布局 `internal/cli`、`internal/provider`…，**文档已落后于代码**，以下为实测）：

| 层 | 包数 | 职责 | 性质 |
|---|---:|---|---|
| `internal/contract/` | 16 | **接口与共享类型**（provider / tool / event / config / permrule / planmode / pricing / surface / observe…） | 被所有人依赖 |
| `internal/base/` | 24 | **叶子工具**（diff / filelock / fileref / fileutil / frontmatter / gitignore / hostwrite / i18n / netclient / neterr / proc / retrieval / scratch / secrets / **shellparse** / sysproxy / tempdir / testenv / textutil / tokencount / workspaceid…） | 不许反向依赖 |
| `internal/model/` | 10 | **厂商适配**（anthropic / openai / responses / laya / catalog / billing / boundedllm / providerbroker / visionimage） | 实现 `contract/provider` |
| `internal/tools/` | 7 | **工具实现**（builtin / advisor / jobs / productdocs / script / sessiontool / shellrun） | 实现 `contract/tool` |
| `internal/ext/` | 16 | **扩展点**（plugin / hook / skill / command / extension / market / mcpregistry / mcpsetup / mcplaunch / pluginpkg / pluginspec / theme / configbackup…） | 运行时装配 |
| `internal/safety/` | 7 | **安全**（permission / sandbox / shellsafe / typesafe / egress / redirectguard / evidence） | 判定与约束 |
| `internal/state/` | 23 | **持久化**（checkpoint / memory / history / migration / sessionstore / sessionv4 / sessioninbox / projectiondb / store / trajectory / stats / usagecatalog / workgroup / workspacelease…） | 唯一写状态的地方 |
| `internal/session/` | 1 | **会话控制**（`control.Controller` —— 所有前端的公共入口） | 核心枢纽 |
| `internal/runtime/` | 21 | **编排**（agent / coordinator / delegation / guardian / plancontract / taskcontract / taskmonitor / taskpolicy / recovery / writeclaim / verdict / isolation…） | 跑循环 |
| `internal/platform/` | 25 | **OS 集成**（gitcmd / gitcommit / gitstatus / worktree / lsp / editor / notify / telemetry / update / appupdate / browser / computer / clipimage / crashreport / instanceid / remote / remotecloud / repair…） | 与操作系统打交道 |
| `internal/frontend/` | 10 | **用户界面**（tui / cli / serve / acp / doctor / capdiag / termrender / traystate / remotehost） | **只有它能引 control** |
| `internal/assembly/` | 1 | **组装根**（`boot`，把一切接起来） | 组合根 |

**命名法值得直接抄**：`contract` = 接口，`base` = 叶子，`ext` = 扩展点，`model` = 厂商适配，`runtime` = 编排，`state` = 持久化，`safety` = 安全，`platform` = 系统集成，`frontend` = 用户界面，`assembly` = 组合根。

### 2.2 依赖方向：单向、无环、可强制执行

```
frontend/*  ·  assembly/boot      ← 只有这一层能引 control
        │
        ▼
   session/control  (Controller)
        │
        ▼
   runtime/*  ──►  contract/*  ◄──  model/*   tools/*
        │                              ▲
        ▼                              │
     state/*   ext/*   safety/*   platform/*
        │
        ▼
      base/*   (叶子层：除同层外不引任何 reasonix/ 包)
```

规则写死在 `tools/repolint/layers.go` 里：

- **`frontends`（5 个）**：`internal/frontend/{acp,cli,serve,tui}` + `internal/assembly/boot`。只有这些能 import `control`；**下方的任何包都不许反向引它们**。
- **`leaves`（40 个）**：叶子层"是封闭的"——可以互相引用，但拖不进任何依赖图。原文注释解释了为什么不禁止同层互引："refusing it is what leaves the same judgement copied into several of them"（禁止了反而会把同一个判断复制到好几个包里）。
- **`hostAdapters`（1 个）**：`internal/frontend/remotehost`。它"紧挨着前端而不是在前端下面"——按设计消费前端契约，内核不许碰它，只有宿主才能装配。

### 2.3 嵌套 Go 模块的策略

| 模块 | 位置 | 为什么独立 |
|---|---|---|
| 主模块 | `./go.mod`（`module reasonix`） | 内核 + CLI + SDK 之外的构建工具 |
| 桌面模块 | `desktop/go.mod` | Electron 侧的平台代码（`desktop/cmd/*`、`desktop/internal/winuninstall`） |
| SDK 模块 | `sdk/go/go.mod` | 供第三方 import 的稳定 API |

Makefile 明确记录了踩过的坑：

```make
# "Every module, because a nested one is where a red test hides longest:
#  the shell's route allowlist sat broken through a release, and
#  `go test ./...` from the root cannot see it."
test: studio-test sdk-test
	go test ./...
studio-test:
	cd desktop && go test ./...
sdk-test:
	cd sdk/go && go test ./...
```

> **带给 Ultrac 的教训**：一旦拆多个 module，根目录的 `go test ./...` 会静默漏掉嵌套模块。必须有独立目标。

### 2.4 分发形态：Go 二进制 + TS 薄壳

`npm/reasonix/package.json` 只打包一个 `bin/reasonix.js`：

```json
{ "bin": { "reasonix": "bin/reasonix.js" }, "files": ["bin/reasonix.js"] }
```

即：**npm 包是下载预编译二进制的引导脚本**，不是 JS 实现。三档分发：

1. `npm i -g reasonix` → 薄 JS 壳下载对应平台二进制
2. `goreleaser` → 6 个交叉编译产物（darwin/linux/windows × amd64/arm64），带 `.minisig` 签名 + `SHA256SUMS`
3. `brew` → macOS

---

## 3. 代码迁移策略

### 3.1 文档即契约：SPEC 先行，代码跟随

`docs/SPEC.md` 开篇第一句就是方法论的声明：

> Reasonix is a coding agent: a thin harness driving multiple models, with **all capabilities supplied by configuration and plugins**. This document is the contract — **code follows it**. Change the contract first, then the code.

配套的六条设计原则（`SPEC.md §1`）：

| # | 原则 | 对迁移的含义 |
|---|---|---|
| 1 | **Config- and plugin-driven core** —— 内核只认接口，具体模型与工具按名字从注册表解析。**No hardcoded `switch model`** | 迁移时不必逐个厂商写分支，写一个工厂即可 |
| 2 | **Single static binary** —— `CGO_ENABLED=0`，一条命令交叉编译 | 选型先过"能不能纯 Go 静态编译"这一关 |
| 3 | **Lean dependencies** —— 默认标准库；第三方必须纯 Go、轻量、不破坏单二进制/跨平台 | 见 §4，这条**已在实践中漂移** |
| 4 | **Two extension tiers** —— 编译期内置（`init()` 自注册）+ 运行期外部插件（stdio JSON-RPC，MCP 兼容） | 扩展有两条路，不必都塞进内核 |
| 5 | **Interface-first & registry-based** | `Provider` / `Tool` 是接口 |
| 6 | **Evolve, don't over-engineer** | 允许先跑起来再收 |

### 3.2 先建接口层：`internal/contract/` 16 个包

迁移的第一步不是翻译实现，是**把"什么被共享"抽成一个独立层**。`contract/` 里没有实现，只有接口和类型：

`provider` · `tool` · `event` · `eventwire` · `config` · `permrule` · `planmode` · `pricing` · `surface` · `observe` · `mcpdiag` · `hostaudit` · `agentgraph` · `agentpreset` · `chartspec` · `ablation`

**这是整个迁移最聪明的一步**：接口层一独立，`model/`（厂商适配）和 `tools/`（工具实现）就变成了可替换的叶子，互不知道对方存在，只在 `runtime/` 里被组装起来。

### 3.3 自注册 + 双注册表：消除中心化开关

`SPEC.md §3.2` 定义了两种注册：

```go
// 编译期：内置工具在 init() 里自注册到一个进程级集合
func RegisterBuiltin(t Tool)
func Builtins() []Tool

// 运行期：每一次 run 装配一个 *Registry
// = 被配置启用的内置工具 + 插件提供的工具
type Registry struct{ /* ... */ }
// Agent 只看得到 *Registry
```

`Provider` 侧同理（`SPEC.md §3.1`）：

```go
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)
}

type Factory func(cfg Config) (Provider, error)
func Register(kind string, f Factory)   // 从 init() 调用
func New(kind string, cfg Config) (Provider, error)
```

**好处**：加一个 OpenAI 兼容厂商 = 一条配置；加一个新协议实现 = 一个新包 + `init()`，不动任何已有文件。`cmd/reasonix/main.go` 只做一件事：**blank-import 所有内置 provider 与 tool**，触发它们的 `init()`。

**对流式的处理**（值得单独学）：`Stream` 返回 `<-chan Chunk`，**流式工具调用增量在 provider 内部按 index 累积，只发出完整的 `ToolCall`**。这样上层拿到的一定是完整调用，不用自己拼 JSON 分片。

### 3.4 单一 Controller：一个内核，四个前端

这是 README 那句 "One local engine, four ways in" 的实现方式。《REASONIX.md》的 Conventions 写得很直白：

> **One transport-agnostic `control.Controller` sits behind every frontend (chat TUI, HTTP/SSE serve, Electron desktop). Add behavior to the controller, not a frontend, so all three inherit it.**

配套还有一条"前端对等"的强制规则：`repolint` 里的 `frontend-parity`（当前预算 28，说明历史上确实欠过债）。

### 3.5 用类型化事件流替代直接写 ANSI

从首个提交就能看出这条主线：

```
cb5f5f104 refactor(agent): emit a typed event stream instead of writing ANSI
1fb0f9fd5 refactor(cli): chat TUI consumes the typed event stream directly
982e83227 feat(control): transport-agnostic session controller (events + commands)
```

`internal/contract/event/` 有 **32 个文件**，处理事件合并（`coalesce.go`）、凭据（`receipt.go`）、预算（`run_budget.go`）、子 agent 交接（`subagent_handoff.go`）、工作区租约（`workspace_lease.go`）、通知码（`notice_codes.go`）……

**架构含义**：agent 内核不认识"终端"，也不认识"窗口"。它只发事件。谁渲染、怎么渲染，是前端的事。这是 `serve`（HTTP+SSE）、`tui`、`acp`、桌面端能共用一份内核的根本原因。

### 3.6 迁移期的兼容策略（有大量明文规则）

这部分对 Ultrac 最有直接参考价值——**迁移不是重写就完了，是重写期间老数据/老用户怎么办**。

| 维度 | 策略 | 出处 |
|---|---|---|
| **配置文件** | 2.x 读取时会**保留它不认识的 section**（如 `[bot]`）原样写回 | `MIGRATING.md` |
| **会话文件** | 按**写入版本**分流读取；2.x **永不修改 1.x 写的文件**（规则 S1）；继续使用时**复制到新会话**而非原地改（S2） | `MIGRATING.md` Sessions 规则 S1–S6 |
| **会话格式演进** | 用 schema 版本号（1 / 2 / v4）标识，而不是靠猜 | 同上 |
| **命令兼容** | 旧参数名保留为别名：`--permission-mode workspace-write` → `Auto`；`--yolo` / `bypassPermissions` 都映射到 Yolo | `MIGRATING.md` Commands |
| **Claude Code 生态** | 直接读 `.mcp.json`；`CLAUDE.md` / `AGENTS.md` / `REASONIX.md` 都认 | `GUIDE.zh-CN.md:1032` |
| **线格式冻结** | golangci 显式关掉 `modernize` 的 `omitzero` 检查，理由：**"That is a wire-format change to session files and the extension protocol, not a refactor, so it lands on its own."** | `.golangci.yml` |
| **共享目录** | 1.x 与 2.x **读写同一个 home**（`~/.reasonix` / `%APPDATA%\reasonix`），可以在一台机器上共存 | `MIGRATING.md` |

> 最后那条 `omitzero` 的注释极有价值：**一个看起来纯属代码风格的 lint 规则，被识别为"会改线格式"而单独排期**。这说明团队对"什么是重构、什么是破坏性变更"有清晰边界。

### 3.7 测试策略：测试文件比源码多

| 层 | 源码文件 | 测试文件 | 测试/源码 |
|---|---:|---:|---:|
| `internal/` 合计 | 1,985 | **2,147** | **1.08** |
| `internal/frontend/` | 337 | 401 | 1.19 |
| `internal/runtime/` | 262 | 365 | 1.39 |
| `internal/ext/` | 222 | 251 | 1.13 |
| `internal/tools/` | 85 | 107 | 1.26 |
| `internal/assembly/` | 64 | **209** | **3.27** |

注意 `assembly/boot` 的比值高达 3.27——因为《REASONIX.md》规定了一条**"效果测试"**惯例：

> **Performance features land with an effect test at their final boundary** (`internal/assembly/boot/effect_test.go` pattern): assert what actually reaches the provider request, frontend sink, or trajectory through the real `boot.Build` assembly. **Component correctness is not system effectiveness.**

翻译：**组件对了不代表系统有效**。所以性能/行为特性的验收测试必须打在"最终边界"上——透过真实的装配过程，断言真正发出去的 provider 请求、前端 sink 或轨迹。

另外还有 `go.uber.org/goleak`（goroutine 泄漏检测）和独立的 `sdk-test-race` 目标。

---

## 4. 依赖替换方案

### 4.1 现实与原则的偏差（先看这个）

`SPEC.md §1.3` 写的是：

> A third-party dependency must be pure-Go, lightweight, and must not compromise the single-binary / cross-platform / distribution story. **TOML parsing is the one accepted dependency.**

但实测 `go.mod`：**43 个直接依赖 + 23 个间接依赖**。

| 类别 | 数量 | 例子 |
|---|---:|---|
| TUI | 7 | `charm.land/bubbletea/v2`、`lipgloss/v2`、`bubbles/v2`、`chroma/v2`、`go-runewidth`、`rivo/uniseg`、`charmbracelet/x/ansi` |
| 配置与序列化 | 4 | `BurntSushi/toml`、`gopkg.in/yaml.v3`、`joho/godotenv`、`santhosh-tekuri/jsonschema/v6` |
| 存储 | 1 | `modernc.org/sqlite` |
| 代码分析 | 5 | `tree-sitter/go-tree-sitter` + JS/Python/Rust/TS 四个 grammar |
| Shell | 1 | `mvdan.cc/sh/v3` |
| 文本与匹配 | 4 | `sabhiram/go-gitignore`、`bmatcuk/doublestar/v4`、`aymanbagabas/go-udiff`、`yuin/goldmark` |
| 网络与远程 | 5 | `gorilla/websocket`、`pkg/sftp`、`kevinburke/ssh_config`、`godbus/dbus/v5`、`klauspost/compress` |
| 平台集成 | 4 | `zalando/go-keyring`、`go-toast/v2`、`atotto/clipboard`、`golang.org/x/image` |
| 安全与发布 | 3 | `golang.org/x/crypto`、`aead.dev/minisign`、`rsc.io/qr` |
| 脚本引擎 | 1 | `go.starlark.net` |
| 其他 | 8 | `spf13/pflag`、`go.uber.org/goleak`、`golang.org/x/{net,sys,text,term,mod}` 等 |

> **结论**：原则是"标准库优先，第三方要克制"，但**"TOML 是唯一依赖"这句话已经不符实**。这是可预期的漂移——文档写的是起点，实践会长出更多。**给 Ultrac 的启示：原则要写"判断标准"，不要写"具体清单"，否则必然过期。**

### 4.2 依赖替换对照表（TS 生态 → 实测 Go 选型）

| 领域 | Ultrac 现状（TS） | Reasonix 实测选型 | 版本 | 备注 |
|---|---|---|---|---|
| TUI 框架 | Ink + React 19 + react-reconciler | **`charm.land/bubbletea/v2`** | v2.0.10 | 正是 Bubble Tea v2（2026-02 发布，vanity 域） |
| TUI 样式 | chalk | `charm.land/lipgloss/v2` | v2.0.6 | — |
| TUI 组件 | 自研 390 个组件 | `charm.land/bubbles/v2` | v2.2.1 | — |
| Markdown | `marked` | `yuin/goldmark` | v1.8.6 | — |
| 语法高亮 | `highlight.js` | `alecthomas/chroma/v2` | v2.27.0 | — |
| 终端宽度 | `get-east-asian-width` + `string-width` | `mattn/go-runewidth` + `rivo/uniseg` | v0.0.30 / v0.4.7 | 中文对齐的关键 |
| ANSI 处理 | `strip-ansi` / `wrap-ansi` | `charmbracelet/x/ansi` | v0.11.8 | — |
| **Schema 校验** | **`zod`**（132 文件 / 331 处） | **`santhosh-tekuri/jsonschema/v6`** | v6.0.3 | 与我的推荐一致 |
| JSON Schema 生成 | 由 zod 顺带产出 | 工具接口直接给 `json.RawMessage` | — | 见 §5.2 |
| glob | `picomatch` | `bmatcuk/doublestar/v4` | v4.10.2 | — |
| gitignore 语义 | `ignore` | `sabhiram/go-gitignore` | — | — |
| **Shell 解析** | **`shell-quote`** | **`mvdan.cc/sh/v3`** | v3.14.1 | **真实 bash AST，非分词** |
| 文本 diff | `diff` v9 | `aymanbagabas/go-udiff` | v0.4.1 | — |
| 内联 diff 着色 | `color-diff-napi`（原生模块） | 纯 Go | — | 原生依赖被消除 |
| 文件监听 | `chokidar` | **无**（未引入 fsnotify） | — | 反直觉，见下 |
| 本地存储 | JSONL 文件 | `modernc.org/sqlite` | v1.60.1 | **纯 Go，免 CGO** |
| 配置解析 | 自研分层（env→project→user） | `BurntSushi/toml` + 自研分层 | v1.6.0 | 未用 viper/koanf |
| 环境变量文件 | `dotenv` 类 | `joho/godotenv` | v1.5.1 | — |
| 凭据存储 | 文件 | `zalando/go-keyring` | v0.2.8 | 走系统钥匙串 |
| WebSocket | `ws` | `gorilla/websocket` | v1.5.3 | 与我的推荐不同（我推 coder/websocket） |
| 二维码 | `qrcode` | `rsc.io/qr` | v0.2.0 | — |
| 压缩 | `zlib`（内置） | `klauspost/compress` | v1.20.1 | — |
| SSH / SFTP | 无 | `pkg/sftp` + `kevinburke/ssh_config` | — | Remote-SSH 模块 |
| 代码解析 | 无 | `tree-sitter` × 5 | — | `code_index` 工具 |
| 脚本引擎 | 无 | `go.starlark.net` | — | `internal/tools/script` |
| 发布 | 手工 | `.goreleaser.yaml` + `aead.dev/minisign` | — | 签名发布 |
| 结构化日志 | 自研 | 标准库（未引入 zap/zerolog/slog 封装） | — | 见下 |
| 遥测 | OpenTelemetry JS | `internal/platform/telemetry` 自研 | — | — |

### 4.3 三个"没选"的选择，比"选了"更值得看

| 决策 | Reasonix 的做法 | 通常建议 | 怎么理解 |
|---|---|---|---|
| **MCP 客户端** | **自研**。`internal/ext/plugin/` 约 120 个文件，含 `transport_stdio.go` / `transport_http.go` / `transport_sse.go` / `oauth.go` / `oauth_pkce.go` / `oauth_refresh.go` / `elicit.go` | 用官方 `modelcontextprotocol/go-sdk` | go.mod 里**没有**官方 SDK。走的是"协议很简单，但适配层很复杂"的判断——他们需要按 function 粒度控制 OAuth 状态目录、凭据轮转、进程沙箱绑定、按调用注 `_meta.progressToken`，这些 SDK 未必留了口子。**代价是约 120 个文件的自维护量** |
| **LLM 客户端** | **自研** OpenAI 兼容实现（`internal/model/openai`），外加 `anthropic` / `responses` / `laya` 三套 | 用 `anthropics/anthropic-sdk-go` | 因为要支持任意 OpenAI 兼容端点 + 保持"加厂商 = 改配置"，官方 SDK 的强类型反而成了约束 |
| **CLI 框架** | **`spf13/pflag` 单库**，子命令路由自研（`internal/frontend/cli`） | cobra | 实测 **cobra 0 个文件引用**。为了控制依赖面 |
| **测试断言** | **标准库 `testing`** | testify | 实测 **testify 0 个文件引用** |
| **配置库** | 自研分层 + `BurntSushi/toml` | viper / koanf | 实测 **两者均 0 个文件引用** |
| **文件监听** | 不引入 `fsnotify` | fsnotify | 没有需要持续监听的点 |

> **模式**：凡是"会侵入内核控制流"的依赖，他们倾向自研（MCP / Provider / CLI 路由 / 配置）；凡是"纯能力"的依赖，直接用库（TUI / 解析 / 压缩 / 匹配）。
>
> **对 Ultrac 的修正**：我上一轮推荐 MCP 用官方 Go SDK。Reasonix 的实践证明**自研 MCP 也是可行路线**，但代价明确——约 120 个文件。**建议：Ultrac 先用官方 SDK 跑通，把自研当备选**，因为 Ultrac 的人力约束比 Reasonix 严得多。

---

## 5. 关键模块对应关系

### 5.1 Claude Code 概念 → Reasonix 包

| Claude Code 概念 | Reasonix 位置 | 说明 |
|---|---|---|
| `QueryEngine` / agent 主循环 | `internal/runtime/agent` | `Run(ctx, input)` 循环，`maxSteps` 上界 |
| 工具契约 `Tool.ts` | `internal/contract/tool` | 只有 4 个方法（见 §5.2） |
| 工具注册表 `tools.ts` | `internal/tools/builtin` + 运行期 `*Registry` | 双注册表：进程级内置 + 每 run 装配 |
| 工具实现 `tools/*` | `internal/tools/{builtin,shellrun,jobs,script,advisor,sessiontool,productdocs}` | 29 个内置工具 |
| 权限 `utils/permissions/` | `internal/safety/{permission,shellsafe,typesafe}` + `internal/contract/permrule` | 权限规则独立成 contract |
| 沙箱 | `internal/safety/{sandbox,egress,redirectguard}` | 进程沙箱 + 出网白名单 |
| MCP 客户端 | `internal/ext/plugin` | 自研，三种传输 |
| Hooks | `internal/ext/hook` | — |
| Skills | `internal/ext/skill` | — |
| 斜杠命令 `commands/` | `internal/ext/command` | 从 `.reasonix/commands/*.md` 加载 |
| 会话存储 `sessionStorage.ts` | `internal/state/{sessionstore,sessionv4,history}` | schema 版本化 |
| 上下文压缩 | `internal/contract/{planmode}` + `runtime` 压缩逻辑 | `compact_ratio` 阈值驱动（默认 0.85） |
| 记忆 `memdir/` | `internal/state/memory` + `internal/state/instruction` | 读 `REASONIX.md` / `AGENTS.md` / `CLAUDE.md` |
| Checkpoint / rewind | `internal/state/checkpoint` | **文件快照**，与 CC 一致（不追踪 bash 副作用） |
| 子 agent / 委派 | `internal/runtime/{delegation,coordinator,agentgraph}` | 见 §5.3 |
| 计划模式 | `internal/contract/planmode` + `internal/runtime/plancontract` | — |
| 遥测 | `internal/platform/telemetry` + `internal/contract/observe` | — |
| TUI | `internal/frontend/tui` + `termrender` | — |
| CLI 入口 | `internal/frontend/cli` + `cmd/reasonix` | — |

### 5.2 两个核心接口（完整定义）

**`Tool`（`internal/contract/tool`）** —— 注意它比 Claude Code 的 `Tool` **窄得多**：

```go
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage                                  // JSON Schema
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}
```

对比 Ultrac/CC 的 `Tool` 类型（约 30 个成员：`isConcurrencySafe` / `isReadOnly` / `isDestructive` / `interruptBehavior` / `isSearchOrReadCommand` / `isOpenWorld` / `requiresUserInteraction`…）。

**Reasonix 把这些语义搬走了**：

- `ReadOnly()` 由 MCP 的 `annotations.readOnlyHint` 映射（默认 `false`——"a remote tool is opaque"）
- 并发安全 / 破坏性 → 权限层与工具调度器（`runtime/schedrun`）
- 交互需求 → 事件流 + `frontend` 处理

**关键设计取舍（`SPEC.md §3.2` 原文）**：

> `Execute` parses raw JSON args itself. **Errors are returned, not fatal — the agent feeds them back so the model can self-correct.**

即：工具报错是**正常返回值**，会作为 tool_result 回喂给模型让它自己纠错。这与我在 Ultrac 计划里 `Result.IsError` 的设计意图一致，但 Reasonix 更彻底——**直接用 Go 的 `error` 承载**，没有单独的 `IsError` 字段。

**`Provider`（`internal/contract/provider`）**：

```go
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)
}
```

**流式用 `<-chan Chunk`，不是 `iter.Seq`。** 这点与我给 Ultrac 的推荐（`iter.Seq2[Event, error]`）不同——两者都是 Go 惯用法，`channel` 更早普及、`iter.Seq` 更晚（Go 1.23+）。Reasonix 用 Go 1.26 却仍选 channel，说明在"跨 goroutine + 可取消 + 背压"场景下 channel 的表达力依然够用。

### 5.3 子 agent 委派：五个概念严格分离

`REASONIX.md` 的 Conventions 单独列了一条，值得抄：

> Subagent delegation keeps five concepts apart:
> - a **profile** says how a worker **thinks**
> - **`TaskSpec`** what **this call** wants
> - **`CapabilityGrant`** what it **may touch**
> - **`ContextRequest`** what it **starts from**
> - **`SchedulerPolicy`** when it **runs**
>
> Put a field in whichever member decides its value — **profiles carry ceilings, never per-call values.**

并有专门的测试强制它：`internal/runtime/delegation/profile_boundary_test.go`。

**这条规则的普适价值**：**一个字段放在"决定它值的那一方"**。Profile 只能表达"上限"（ceiling），不能表达"这次调用要什么"——否则 profile 会变成每次调用的参数垃圾桶。

同理，两模型协作的设计也很克制（`SPEC.md §3.5`）：planner 与 executor **在各自独立的 session 里跑**，理由是——**在同一段对话里切模型会破坏 prompt 前缀，从而打掉前缀缓存**。所以"两模型协作"和"缓存优先"不是矛盾的，实现方式是**不共享会话**。

### 5.4 29 个内置工具

```
bash            bash_output     kill_shell      wait
read_file       write_file      edit_file       multi_edit
delete_range    delete_symbol   move_file       ls
glob            grep            code_index      notebook_edit
todo_write      update_goal     complete_step   context_budget
compress        recall          system_one      web_fetch
browser_open    browser_read    browser_act     computer_read
computer_act
```

对照 Claude Code 的工具集，能看出取舍：

| CC 有的 | Reasonix | 说明 |
|---|---|---|
| `Bash` | ✅ `bash` + `bash_output` + `kill_shell` + `wait` | **拆成 4 个**——后台作业是一等公民 |
| `Read` / `Write` / `Edit` | ✅ `read_file` / `write_file` / `edit_file` / `multi_edit` | 命名 snake_case |
| `Glob` / `Grep` / `LS` | ✅ `glob` / `grep` / `ls` | — |
| `WebFetch` / `WebSearch` | ⚠️ 只有 `web_fetch` | 无独立 web_search |
| `TodoWrite` | ✅ `todo_write` | — |
| `Task` / `Agent` | ❌ | 委派在 `internal/runtime/delegation`，不走工具面 |
| `NotebookEdit` | ✅ `notebook_edit` | — |
| `LSP` | ❌ 工具面没有 | LSP 在 `internal/platform/lsp`，作为服务而非工具 |
| — | ➕ `code_index` | tree-sitter 代码索引 |
| — | ➕ `delete_range` / `delete_symbol` | **结构化删除**，不靠字符串匹配 |
| — | ➕ `context_budget` / `compress` / `recall` | 上下文管理提到工具面 |
| — | ➕ `browser_*` / `computer_*` | 浏览器与桌面操作 |
| — | ➕ `update_goal` / `complete_step` / `system_one` | 目标与步骤契约 |

---

## 6. 工程门禁体系（本项目最值得学的部分）

### 6.1 `tools/repolint`：把架构不变量编译成检查

`tools/repolint/` 是一个**自研的仓库级 linter**，约 40 个检查器文件。实测清单：

| 检查器 | 作用 |
|---|---|
| **`layers.go`** | **分层规则**（frontends / leaves / hostAdapters），`layering` 预算 = **0** |
| `layer_order.go` | 层序一致性，预算 = **0** |
| **`baseline.go` / `baseline.json`** | **ratchet 基线**：冻结历史欠债，只许降不许升 |
| `changed.go` | `-changed` 只检查工作区改动（本地快速循环） |
| **`structstate.go`** | 按**标量字段数**给 mutex/atomic 保护的 struct 设棘轮——"独立 flag 会乘出类型系统不承认的合法状态" |
| **`windowstate.go`** | 上下文窗口归属检查：`window-state` 只允许通过 `a.window()` 读 `sess.win`，预算 = **0** |
| **`errortext.go`** | 禁止新增"对错误消息字符串做匹配"的代码 |
| **`refusal.go` / `inboxrefusal.go`** | 禁止在 `internal/frontend/serve` 新增 `http.Error`，要求走带点号编码的 `refuse` |
| **`wireparity.go`** | 线协议对等，预算 = **0** |
| `frontendparity.go` | 前端对等（CLI / TUI / 桌面行为一致） |
| `frontendtypes.go` | 前端类型一致性 |
| `complexity.go` / `size.go` | 复杂度与文件/函数体积 |
| `comments.go` / `docs.go` | 注释与文档规范（含 `doc-owner` 文档责任人） |
| `codeowners.go` | CODEOWNERS 完整性，预算 = **0** |
| `dialect.go` | 含一条 **`claude-dialect`** 规则（预算 1） |
| `orphan.go` | 孤立文件检测 |
| `tagparity.go` | 构建 tag 对等 |
| `flatview.go` / `buildartifact.go` / `source.go` / `only.go` | 结构、构建产物、来源、唯一性检查 |
| `codeowners.go` | — |

### 6.2 Ratchet 基线：23 条规则，12 条零容忍

`baseline.json` 结构 = `{ limits: {规则名: 预算}, files: {文件: {...}} }`，`files` 有 **469** 条按文件冻结的条目。

| 预算 = 0（零容忍，13 条） | 有预算（11 条） |
|---|---|
| `layering` 0 | `complexity` 759 |
| `layer-order` 0 | `function-size` 2946 |
| `wire-parity` 0 | `file-size` 20867 |
| `window-state` 0 | `essay` 2737 |
| `refusal-path` 0 | `doc-prose` 2147 |
| `flat-view` 0 | `release-note` 661 |
| `banner` 0 | `orphan` 75 |
| `build-artifact` 0 | `struct-state` 31 |
| `codeowners` 0 | `frontend-parity` 28 |
| `commented-code` 0 | `doc-owner` 27 |
| `doc-language` 0 | `error-text` 9 |
| `marker` 0 | `claude-dialect` 1 |
| `narrative` 0 | |

**这个机制的精妙之处**：它解决了"大项目想加严格规则但一加就红一片"的死结。

1. 新规则可以立刻加进来，历史违例进 `baseline.json` 冻结
2. 之后**只许降不许升**（`-update` 可记录下降，放宽需要显式 `-allow-drop`）
3. **架构关键规则一开始就设 0**：`layering` / `layer-order` / `wire-parity` 从第一天就是铁律

> **这是 Ultrac 最该照抄的一件事。** 513k 行存量 + 0 测试的情况下，你不可能一次把规则加严；但你可以**先冻结现状，再让它单调下降**。

### 6.3 Makefile 的质量门禁

| 目标 | 命令 | 作用 |
|---|---|---|
| `build` | `CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=..."` | 静态二进制 + 版本注入 |
| `lint` | `lint-go` + `go run ./tools/repolint` | 双门禁 |
| `check` | `go run ./tools/repolint -changed` | **只查改动**（快循环） |
| `lint-update` | `go run ./tools/repolint -update` | 重新基线 |
| **`coverage-gate`** | `go run ./tools/covergate` | **声明的敏感路径覆盖率不许下降**；降低需 `-allow-drop` |
| `pricecheck` | `go run ./tools/pricecheck` | 抓厂商定价页核对表（走网络，**故意不进 CI**） |
| `lint-cross` | 循环 `GOOS=linux/darwin/windows` × 含 `desktop` | "Linting one GOOS leaves every `//go:build windows` and `darwin` file unchecked" |
| `test` | `studio-test` + `sdk-test` + `go test ./...` | 覆盖嵌套模块 |
| `sdk-test-race` | `cd sdk/go && go test -race ./...` | SDK 竞态检测 |
| `cross` | 6 目标循环 | 交叉编译 |
| `hooks` | `git config core.hooksPath .githooks` | pre-push 跑 `go vet` |

关于 `check` 目标有一段很有意思的注释，说明为什么要做成 Makefile 目标而不是让人手敲命令：

> It exists as a target rather than a command to paste because **the host recognizes `make check` as a verification it can read the result of**, while an arbitrary `go run ./tools/...` is an unknown program it must assume writes — so only this form can be cited as evidence that the standards gate passed.

即：**为了让人和 AI 都能把"门禁通过"当作可引用的证据。**

### 6.4 三条可以直接搬进 Ultrac 的工程约定

**① Judgements read structure, never wording（判断读结构，不读措辞）**

> Judgements read structure, never wording: shell ASTs (`shellparse`), types, contracts, tool schemas. **Phrase tables and message sniffing have been retired in batches** (task-policy prose, the planner's approval phrases, the executor handoff tables) because each one misfires on real input, and **a misfiring judgement inside a gate is worse than no gate.** … **Something that can only be built by matching words does not get built — say what is missing instead of guessing at it.**

这条对 Ultrac 直接相关：Ultrac 里大量权限判定、`bashClassifier`、`shadowedRuleDetection`、`dangerousPatterns` 都是**对 bash 命令做模式/措辞匹配**。Reasonix 的做法是**解析成 AST 再判断**（`internal/base/shellparse`，用 `mvdan.cc/sh`）。

**② Errors carry an identity, never a sentence（错误带身份，不带句子）**

> an error a caller must tell apart **carries an identity** — a sentinel, a typed code — never a sentence. **Only the producer knows a deadline expired rather than a socket closed, and a message is where that knowledge goes to die**: the reader is left matching words, or with nothing.
> - 跨 HTTP：带点号的错误码，经 `refuse`
> - Go 内部：`errors.Is` 对 sentinel，包装须保留

而且有 linter 强制：`errortext` 检查器**失败于任何新增的"对错误消息做匹配"的代码**。

**③ Whoever first knows why an operation failed owns its class（谁最先知道失败原因，谁定义失败类别）**

> A transport status is not a domain identity, and a display string is not a classification.

---

## 7. 对 Ultrac 的迁移结论

### 7.1 被证实的（我上一轮的推荐，这里有实测支撑）

| 我的推荐 | Reasonix 实证 |
|---|---|
| TUI 用 Bubble Tea v2 | ✅ `charm.land/bubbletea/v2 v2.0.10` + `lipgloss/v2` + `bubbles/v2` |
| SQLite 用纯 Go（免 CGO） | ✅ `modernc.org/sqlite v1.60.1` |
| Shell 解析用 `mvdan.cc/sh` 而非分词库 | ✅ `mvdan.cc/sh/v3 v3.14.1`，且被提升为"判断读结构"的基石 |
| JSON Schema 校验用 `santhosh-tekuri/jsonschema/v6` | ✅ 同版本 v6.0.3 |
| glob 用 `doublestar/v4` | ✅ 同库 |
| gitignore 用 `sabhiram/go-gitignore` | ✅ 同库 |
| diff 用 `go-udiff` | ✅ 同库 |
| 终端宽度用 `go-runewidth` | ✅ 同库 + `rivo/uniseg` |
| goroutine 泄漏用 `goleak` | ✅ `go.uber.org/goleak` |
| 交叉编译 + goreleaser | ✅ `.goreleaser.yaml` + `-s -w` + 版本注入 + 6 目标 |
| 保留 TS 外壳、Go 做引擎 | ✅ **桌面端 760 个 TS 文件建在 Go 内核之上**，通过 HTTP+SSE 通信 |
| 分层 + 依赖方向单向 | ✅ 且**用 linter 强制，预算 = 0** |
| 大小写/命名保持 snake_case 工具名 | ✅ 全部工具 snake_case |

### 7.2 需要修正的

| 我的推荐 | 修正 | 依据 |
|---|---|---|
| MCP 用官方 Go SDK | **改为"先官方、自研作备选"** | Reasonix 实测自研约 120 文件，go.mod 无官方 SDK。自研能控 OAuth 状态目录、凭据轮转、进程沙箱绑定——但人力代价高 |
| `iter.Seq2[Event, error]` 做流式 | **channel 也是可行解**（`<-chan Chunk`） | Reasonix 用 Go 1.26 仍选 channel。两者都可，channel 在跨 goroutine 场景表达力够 |
| CLI 用 cobra | **可降级为 pflag + 自研路由** | Reasonix 实测 cobra 0 引用。若 Ultrac 子命令层级简单，cobra 不是必需 |
| 配置用 koanf | **可降级为 pnpm… 抱歉，是 toml 库 + 自研分层** | Reasonix 用 `BurntSushi/toml` + 自研优先级合并，koanf/viper 均 0 引用。**但 Ultrac 的配置语义比 Reasonix 复杂得多**（`SAFE_ENV_VARS` 白名单、`PROVIDER_MANAGED_BY_HOST` 剔除），建议仍用显式 merge 的 koanf |
| 测试用 testify + go-cmp | **标准库就够** | Reasonix 实测 testify 0 引用。但 Ultrac 从 0 开始建测试，testify 能显著提速，**这条我保留推荐** |
| WebSocket 用 `coder/websocket` | **gorilla/websocket 仍在用** | Reasonix 选 gorilla v1.5.3 |

### 7.3 新发现（我上一轮没提到的）

| # | 发现 | 对 Ultrac 的价值 |
|---|---|---|
| 1 | **`repolint` + `baseline.json` ratchet** | **最高价值**。513k 存量 + 0 测试的现实下，这是唯一可行的"逐步收紧"路径 |
| 2 | **测试文件数 > 源码文件数（2147 vs 1985）** | 直接量化了"重写的前提是测试"这个判断 |
| 3 | **`contract/` 独立成层（16 个包，纯接口）** | 迁移第一步应该是抽接口层，而不是翻译实现 |
| 4 | **`init()` 自注册 + 双注册表** | 消除中心化 `switch`，加厂商/工具不动已有文件 |
| 5 | **类型化事件流替代直接写 ANSI** | 四个前端共用内核的根本原因；Ultrac 的 `SDKControl*` 已经走在正确路上 |
| 6 | **"效果测试"打在最终边界**（`boot.Build` 断言真实 provider 请求） | 解决"组件对了但系统不对" |
| 7 | **`coverage-gate`：敏感路径覆盖率不许下降** | 比"达到 X% 覆盖率"更可执行 |
| 8 | **`lint-cross`：单 GOOS lint 会漏掉别的平台 build tag 文件** | Windows 平台代码多的话必须配 |
| 9 | **嵌套 module 会让根目录 `go test ./...` 静默漏测** | 若拆模块必须配独立测试目标 |
| 10 | **`errortext` linter：禁止对错误消息做字符串匹配** | 与 Ultrac 的 D6 决策（错误建模）完美互补 |
| 11 | **"判断读结构不读措辞"** | 直接指向 Ultrac 的 `bashClassifier` / `dangerousPatterns` 风险区 |
| 12 | **`structstate`：按标量字段数给并发结构设棘轮** | 防"再加一个 bool"式的状态爆炸 |
| 13 | **文档即契约**：`SPEC.md` 声明"code follows it. Change the contract first, then the code" | Ultrac 只有 `docs/config-loading.md`，缺一份正式 Spec |
| 14 | **迁移期兼容规则明文编号**（会话 schema S1–S6；配置保留未知 section；lint 规则识别"会改线格式"而单独排期） | Ultrac 的契约冻结清单可以直接引用这套写法 |
| 15 | **两模型协作 = 两个 session**（不为跨模型共享会话，以保前缀缓存） | 若 Ultrac 未来加 planner，这是正确姿势 |
| 16 | **委托五概念分离**（profile / TaskSpec / CapabilityGrant / ContextRequest / SchedulerPolicy），profile 只写上限 | 若 Ultrac 移植子 agent 体系，这是现成的职责划分 |

### 7.4 一句话总结

> Reasonix 证明的**不是**"513k 行 TS 可以翻译成 Go"，而是"**一个 Go 内核 + 一个 TS 外壳 + 一套能强制执行的架构门禁 + 比源码更多的测试**，可以在 19 周内长到 80 万行"。
>
> 它的可迁移性不在代码，在**方法**：先把契约写成文档，把接口抽成独立层，把架构约束写成 linter，把存量欠债冻结成基线，然后让边界只允许收紧。

---

## 附录 A：关键文件索引

| 用途 | 路径（相对 `DeepSeek-Reasonix/`） |
|---|---|
| 工程契约（先读这个） | `docs/SPEC.md` |
| 仓库约定与纪律 | `REASONIX.md` |
| TS/Go 边界规则 | `docs/STUDIO_SHELL_BOUNDARIES.md` |
| 1.x → 2.x 迁移规则 | `docs/MIGRATING.md` |
| 1.x 路线图与未决事项 | `docs/ROADMAP.md` |
| 工具契约 | `docs/TOOL_CONTRACT.md` |
| Checkpoint / rewind | `docs/CHECKPOINTS.md` |
| 分层强制规则 | `tools/repolint/layers.go` |
| Ratchet 基线 | `tools/repolint/baseline.json` |
| 覆盖率门禁 | `tools/covergate/` |
| 构建与门禁入口 | `Makefile` |
| Lint 配置 | `.golangci.yml` |
| 核心接口 | `internal/contract/{provider,tool,event}/` |
| 会话控制枢纽 | `internal/session/control/` |
| 组装根 | `internal/assembly/boot/` |
| MCP 自研实现 | `internal/ext/plugin/` |
| 内置工具 | `internal/tools/builtin/` |
| 参考插件（可运行） | `cmd/reasonix-plugin-example/` |

## 附录 B：本次分析的证据边界

| 我核实了的 | 我没能核实 / 需要说明的 |
|---|---|
| 文件数与代码行数（实测统计） | **TS→Go 的迁移过程**：本仓历史被重置过，首个提交即 Go。前身项目代号 `duo`，但 `duo` 不在本机，其源码语言构成无法验证 |
| git 历史起点与提交信息 | 1.x 之前是否曾有 TS 实现——**无法验证** |
| `go.mod` 全部依赖 | 1.x 与 2.x 各自的完整依赖演进史——只对比了当前快照 |
| `repolint` 检查器清单与基线数值 | `docs/SPEC.md §2` 的目录布局**已落后于实际代码**（仍是旧版 `internal/cli`、`internal/provider` 结构），本报告的目录结构以实测为准 |
| 设计原则、约定、边界规则（文档原文引用） | 部分内部设计文档（`docs/design/`）未逐篇通读 |

