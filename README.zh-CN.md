# Ultrac

[English](README.md) | **简体中文**

> 一个 Go 编写的编码 agent 引擎 —— 从零移植 **pi core harness**
> （agent loop、事件协议、会话格式、工具、print/RPC 模式）。
> 思考、行动、观察、循环 —— 单个静态二进制搞定。

Ultrac 是一个面向软件工程任务的终端原生 AI agent。它读取你的代码库，运行工具
（bash、文件编辑/写入、read、ls、grep、find），调用语言模型，并循环执行
*思考 → 行动 → 观察*，直到任务完成。

> ⚠️ **状态**：`1.0.0-dev`。引擎已完成（移植计划 P0–P7，所有验收关卡通过）；
> CLI 表面仍是 pi 的无头表面——按设计不提供交互式 TUI。

---

## 🎯 这是什么

Ultrac 的引擎（`cmd/`、`internal/`）是 **pi core harness 的 Go 实现**，取代早期
Claude-Code 形态的 TypeScript 重建。移植计划——决策、偏差与逐阶段验收记录——在
[`docs/开发计划/pi-core-harness-plan.md`](docs/开发计划/pi-core-harness-plan.md)（这项工作的宪法）。

**目标（全部达成）**

| # | 目标 | 度量方式 |
|---|---|---|
| G1 | 可跑通真实编码任务 | agent 自主完成 read→edit→bash 闭环，无需人工干预 |
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

## 🚀 快速开始

前置要求：**Go ≥ 1.26**，以及 `~/.ultrac/settings.json` 里的 provider 凭据
（`env` 块：`ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_MODEL` 等，
兼容任意 anthropic-messages 或 openai-completions 端点）。

```bash
# 构建（或：make build）
go build -o bin/ultrac ./cmd/ultrac

# 单条 prompt，print 模式 —— agent 跑完工具后退出
./bin/ultrac -p "Read go.mod and say the module name in one sentence"

# 事件流，每行一个 JSON 对象
./bin/ultrac --mode json -p "List the Go files under internal/session"

# 恢复该目录下最近的会话
./bin/ultrac -c "now also count the test files"

# RPC 模式：stdin/stdout 上的无头 JSONL 协议
./bin/ultrac --mode rpc

# 诊断
./bin/ultrac config                                   # 配置层级 + 生效的 env
./bin/ultrac sessions [dir]                           # 列出某目录的会话
./bin/ultrac probe -mode anthropic                    # 一次真实请求，覆盖 12 类事件
```

`make live-agent` 运行只读的真实端点验收 prompt。

## 🖥️ 桌面端（Electron + React SPA）

桌面 UI 是 `desktop/` 下的两个包（旧的 `reasonix-desktop/` 目录是 Reasonix 壳的
**只读参考副本**——已 gitignore，**不要**运行它）：

- `desktop/shell/` —— Electron 壳。spawn `bin/ultrac --mode rpc`，并通过 stdio
  桥接 C4 JSONL 协议（管道*本身*就是安全边界——无需 token/网关）。
- `desktop/shell-spa/` —— React 19 + Vite + TypeScript SPA，即加载进窗口的聊天界面。

完整设计见 [`docs/开发计划/desktop-shell-plan.md`](docs/开发计划/desktop-shell-plan.md)。

**前置要求**

- Go ≥ 1.26（用于构建内核——本仓库已构建为 `bin/ultrac.exe`）
- Node ≥ 22.18（本机的托管 `node` 22.22.2 可用）
- `~/.ultrac/settings.json` 里的 provider 凭据（与 CLI 相同——见上文）

**构建与启动**

```bash
# 1. 构建 Go 内核（已存在于 bin/ultrac.exe；改了 Go 代码才需要重编）
go build -o bin/ultrac ./cmd/ultrac

# 2. 构建 SPA —— 产出 desktop/shell-spa/dist/（窗口加载这个）
cd desktop/shell-spa
npm install
npm run build
cd ../..

# 3. 安装 Electron 壳并启动
cd desktop/shell
npm install          # 拉取 electron ^44（使用 .npmrc 里的 npmmirror 镜像）
npm start            # 运行 `electron .`
```

壳会自动找到内核 `bin/ultrac.exe`（dev 路径 `desktop/shell/src/../../../bin`）
和页面 `desktop/shell-spa/dist`。可用环境变量 `ULTRAC_BIN` / `ULTRAC_SHELL_PAGE`
覆盖。

> ⚠️ **Windows / 本机坑**：环境全局设置了 `ELECTRON_RUN_AS_NODE=1`，这会让
> `electron .` 退化成纯 Node（没有 GUI 窗口）。启动前需 unset：
>
> ```bash
> cd desktop/shell
> env -u ELECTRON_RUN_AS_NODE npm start
> ```

**各部分如何协作**（一句话）：Electron 窗口加载 SPA；SPA 通过壳的 preload 桥
（`kernel:call` / `kernel:event` / `session_loaded` / `shell:open-external`）与壳
通信；壳通过 stdio spawn `ultrac --mode rpc`。关闭窗口即关闭 stdin，内核随之退出
（父进程租约语义）——不需要额外管道代码。

**SPA 开发循环**（热更新，不用 Electron）：`cd desktop/shell-spa && npm run dev`
在 Vite dev URL 提供 UI；指向一个运行中的 RPC 内核即可实时测试。

## 🗂️ 项目结构

```
cmd/ultrac/          # CLI：flags、print 模式、RPC 模式、诊断、共享装配
desktop/            # Electron 壳 + React SPA（见上文「🖥️ 桌面端」）
├── shell/           #   Electron 壳 —— spawn `ultrac --mode rpc`，JSONL 桥
└── shell-spa/       #   React 19 + Vite SPA —— 加载进窗口的聊天界面
internal/
├── agent/           # C3：agent loop、Agent 状态机、工具调用执行
├── ai/              # C1+C2：消息/事件类型、provider 注册表、dialect 适配器
│   ├── anthropic/   #   anthropic-messages 适配器
│   └── openai/      #   openai-completions 适配器
├── config/          # 配置层级、env 应用、路径解析
├── prompt/          # 分段系统提示词（sections 补丁语义）
├── rpc/             # C4：RPC 命令/响应类型 + JSONL 服务端
├── session/         # pi 形态的 SessionManager（header + 条目树 + uuidv7）
├── tool/            # 8 个工具（bash、edit、find、grep、ls、read、write + powershell）
├── transport/       # pi 线协议 writer（print/json 与 RPC 共用）
├── logging/         # 按日期切分的 error/MCP 日志
└── version/         # 构建元数据
```

## 🧪 与 pi 对照验证

验收关卡是一次**双跑**：同一 prompt 分别经过真实的 pi 和 ultrac，对事件类型序列
做 diff。harness 脚本（本地，未跟踪——见 `bin/`）：

```bash
bash bin/double-run.sh "Reply with exactly: E2-OK"
```

pi 侧的配置说明（node/npm、`dsanthropic` provider-id 冲突的绕法、`</dev/null`）
记录在脚本头部注释和计划的 P7 记录里。

## ⚙️ 配置

分层解析（用户 `~/.ultrac/settings.json` → 项目 `.ultrac/settings.json` →
环境变量 → 默认值），`ultrac config` 会报告 host 管理与安全白名单的过滤情况。
解析顺序描述在 [`docs/项目理解/config-loading.md`](docs/项目理解/config-loading.md)（为 TS 时代写的；
Go 层镜像了同样的顺序——见 `internal/config/`）。

## 📜 关于 TypeScript 树

最早的 Claude-Code 形态 TypeScript 重建（`src/`，约 51.3 万行，Ink TUI，bun 构建）
已在 **P7 退役**：引擎转向 pi 语义后它不再有消费者（计划 §0.2 / 风险 R11），且移植
已完成并通过双跑验证。完整代码树仍保留在 git 历史中（P7 之前的提交）；Go 引擎才是
实现。

## 📄 许可

内部重建 / 研究用途。与 Anthropic 无关联，也未获其背书。所有商标归各自所有者。
