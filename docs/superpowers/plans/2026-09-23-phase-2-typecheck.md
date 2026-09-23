# Phase 2 — 类型检查通过实施计划

> **对代理worker：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 按任务逐步执行此计划。步骤使用复选框 (`- [ ]`) 语法跟踪进度。

**目标：** `bunx tsc --noEmit` 对全部 1,911 个源文件零错误。

**架构：** 按根因分四层推进，每层独立可验证（错误计数单调下降）：① 编译配置与依赖版本对齐（`lib`、`@types/bun`、`@anthropic-ai/sdk`、6 个漏录外部包）；② 用脚本为 ~95 个"源码里被引用但磁盘上不存在"的本地模块生成声明文件草稿，再按引用点精修；③ 归一化 95 处构建变体字面量比较；④ 按错误码分战役清零残余真实类型错误。全程保持 `bun build` 不回归。

**技术栈：** Bun 1.3.14、TypeScript 5.x、`tsc --noEmit`

**规格说明：** `docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md`（§3 Phase 2）

---

## 全局约束

- Phase 2 交付物（spec §3）：`tsc --noEmit` 零错误；所有模块可解析。
- **回归门：每个任务提交前必须执行 `bun build src/entrypoints/cli.tsx --target=bun --outdir=dist` 并确认退出码 0**（Phase 1 交付物，Phase 2 不得破坏）。
- `strict: false` 保持不变（Phase 1 Task 2 决定）。不要为了提高"类型质量"打开 strict——本阶段目标是清零，不是加严。
- **React 三件套版本冻结不动：** `react` 保持 `^18.3.1`、`ink` 保持 `^5.0.0`、`react-reconciler` 保持 `^0.29.2`（Phase 1 Task 8a 规则）。源码里 7 个文件 `import { use } from 'react'`（React 19 API）**不得**通过升级 React 解决，按 Task 12 的配方处理。
- 依赖版本只允许"源码可证明需要更新"时升级（目前只有 `@anthropic-ai/sdk`）。每次升级按 spec §0 在 §9.2 留修订记录。
- 4 个 `@ant/*` 内部包继续用 `declare module` + 本地 shim（spec §9.4-A），**禁止** `bun add`。
- **禁止 `// @ts-nocheck`**（整文件放弃检查等于规避交付物）。**禁止无注释的 `as any`**——确需压制时必须写 `// @ts-expect-error TSxxxx - <原因>` 或 `as unknown as X // <原因>`。
- 新建的桩/声明文件必须是**真实 `.ts` 文件**（不是 ambient `declare module`），保证 bun 运行时也能解析；文件顶部加 `// TODO: Phase 6 - 还原 <模块名> 真实实现`。
- 每个任务开始前记录 `bunx tsc --noEmit 2>&1 | grep -c "error TS"`，结束后再记一次，写进提交信息。
- 文档与实测不符时按 spec §0 当场修订（保留 `> YYYY-MM-DD 修订：<原判断> → <新判断>。<原因>` 行）。

---

## Review Focus

以下五类是 spec 暗示但没有任何任务的测试会覆盖、却最可能让使用者踩坑的输入/条件。每条都挂到拥有该代码的任务上，在该任务自己的步骤风格里加验证。

1. **feature 门控代码在构建期被删、在运行期会执行。** Bun 1.3.14 原生实现 `bun:bundle` 的 `feature()` 并在构建期做 DCE：bunfig.toml 未定义的旗标返回 `false`，整支代码被删除（所以 Phase 1 的 `bun build` 对 `require('./commands/workflows/index.js')` 这类缺失模块不报错）；而 `bun run` 运行时用的是我们的 shim `src/types/bun-bundle.js`，恒返回 `true`。本计划新建的桩文件在构建期可能被 DCE 掉、在运行期却会被真实走到。→ 验证挂在 Task 4/8：每个新建桩文件执行 `bun -e "import * as m from '<路径>'; console.log(Object.keys(m))"`，确认导出名与 tsc 看到的声明一致（不是 `{}`）。
2. **19 个泄露来的 `.js` 文件（18 个 `src/commands/*/index.js` + `src/services/contextCollapse/index.js`）在 `noImplicitAny: false` 下整体是 `any`，tsc 看不见它们的形状缺口。** 已知实例：`src/services/contextCollapse/index.js` 只导出 `getStats`/`subscribe`/`isContextCollapseEnabled`，但 `src/query.ts`、`src/screens/REPL.tsx`、`src/setup.ts`、`src/services/compact/postCompactCleanup.ts` 还用了 `applyCollapsesIfNeeded`/`isWithheldPromptTooLong`/`recoverFromOverflow`/`resetContextCollapse`/`initContextCollapse`——tsc 不报，运行期会是 `undefined is not a function`。→ 验证挂在 Task 9：`bun -e` 逐个 require 这 19 个 `.js` 文件，把"被引用但未导出"的名字补进桩（或记录待 Phase 6）。
3. **`unknown` 型数据流入副作用位置。** 残余错误里 `unknown` 是最大单一来源（TS2339/TS2322/TS2345/TS2538 大量出现），典型如 `src/cli/structuredIO.ts:355` 把 `Object.entries(message.variables)` 的 `unknown` 值写进 `process.env[key]`。压制时若把目标类型改宽，会把运行期才炸的类型错误提前掩盖。→ 验证挂在 Task 11：每个 `unknown` 修复只允许"在 unknown 的产生点补类型/铸型"，不允许改宽消费端签名；改完后 `bun build` + 该文件所在目录无新增错误。
4. **构建变体字面量归一化不得改变运行期行为。** 95 处 `"external" === 'ant'` 这类比较在源码里已被构建期替换成字面量，恒为假。归一化成 `false` 后行为不变；但如果误把 `"external" !== 'ant'`（恒真）写成 `false`，会静默关掉内部功能。→ 验证挂在 Task 9：`grep -rn "\"external\" === 'ant'\|\"external\" !== 'ant'" src/` 为 0，且 `git diff` 中每个 `true`/`false` 与原比较运算符一致。
5. **`@types/bun` 是全局 augment，可能改变既有文件的解析结果。** 装上它之后 `Bun.*`、`bun:ffi` 有了类型，但也可能让某些原本靠 `any` 通过的 Node/Bun 混用代码暴露新错误，或与 `@types/node@18` 冲突。→ 验证挂在 Task 1：装前装各跑一次全量 tsc，diff 错误集合；新增错误超过 20 条时停下报告，不要顺手改源码。

---

## 测量基线（2026-09-23，执行前实测）

```
bunx tsc --noEmit  →  1,922 errors / 580 files
```

错误码分布（executor 用它核对各任务的下降量）：

| 错误码 | 次数 | 主要根因 | 计划中的任务 |
|---|---|---|---|
| TS2307 Cannot find module | 568 | 本地缺失模块 ~95 个文件（type-only import 为主，bun build 不解析所以 Phase 1 看不见）；6 个漏录外部包；SDK 子路径 | Task 4-8 |
| TS2339 Property does not exist | 554 | 缺失类型模块导致 `never`/`unknown`/`{}` 收窄；SDK 旧版类型 | Task 2、5-7、10-11 |
| TS2305 Module has no exported member | 137 | SDK 旧版缺导出；再导出链断点（`agentSdkTypes.ts` 等） | Task 2、6 |
| TS2367 Comparison no overlap | 104 | 构建变体字面量（95 处 `"external" === 'ant'` 等） | Task 9 |
| TS2322 / TS2345 | 186 | `unknown` 传播 | Task 11 |
| TS2867 Cannot find name 'Bun' | 59 | 未装 `@types/bun` | Task 1 |
| TS2724 / TS2694 | 83 | SDK 旧版导出名不存在 | Task 2 |
| TS2550 Property 'findLast' does not exist | 41 | `lib` 低于 ES2023 | Task 1 |
| TS2304 Cannot find name | 32 | ANT-only 标识符（`GateOverridesWarning`、`resolveAntModel` 等）在 `false &&` 死分支里被引用 | Task 12 |
| TS2460 / TS2554 / TS2749 / TS2698 / TS2538 / TS2349 / TS2488 / TS2739 / TS2353 / TS2769 / TS2786 / TS2352 / TS2440 / TS2362 / TS2578 / TS5097 / TS2820 / TS2740 / TS2677 / TS2344 / TS2306 / TS1360 / TS2604 / TS2464 / TS2551 | ~170 | 混合：`unknown` 传播、桩形状缺口、`@ts-expect-error` 失效、`allowImportingTsExtensions` | Task 1、11-12 |

各配置杠杆的实测收益（Task 1-3 的预期下降量来源）：

| 杠杆 | 实测错误下降 |
|---|---|
| `lib: ES2022` → `ES2024` | 1,922 → 1,879（−43，含 `findLast`、`PromiseWithResolvers`） |
| 安装 `@types/bun@^1.4.2` | −59（TS2867 归零）+ Bun.* 级联（未测，安装被权限拦截） |
| `@anthropic-ai/sdk` 0.27.3 → 0.127.0 | −105 直接（TS2305 16 / TS2307 33 / TS2694 20 / TS2724 33 / TS2345 3） |

**关于 `bun build` 为何对缺失模块不报错的机制记录**（影响本计划全部桩任务，executor 必须理解）：`bun build` 只打包可达图，且 Bun 1.3.14 原生实现 `bun:bundle` 的 `feature()` 构建期 DCE——bunfig.toml 没定义的旗标返回 `false`，`require('./commands/workflows/index.js')` 整支被删除，永不解析。`tsc` 不认识 `feature()` 的 DCE（我们的 `bun-bundle.d.ts` 声明它返回 `boolean`），两个分支都做类型检查。**所以"build 通过"不代表"模块存在"**，Phase 2 的桩覆盖必须以 tsc 输出为准。

---

### Task 1：tsconfig `lib` 升至 ES2024 + 安装 `@types/bun`

**文件：**
- 修改：`tsconfig.json`（`lib`、`allowImportingTsExtensions`）
- 修改：`package.json`、`bun.lock`（devDependencies 增加 `@types/bun`）

**接口：**
- 输入：无（第一个任务）
- 输出：`lib: ["ES2024"]`、`allowImportingTsExtensions: true` 的 tsconfig；`@types/bun` 进入 devDependencies

**背景：** ① `lib: ES2022` 导致 41 处 `findLast`、1 处 `PromiseWithResolvers` 报错；② `Bun` 全局未声明导致 59 处 TS2867，`bun:ffi` 也无法解析；③ Phase 1 Task 6 把 `cli.tsx` 的 import 改成 `'../main.tsx'` 后引入了 TS5097（`allowImportingTsExtensions` 未开）。

- [ ] **步骤 1：修改 tsconfig.json**

把 `compilerOptions` 里这两行：

```json
    "lib": ["ES2022"],
```

改为：

```json
    "lib": ["ES2024"],
    "allowImportingTsExtensions": true,
```

- [ ] **步骤 2：验证 lib 升级生效**

执行：`bunx tsc --noEmit 2>&1 | grep -c "error TS"`
期望：`1879`（基线 1,922，−43）

- [ ] **步骤 3：安装 `@types/bun`**

执行：`bun add -d @types/bun@^1.4.2`
期望：安装成功，`package.json` devDependencies 出现 `"@types/bun": "^1.4.2"`

- [ ] **步骤 4：验证 Bun 类型生效**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -c "error TS2867"
bunx tsc --noEmit 2>&1 | grep -c "bun:ffi"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条 `0`，第二条 `0`，第三条 < 1879 且相对步骤 2 的**新增错误不超过 20 条**（超过就停下报告，见 Review Focus 第 5 条）

- [ ] **步骤 5：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0，生成 `dist/cli.js`

- [ ] **步骤 6：提交**

```bash
git add tsconfig.json package.json bun.lock
git commit -m "build(phase2): lib 升至 ES2024，安装 @types/bun，开启 allowImportingTsExtensions"
```

---

### Task 2：升级 `@anthropic-ai/sdk` 至与源码匹配的版本

**文件：**
- 修改：`package.json`、`bun.lock`

**接口：**
- 输入：无
- 输出：`@anthropic-ai/sdk` 升级到 npm latest（2026-09-23 为 0.127.0）；SDK 相关 TS 错误归零

**背景：** Phase 1 把 SDK 固定在 `^0.27.0`（计划原文随手写的版本）。源码实际针对新得多的 SDK：`import ... from '@anthropic-ai/sdk/resources/beta/messages/messages.mjs'` 有 22 处，而 0.27.3 的 exports 里**没有** `./resources/*` 子路径；另有 `ContentBlockParam`、`Base64ImageSource`、`StreamEvent` 等导出名只在新版存在。这直接造成 105 处错误（TS2305 16 / TS2307 33 / TS2694 20 / TS2724 33 / TS2345 3）。已实测 latest（0.127.0）的 exports 含 `"./resources/*": { "import": "./resources/*.mjs" }`，且随包发布 `.d.mts`。

- [ ] **步骤 1：记录升级前版本**

执行：`grep '"@anthropic-ai/sdk"' package.json`
期望：`"@anthropic-ai/sdk": "^0.27.0"`

- [ ] **步骤 2：升级**

执行：`bun add @anthropic-ai/sdk@latest`
期望：`package.json` 中版本变为 `^0.127.0`（或当时 latest）

- [ ] **步骤 3：验证 SDK 错误归零**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -c "@anthropic-ai/sdk"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条 `0`；第二条 ≤ 步骤 1 前的总数（Task 1 后的数）− 100 左右

- [ ] **步骤 4：运行时冒烟**

执行：`bun -e "import { Anthropic } from '@anthropic-ai/sdk'; console.log('sdk OK', typeof Anthropic)"`
期望：`sdk OK function`

- [ ] **步骤 5：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 6：修订 spec**

按 spec §0 在 §9.2 的 `@anthropic-ai/sdk` 行补版本说明，并在 §0 修订记录表加一行：

```markdown
| 2026-09-23 | §9.2 | `@anthropic-ai/sdk` 版本约束 `^0.27.0` → `^0.127.0`——Phase 1 的版本号是计划里随手写的，源码 22 处 import `@anthropic-ai/sdk/resources/beta/messages/messages.mjs` 需要含 `./resources/*` 子路径的新版 exports，旧版直接造成 105 处类型错误 |
```

- [ ] **步骤 7：提交**

```bash
git add package.json bun.lock docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md
git commit -m "build(phase2): 升级 @anthropic-ai/sdk 至 0.127.x，消除 105 处 SDK 类型错误"
```

---

### Task 3：补录并处理 §9.2 遗漏的 6 个外部包 + `vscode-jsonrpc/node.js` 子路径

**文件：**
- 修改：`package.json`、`bun.lock`
- 修改：`tsconfig.json`（`paths` 增加 1 条）
- 修改：`src/types/missing-modules.d.ts`（仅当某包装不上）

**接口：**
- 输入：下方表格（2026-09-23 已用 `npm view <pkg> version` 逐个确认可安装）
- 输出：外部包 TS2307 清零

**背景：** spec §9.2 的 66 包清单漏了 6 个真实被 import 的包。已实测全部可在公开 npm 安装（`cli-highlight` 2.1.11、`plist` 5.0.0、`cacache` 21.0.1、`image-processor-napi` 0.0.1、`audio-capture-napi` 0.0.1、`url-handler-napi` 0.0.1）。另有 `vscode-jsonrpc/node.js`：已装 9.0.2 的 exports 只有 `"./node"` 没有 `"./node.js"`，而 bun build 能容忍该写法（Phase 1 已通过），只有 tsc 报错。

| 包 | 引用点 | 处理 |
|---|---|---|
| `cli-highlight` | `src/utils/cliHighlight.ts:11-12`（`typeof import('cli-highlight')`） | `bun add` |
| `plist` | `src/services/notifier.ts:138`（动态 import） | `bun add` |
| `cacache` | `src/utils/cleanup.ts:466`（动态 import） | `bun add` |
| `image-processor-napi` | `src/tools/FileReadTool/imageProcessor.ts:46`、`src/utils/imagePaste.ts:108,137`（动态 import） | `bun add` |
| `audio-capture-napi` | `src/services/voice.ts:20,27`（`typeof import` + 动态 import） | `bun add` |
| `url-handler-napi` | `src/utils/deepLink/protocolHandler.ts:95`（动态 import） | `bun add` |
| `vscode-jsonrpc/node.js` | `src/services/lsp/LSPClient.ts:8` | tsconfig paths 映射 |

- [ ] **步骤 1：安装 6 个包**

执行：
```bash
bun add cli-highlight plist cacache image-processor-napi audio-capture-napi url-handler-napi
```
期望：安装成功，`package.json` dependencies 增加 6 项

- [ ] **步骤 2：为 `vscode-jsonrpc/node.js` 加 paths 映射**

在 `tsconfig.json` 的 `paths` 里追加一条（放在 `"src/*"` 之后、`"bun:bundle"` 之前）：

```json
      "vscode-jsonrpc/node.js": ["./node_modules/vscode-jsonrpc/lib/node/main.d.ts", "./node_modules/vscode-jsonrpc/lib/node/main.js"],
```

- [ ] **步骤 3：验证外部包错误归零**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep "TS2307" | grep -v "^\S*: Cannot find module '[./]" | grep -v "src/"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条无输出（所有 TS2307 的 specifier 都已是本地相对路径或 `src/` 开头）；第二条较 Task 2 后再降

- [ ] **步骤 4：原生模块运行时冒烟（记录能力，不要求成功）**

执行：
```bash
bun -e "await import('image-processor-napi').then(m => console.log('image OK')).catch(e => console.log('image FAIL:', e.message.slice(0,80)))"
bun -e "await import('audio-capture-napi').then(m => console.log('audio OK')).catch(e => console.log('audio FAIL:', e.message.slice(0,80)))"
bun -e "await import('url-handler-napi').then(m => console.log('url OK')).catch(e => console.log('url FAIL:', e.message.slice(0,80)))"
```
期望：三种结果都允许。**FAIL 不要回滚安装**（本阶段只要类型），但要在 spec §9.4 加一节记录"装得上但 Windows 运行时加载失败的原生模块"。

- [ ] **步骤 5：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 6：修订 spec**

① §9.2 的无作用域包清单里补入 6 个包；② §0 修订记录表加一行：

```markdown
| 2026-09-23 | §9.2 | 66 个 → 72 个外部依赖——Task 3 实测 `cli-highlight`/`plist`/`cacache`/`image-processor-napi`/`audio-capture-napi`/`url-handler-napi` 6 个包被源码引用但清单漏录（`npm view` 确认全部可安装），另记录 `vscode-jsonrpc/node.js` 子路径在 9.x exports 中不存在、用 tsconfig paths 指向 `lib/node/main` |
```

- [ ] **步骤 7：提交**

```bash
git add package.json bun.lock tsconfig.json docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md
git commit -m "build(phase2): 补录并安装 6 个遗漏外部包，修复 vscode-jsonrpc 子路径解析"
```

---

### Task 4：编写缺失模块声明生成脚本并产出全部草稿

**文件：**
- 新建：`scripts/phase2/gen_missing_module_stubs.py`
- 新建：~95 个声明草稿文件（下表分组；`src/types/message.ts`、`src/types/tools.ts` 等）
- 修改：`src/types/missing-modules.d.ts`（不加本地模块声明——草稿是真实文件）

**接口：**
- 输入：`tsc --noEmit` 输出的 TS2307 行
- 输出：每个缺失目标路径一个 `<name>.ts` 草稿文件；`scripts/phase2/gen_missing_module_stubs.py` 可重复执行

**背景：** 186 个唯一缺失 specifier 解析到 ~95 个目标文件（相对 specifier 按引用点目录解析，`src/...` 别名按仓库根解析）。最高频的 15 个：

| 目标文件 | 引用点数 |
|---|---|
| `src/types/message.ts` | 183 |
| `src/constants/querySource.ts` | 21 |
| `src/types/tools.ts` | 19 |
| `src/entrypoints/sdk/controlTypes.ts` | 19 |
| `src/keybindings/types.ts` | 17 |
| `src/types/utils.ts` | 15 |
| `src/components/agents/new-agent-creation/types.ts` | 13 |
| `src/proactive/index.ts` | 10 |
| `src/components/mcp/types.ts` | 10 |
| `src/cli/handlers/ant.ts` | 9 |
| `src/services/oauth/types.ts` | 9 |
| `src/components/Spinner/types.ts` | 8 |
| `src/components/FeedbackSurvey/utils.ts` | 8 |
| `src/commands/plugin/types.ts` | 6 |
| `src/utils/secureStorage/types.ts` | 6 |

其余 ~80 个文件各 1-5 个引用点，另有 20 个 `src/skills/bundled/claude-api/**/*.md` 文本导入目标。

**已实测的声明文件写法**（tsc 与 bun build 均通过，运行期导出 `undefined`）：

```ts
export type Foo = any
export const bar: any = undefined as any
export function baz(...args: any[]): any { return undefined }
export default undefined as any
```

- [ ] **步骤 1：创建脚本**

创建 `scripts/phase2/gen_missing_module_stubs.py`：

```python
#!/usr/bin/env python3
"""Phase 2 helper: generate first-draft declaration files for the local
modules that `tsc --noEmit` reports as TS2307 (Cannot find module).

Usage:
    bunx tsc --noEmit 2>&1 | grep "error TS2307" > .tmp/ts2307.txt
    python scripts/phase2/gen_missing_module_stubs.py .tmp/ts2307.txt

Each generated file carries a `// DRAFT` marker. Review every file before
committing: the draft types everything as `any`, which silences TS2307 but
leaves `never`/`unknown` narrowing errors in place for the refine tasks.
"""
import collections
import os
import re
import sys

ROOT = os.getcwd()
SKIP_DIRS = {'node_modules', 'dist', '.git', '.tmp', 'scripts', 'docs'}


def iter_ts_files():
    for dp, dn, fn in os.walk(os.path.join(ROOT, 'src')):
        dn[:] = [d for d in dn if d not in SKIP_DIRS]
        for f in fn:
            if f.endswith('.ts') or f.endswith('.tsx'):
                yield os.path.join(dp, f).replace(os.sep, '/')


def parse_ts2307(path):
    """-> list of (importing_file, specifier)"""
    out = []
    rx = re.compile(r"^(\S+?)\((\d+),\d+\): error TS2307: Cannot find module '([^']+)'")
    for line in open(path, encoding='utf-8'):
        m = rx.match(line.strip())
        if m:
            out.append((m.group(1).replace('\\', '/'), m.group(3)))
    return out


def resolve_target(importing_file, spec):
    """-> repo-relative target path, or None for external packages."""
    if spec.endswith('.md'):
        ext = '.md'
    elif '.js' in os.path.basename(spec):
        ext = '.ts'
    else:
        return None
    if spec.startswith('src/'):
        base = spec
    elif spec.startswith('.'):
        base = os.path.normpath(os.path.join(os.path.dirname(importing_file), spec))
    else:
        return None  # external package
    base = base.replace(os.sep, '/')
    if base.endswith('.js'):
        base = base[:-3]
    if base.endswith('.tsx'):
        return base
    if ext == '.md':
        return base
    return base + '.ts'


def main(ts2307_path):
    sites = parse_ts2307(ts2307_path)
    # target -> set of imported names; target -> flags
    names = collections.defaultdict(set)
    has_default = collections.defaultdict(bool)
    tsx_targets = set()

    for importing_file, spec in sites:
        tgt = resolve_target(importing_file, spec)
        if tgt:
            tsx_targets.add(tgt)

    # collect names from every import/export-from statement whose resolved
    # target is in the missing set
    for tf in iter_ts_files():
        try:
            src = open(tf, encoding='utf-8').read()
        except Exception:
            continue
        for fm in re.finditer(r"from\s+['\"]([^'\"]+)['\"]", src):
            spec = fm.group(1)
            # find the statement start (import/export keyword) before this from
            head = src[:fm.start()]
            kw = None
            for m in re.finditer(r'\b(import|export)\b', head):
                kw = m
            if kw is None:
                continue
            between = head[kw.end():]
            if re.search(r'\bimport\b|\bexport\b|;', between):
                continue
            if between.count('{') > 1 or between.count('}') > 1:
                continue
            # resolve against BOTH the importing file and the src/ alias
            cands = []
            if spec.startswith('.'):
                cands.append(os.path.normpath(os.path.join(os.path.dirname(tf), spec)).replace(os.sep, '/'))
            elif spec.startswith('src/'):
                cands.append(spec)
            for c in cands:
                if c.endswith('.js'):
                    c = c[:-3]
                for probe in (c + '.ts', c + '.tsx', c):
                    if probe in tsx_targets:
                        tgt = probe
                        break
                else:
                    continue
                break
            else:
                continue
            clause = between.strip()
            clause = re.sub(r'^type\s+', '', clause)
            if clause.startswith('*'):
                continue
            brace = clause.find('{')
            head_part = clause[:brace] if brace >= 0 else clause
            head_part = head_part.strip().rstrip(',').strip()
            if head_part and re.match(r'^[A-Za-z_$][\w$]*$', head_part):
                has_default[tgt] = True
            if brace >= 0:
                inner = clause[brace + 1:clause.rfind('}')]
                for part in inner.split(','):
                    part = part.strip()
                    if not part:
                        continue
                    nm = part.split(' as ')[0].strip()
                    if nm.startswith('type '):
                        nm = nm[5:].strip()
                    if nm:
                        names[tgt].add(nm)

    # generic detection: Name< used anywhere in src
    allsrc = {}
    for tf in iter_ts_files():
        try:
            allsrc[tf] = open(tf, encoding='utf-8').read()
        except Exception:
            pass

    created = []
    for tgt in sorted(tsx_targets):
        rel = os.path.relpath(tgt, ROOT).replace(os.sep, '/')
        if os.path.exists(tgt):
            continue
        os.makedirs(os.path.dirname(tgt), exist_ok=True)
        if tgt.endswith('.md'):
            title = os.path.basename(tgt)
            with open(tgt, 'w', encoding='utf-8') as f:
                f.write('<!-- TODO: Phase 6 - 还原 %s 真实内容（Phase 2 建桩） -->\n\n# %s\n\nSTUB\n' % (rel, title))
            created.append(rel)
            continue
        lines = [
            '// TODO: Phase 6 - 还原 %s 真实实现' % rel,
            '// DRAFT: generated by scripts/phase2/gen_missing_module_stubs.py — review before commit',
        ]
        for nm in sorted(names.get(tgt, ())):
            generic = any(re.search(r'\b' + re.escape(nm) + r'\s*<', s) for s in allsrc.values())
            if generic:
                lines.append('export type %s<T = any> = any' % nm)
            else:
                lines.append('export type %s = any' % nm)
                lines.append('export const %s: any = undefined as any' % nm)
        if has_default.get(tgt):
            lines.append('export default undefined as any')
        if len(lines) == 2:
            lines.append('export default undefined as any')
        with open(tgt, 'w', encoding='utf-8') as f:
            f.write('\n'.join(lines) + '\n')
        created.append(rel)

    print('created %d files' % len(created))
    for c in created:
        print('  ' + c)


if __name__ == '__main__':
    main(sys.argv[1])
```

- [ ] **步骤 2：生成草稿**

先确保 `.tmp/` 不进版本库——在 `.gitignore` 末尾加一行 `.tmp/`：

```bash
printf '.tmp/\n' >> .gitignore
```

执行：
```bash
mkdir -p .tmp
bunx tsc --noEmit 2>&1 | grep "error TS2307" > .tmp/ts2307.txt
python scripts/phase2/gen_missing_module_stubs.py .tmp/ts2307.txt
```
期望：打印 `created N files`，N ≈ 90-100（含 20 个 `.md`）；`src/types/message.ts` 在列

- [ ] **步骤 3：验证 TS2307 塌落**

执行：`bunx tsc --noEmit 2>&1 | grep -c "error TS2307"`
期望：从 568 降到 < 100（剩余主要是外部包与 SDK 相关，Task 2-3 已处理的话应接近 0）

- [ ] **步骤 4：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0（新增真实文件后 bun 会打包它们；若报错说明某个草稿文件的语法或默认导出形式有问题，按报错修）

- [ ] **步骤 5：Review Focus 第 1 条验证（运行期导出名一致）**

执行：
```bash
for f in src/types/message.ts src/types/tools.ts src/types/utils.ts src/constants/querySource.ts; do
  echo "== $f"; bun -e "import * as m from './$f'; console.log(Object.keys(m).slice(0,8))"
done
```
期望：每个文件打印出非空导出名列表（`export type` 在运行期不存在，只应看到 `export const`/`export default` 的名字）；若某个文件键数为 0，检查它是否只有 type 导出——那是允许的，但要在 Task 5-8 精修时确认引用点都是 `import type`

- [ ] **步骤 6：提交**

```bash
git add .gitignore scripts/phase2/gen_missing_module_stubs.py src/
git commit -m "feat(phase2): 为缺失本地模块生成声明草稿（TS2307 568→<N>）"
```

---

### Task 5：精修 `src/types/message.ts`（183 个引用点）

**文件：**
- 修改：`src/types/message.ts`（Task 4 草稿）

**接口：**
- 输入：Task 4 草稿
- 输出：`Message` 及消息联合类型；下游 TS2339/TS2322 显著下降
- 消费方示例签名（executor 按此校验形状）：
  - `src/utils/sessionStorage.ts:58` `import type { AssistantMessage, AttachmentMessage, Message, ... } from '../types/message.js'`
  - `src/utils/messages.ts`、`src/services/api/claude.ts` 等 183 处

**背景：** 这是全仓库最高频的缺失模块。草稿的 `any` 能消掉 TS2307，但消费方大量按 `.type` 判别联合成员（`entry.type === 'summary'` 等），`any` 会让判别通过却掩盖形状；反之若联合成员不全会把 `entry` 收窄成 `never`（`src/utils/sessionStorage.ts:3658` 一带有 91 处 TS2339-on-never 就是这个原因）。

- [ ] **步骤 1：收集全部被引用的名字**

执行：`grep -rhoE "from ['\"](\.\./)*types/message\.js['\"]" src/ | wc -l`，再对每个引用点执行 `grep -B8 "types/message.js" <文件>` 收集名字。或用：

```bash
grep -rn -B12 "types/message.js'" src/ --include=*.ts --include=*.tsx | grep -oE "\b[A-Z][A-Za-z0-9_]+\b" | sort | uniq -c | sort -rn | head -40
```

- [ ] **步骤 2：重建 `Message` 联合**

按消费方的判别字段重建。最小可用形状（保持 `any` 成员以避免误伤，但联合判别必须成立）：

```ts
// TODO: Phase 6 - 还原 src/types/message.ts 真实实现
// DRAFT refined: 判别联合按 sessionStorage/messages 的 entry.type 分支重建

export type ProgressMessage = {
  type: 'progress'
  uuid: string
  parentUuid: string | null
  sessionId: string
  [key: string]: any
}

export type SummaryMessage = {
  type: 'summary'
  leafUuid: string
  summary: string
  [key: string]: any
}

export type CustomTitleMessage = {
  type: 'custom-title'
  sessionId: string
  customTitle: string
  [key: string]: any
}

export type TagMessage = {
  type: 'tag'
  sessionId: string
  tag: string
  [key: string]: any
}

export type AgentNameMessage = {
  type: 'agent-name'
  sessionId: string
  agentName: string
  [key: string]: any
}

export type UserMessage = {
  type: 'user'
  uuid: string
  parentUuid: string | null
  sessionId: string
  message: any
  [key: string]: any
}

export type AssistantMessage = {
  type: 'assistant'
  uuid: string
  parentUuid: string | null
  sessionId: string
  message: any
  [key: string]: any
}

export type AttachmentMessage = {
  type: 'attachment'
  uuid: string
  parentUuid: string | null
  sessionId: string
  attachment: any
  [key: string]: any
}

export type Message =
  | UserMessage
  | AssistantMessage
  | AttachmentMessage
  | SummaryMessage
  | CustomTitleMessage
  | TagMessage
  | AgentNameMessage
  | ProgressMessage
  | { type: string; [key: string]: any }
```

**规则：** ① 每个联合成员必须有判别字段 `type`，且字面量值与消费方的比较值一致（从 `=== 'xxx'` 反推）；② 联合最后必须保留 `{ type: string; [key: string]: any }` 兜底成员，防止未列出的 `type` 把变量收窄成 `never`；③ 其余被引用的名字（`NormalizedUserMessage`、`RenderableMessage` 等）保持 `export type X = any` + `export const X: any = undefined as any` 双形式。

- [ ] **步骤 3：逐个补齐其余导出名**

对步骤 1 收集到的每个名字，确保文件里有对应导出（草稿已生成大部分；补齐漏网的）。

- [ ] **步骤 4：验证**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -c "types/message"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
bunx tsc --noEmit 2>&1 | grep -c "on type 'never'"
```
期望：第一条 `0`；第二条较 Task 4 后明显下降；第三条较基线（91）显著下降

- [ ] **步骤 5：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 6：提交**

```bash
git add src/types/message.ts
git commit -m "feat(phase2): 重建 src/types/message.ts 判别联合（183 个引用点）"
```

---

### Task 6：精修 `src/types/` 其余模块 + `src/entrypoints/sdk/` 再导出链

**文件：**
- 修改：`src/types/tools.ts`、`src/types/utils.ts`、`src/types/messageQueueTypes.ts`、`src/types/notebook.ts`、`src/types/statusLine.ts`、`src/types/fileSuggestion.ts`、`src/utils/secureStorage/types.ts`、`src/entrypoints/sdk/controlTypes.ts`、`src/entrypoints/sdk/sdkUtilityTypes.ts`、`src/entrypoints/sdk/settingsTypes.generated.ts`
- 修改：`src/entrypoints/agentSdkTypes.ts`（补再导出行）

**接口：**
- 输入：Task 4 草稿
- 输出：`DeepImmutable<T>` 等泛型类型可用；`agentSdkTypes.ts` 导出链完整
- 已知必须项：
  - `src/types/utils.ts` 必须导出泛型 `DeepImmutable<T>`（`src/components/tasks/BackgroundTask.tsx:14` 用 `DeepImmutable<BackgroundTaskState>`；草稿若非泛型会报 TS2315 "Type 'DeepImmutable' is not generic"）
  - `src/entrypoints/agentSdkTypes.ts` 必须能导出 `ModelUsage`、`SDKStatus`、`ModelInfo`、`SDKUserMessageReplay`、`PermissionResult`、`McpServerConfigForProcessTransport`、`McpServerStatus`、`RewindFilesResult`、`HookInput`、`HookJSONOutput`、`PermissionUpdate`（`src/cli/print.ts:111-123`、`src/cli/structuredIO.ts:9-11`、`src/bootstrap/state.ts:10` 等在 import）

**背景：** `agentSdkTypes.ts` 是存在的真实文件，靠 `export * from './sdk/coreTypes.js'` 等四条再导出链聚合类型；其中两条指向缺失模块（`controlTypes.js`、`settingsTypes.generated.js`）。上游缺失导致一批 TS2305/TS2460（`declares 'SDKMessage' locally, but it is exported as 'Settings'`）。

- [ ] **步骤 1：修泛型**

检查每个草稿文件里被 `Name<...>` 使用的类型名，改成泛型形式。执行：

```bash
bunx tsc --noEmit 2>&1 | grep "TS2315"
```
期望输出为空后继续（`DeepImmutable` 是主要实例，草稿的泛型探测已自动处理，此步是兜底核对）

- [ ] **步骤 2：补 `agentSdkTypes.ts` 再导出链**

在 `src/entrypoints/agentSdkTypes.ts` 现有的 `export type { SDKControlRequest, SDKControlResponse } from './sdk/controlTypes.js'` 之后，加入步骤"接口"里列出的缺失名字。改法（把现有 3 行替换为）：

```ts
// Control protocol types for SDK builders (bridge subpath consumers)
/** @alpha */
export type {
  SDKControlRequest,
  SDKControlResponse,
  // TODO: Phase 6 - 以下名字在泄露源码里没有导出来源，按消费方 import 补录
  ModelUsage,
  SDKStatus,
  ModelInfo,
  SDKUserMessageReplay,
  PermissionResult,
  McpServerConfigForProcessTransport,
  McpServerStatus,
  RewindFilesResult,
  HookInput,
  HookJSONOutput,
  PermissionUpdate,
} from './sdk/controlTypes.js'
```

并在 `src/entrypoints/sdk/controlTypes.ts` 草稿里为这些名字各加一行 `export type <Name> = any`（草稿生成器只收集了 import 点的名字，漏了再导出点消费方，需手工补）。

- [ ] **步骤 3：验证**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -cE "agentSdkTypes|TS2460|TS2315"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条 `0`；第二条继续下降

- [ ] **步骤 4：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 5：提交**

```bash
git add src/types/ src/utils/secureStorage/types.ts src/entrypoints/
git commit -m "feat(phase2): 精修 types 与 entrypoints/sdk 声明，补齐再导出链"
```

---

### Task 7：精修中流量集群（6-21 个引用点的 13 个模块）

**文件：**
- 修改：`src/constants/querySource.ts`、`src/keybindings/types.ts`、`src/components/agents/new-agent-creation/types.ts`、`src/proactive/index.ts`、`src/components/mcp/types.ts`、`src/cli/handlers/ant.ts`、`src/services/oauth/types.ts`、`src/components/Spinner/types.ts`、`src/components/FeedbackSurvey/utils.ts`、`src/commands/plugin/types.ts`

**接口：**
- 输入：Task 4 草稿
- 输出：这些模块的导出名与消费方用法对齐

**背景：** 这批模块的消费方有具体形状要求，纯 `any` 会留下下游错误。已知两个必须精确的点：

1. `src/constants/querySource.ts`（21 个引用点）——消费方拿它做查询来源判别。按引用点反推导出，例如 `src/constants/querySource.ts` 至少导出：

```ts
// TODO: Phase 6 - 还原 src/constants/querySource.ts 真实实现
export type QuerySource = 'ant' | 'external' | 'internal' | string
export const QUERY_SOURCE: QuerySource = 'external'
```

（`QuerySource` 保留 `| string` 兜底，避免 `"external" === 'ant'` 之外的判别被收窄成 `never`；Task 9 会处理那 95 处比较本身。）

2. `src/services/oauth/types.ts`（9 个引用点）——`src/cli/handlers/auth.ts:162,212` 访问 `OrgValidationResult.message`，所以该类型必须有 `message` 字段：

```ts
export type OrgValidationResult = {
  valid: boolean
  message?: string
  orgId?: string
  [key: string]: any
}
```

- [ ] **步骤 1：逐个模块列消费方属性访问**

对每个目标文件执行：`grep -rn -A3 "from '<相对路径>'" src/ --include=*.ts --include=*.tsx | head -40`，或用脚本思路：对该模块每个导入名，grep 出所有使用点，收集 `.属性` 访问。

- [ ] **步骤 2：按访问点补字段**

在草稿基础上，为每个被访问属性补字段（值类型不确定就用 `any`，不要用 `unknown`——见全局约束与 Review Focus 第 3 条）。

- [ ] **步骤 3：验证**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -E "querySource|keybindings/types|new-agent-creation|proactive|components/mcp/types|cli/handlers/ant|oauth/types|Spinner/types|FeedbackSurvey/utils|plugin/types" | wc -l
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条较 Task 6 后下降；第二条继续下降

- [ ] **步骤 4：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 5：提交**

```bash
git add src/constants/querySource.ts src/keybindings/types.ts src/components/ src/proactive/ src/cli/handlers/ant.ts src/services/oauth/types.ts src/commands/plugin/types.ts
git commit -m "feat(phase2): 精修 13 个中流量缺失模块的声明形状"
```

---

### Task 8：长尾模块 + 20 个 claude-api `.md` 文件

**文件：**
- 修改：Task 4 生成的其余 ~60 个草稿文件（各 1-5 个引用点），包括：
  `src/assistant/{index,gate,sessionDiscovery}.ts`、`src/bridge/{peerSessions,webhookSanitizer}.ts`、`src/cli/{up,rollback}.ts`、`src/cli/transports/Transport.ts`、`src/commands/{buddy,fork,peers,workflows}/index.ts`、`src/commands/install-github-app/types.ts`、`src/components/{FeedbackSurvey/useFrustrationDetection,messages/SnipBoundaryMessage,messages/UserCrossSessionMessage,messages/UserForkBoilerplateMessage,messages/UserGitHubWebhookMessage,permissions/MonitorPermissionRequest/MonitorPermissionRequest,permissions/ReviewArtifactPermissionRequest/ReviewArtifactPermissionRequest,tasks/MonitorMcpDetailDialog,tasks/WorkflowDetailDialog,ui/option,wizard/types}.ts`、`src/coordinator/workerAgent.ts`、`src/hooks/notifs/useAntOrgWarningNotification.ts`、`src/ink/{cursor,events/paste-event,events/resize-event}.ts`、`src/jobs/classifier.ts`、`src/memdir/memoryShapeTelemetry.ts`、`src/query/transitions.ts`、`src/server/**`（8 个）、`src/services/compact/{cachedMCConfig,reactiveCompact,snipProjection}.ts`、`src/services/contextCollapse/{operations,persist}.ts`、`src/services/lsp/types.ts`、`src/services/sessionTranscript/sessionTranscript.ts`、`src/services/skillSearch/**`（6 个）、`src/services/tips/types.ts`、`src/skills/mcpSkills.ts`、`src/ssh/**`（2 个）、`src/tasks/{LocalWorkflowTask/LocalWorkflowTask,MonitorMcpTask/MonitorMcpTask}.ts`、`src/tools/**`（9 个）、`src/utils/**`（11 个）
- 新建：20 个 `src/skills/bundled/claude-api/**/*.md`（Task 4 脚本已建最小桩）

**接口：**
- 输入：Task 4 草稿 + 当前 tsc 输出
- 输出：本地模块 TS2307 归零

- [ ] **步骤 1：核对草稿覆盖**

执行：`bunx tsc --noEmit 2>&1 | grep "TS2307"`
期望：只剩 `@ant/*`（已有 shim，不应出现）或确认为外部包的条目。凡是"本地路径"的 TS2307，说明草稿没覆盖到——回到 Task 4 步骤 2 重跑脚本，或手工补文件。

- [ ] **步骤 2：修 `require(...) as typeof import(...)` 形式的引用点**

`src/commands.ts` 等处有 `require('./commands/workflows/index.js') as typeof import('./commands/workflows/index.js')`。草稿是真实 `.ts` 文件后这类写法自动满足；但要确认 `src/commands/workflows/index.ts`、`src/commands/peers/index.ts`、`src/commands/fork/index.ts`、`src/commands/buddy/index.ts` 有 `export default`（`commands.ts` 取 `.default`）。草稿已按 default-import 探测生成；没有则手工加 `export default undefined as any`。

- [ ] **步骤 3：给 20 个 `.md` 桩补最小合法内容**

Task 4 脚本生成的是 `# <title>\n\nSTUB`。确认 `src/skills/bundled/claudeApiContent.ts` 的 20 个默认导入都能解析：执行 `bunx tsc --noEmit 2>&1 | grep -c "claude-api"`，期望 `0`。

- [ ] **步骤 4：验证 TS2307 归零**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -c "TS2307"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条 `0`；第二条较 Task 7 后继续下降

- [ ] **步骤 5：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0。**注意：** 这一步之后 bun 会把 `claudeApiContent.ts` 所在链（若可达）和所有新草稿打进 bundle，bundle 体积会上升；只要退出码 0 就通过。

- [ ] **步骤 6：Review Focus 第 1 条全量验证**

执行：
```bash
bun -e "
const fs = require('fs');
const files = fs.readdirSync('src', {recursive: true}).filter(f => String(f).endsWith('.ts'));
console.log('ts files:', files.length);
" 2>/dev/null || bun -e "console.log('skip')"
```
再对 Task 4 生成的全部文件抽样执行 `bun -e "import * as m from './<path>'; console.log('<path>', Object.keys(m).length)"`，确认无运行期抛错（语法错误会在这里暴露）。

- [ ] **步骤 7：提交**

```bash
git add src/
git commit -m "feat(phase2): 补齐长尾缺失模块声明与 claude-api md 桩（TS2307 归零）"
```

---

### Task 9：归一化 95 处构建变体字面量比较

**文件：**
- 修改：含 `"external" === 'ant'` / `"external" !== 'ant'` / `"production" === 'test'` 等模式的 ~30 个源文件

**接口：**
- 输入：当前 tsc 输出中 104 处 TS2367
- 输出：TS2367 归零；运行期行为不变

**背景：** 泄露源码里构建期常量替换已固化：真实项目的构建变体比较被烘成了字面量。`"external" === 'ant'` 恒为 `false`，`"external" !== 'ant'` 恒为 `true`。实测分布：`"external" === 'ant'` 89 处、`"external" !== 'ant'` 7 处、`"production" === 'test'` 4 处、`"production" === 'development'` 4 处、`"production" !== 'test'` 1 处。tsc 对恒假/恒真比较报 TS2367。

- [ ] **步骤 1：全量定位**

执行：
```bash
grep -rn "\"external\" === 'ant'" src/ --include=*.ts --include=*.tsx | wc -l
grep -rn "\"external\" !== 'ant'" src/ --include=*.ts --include=*.tsx | wc -l
grep -rn "\"production\" === 'test'\|\"production\" === 'development'\|\"production\" !== 'test'" src/ --include=*.ts --include=*.tsx | wc -l
```
期望：89 / 7 / 9

- [ ] **步骤 2：机械替换**

按语义等价规则替换（**`===` 恒假 → `false`，`!==` 恒真 → `true`**）：

```bash
# 恒假 → false
grep -rl "\"external\" === 'ant'" src/ --include=*.ts --include=*.tsx | while read f; do
  sed -i 's/"external" === '"'"'ant'"'"'/false/g' "$f"
done
grep -rl "\"production\" === 'test'\|\"production\" === 'development'" src/ --include=*.ts --include=*.tsx | while read f; do
  sed -i "s/\"production\" === 'test'/false/g; s/\"production\" === 'development'/false/g" "$f"
done
# 恒真 → true
grep -rl "\"external\" !== 'ant'" src/ --include=*.ts --include=*.tsx | while read f; do
  sed -i 's/"external" !== '"'"'ant'"'"'/true/g' "$f"
done
grep -rl "\"production\" !== 'test'" src/ --include=*.ts --include=*.tsx | while read f; do
  sed -i "s/\"production\" !== 'test'/true/g" "$f"
done
```

在 `docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md` §0 修订记录表加一行说明这批字面量的来源与处理。

- [ ] **步骤 3：Review Focus 第 4 条验证（运算符与替换值一致）**

执行：
```bash
git diff -U0 | grep -E "^[+-].*(false|true)" | grep -vE "^[+-]{3}" | head -20
```
期望：每个 `+false` 行的原行是 `===`，每个 `+true` 行的原行是 `!==`。逐个核对，发现不一致立即 `git checkout -- <file>` 重来。

- [ ] **步骤 4：验证 TS2367 归零**

执行：`bunx tsc --noEmit 2>&1 | grep -c "TS2367"`
期望：`0`

- [ ] **步骤 5：回归门 + Review Focus 第 2 条**

执行：
```bash
bun build src/entrypoints/cli.tsx --target=bun --outdir=dist
bun -e "
const mods = ['./src/services/contextCollapse/index.js','./src/commands/env/index.js','./src/commands/ctx_viz/index.js','./src/commands/ant-trace/index.js','./src/commands/teleport/index.js','./src/commands/autofix-pr/index.js','./src/commands/backfill-sessions/index.js','./src/commands/break-cache/index.js','./src/commands/bughunter/index.js','./src/commands/debug-tool-call/index.js','./src/commands/good-claude/index.js','./src/commands/issue/index.js','./src/commands/mock-limits/index.js','./src/commands/oauth-refresh/index.js','./src/commands/onboarding/index.js','./src/commands/perf-issue/index.js','./src/commands/reset-limits/index.js','./src/commands/share/index.js','./src/commands/summary/index.js'];
for (const m of mods) { try { const x = require(m); console.log(m, '->', Object.keys(x).join(',')); } catch (e) { console.log(m, 'LOAD FAIL', e.message.slice(0,60)); } }
"
```
期望：build 退出码 0；19 个 `.js` 全部 LOAD 成功（这些是泄露来的 `.js`，`noImplicitAny: false` 下 tsc 视其为 `any`，形状缺口只能这样查）。对 `contextCollapse`：若输出只有 `getStats,subscribe,isContextCollapseEnabled`，把 `applyCollapsesIfNeeded`、`isWithheldPromptTooLong`、`recoverFromOverflow`、`resetContextCollapse`、`initContextCollapse` 5 个名字补成空实现（同步 `export function`，保持 Phase 1 Task 8d 已记录的 async 例外裁决——这 5 个是同步消费的）。

- [ ] **步骤 6：提交**

```bash
git add src/ docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md
git commit -m "refactor(phase2): 归一化 95 处构建变体字面量比较（TS2367 归零）"
```

---

### Task 10：清零 `never` 收窄类 TS2339

**文件：**
- 修改：`src/utils/sessionStorage.ts` 等出现 `on type 'never'` 的文件

**接口：**
- 输入：当前 tsc 输出中 `Property '...' does not exist on type 'never'`（基线 91 处）
- 输出：该模式归零

**背景：** 典型形态在 `src/utils/sessionStorage.ts:3658` 一带：`isLegacyProgressEntry(entry: unknown): entry is LegacyProgressEntry` 是类型谓词，实参 `entry: Entry` 与谓词类型求交得 `never`（`Entry` 联合里没有 progress 成员，源码注释自己也写明了这一点），随后 `entry.parentUuid` 等访问全部报错。

- [ ] **步骤 1：定位全部实例**

执行：`bunx tsc --noEmit 2>&1 | grep "on type 'never'" | wc -l`
期望：91

- [ ] **步骤 2：按配方修复**

配方 A（首选，最小改动）：把谓词调用的实参铸成 `unknown`，让谓词从 `unknown` 收窄而不是与实参类型求交。`src/utils/sessionStorage.ts:3631`：

```ts
      if (isLegacyProgressEntry(entry as unknown)) {
```

配方 B（当 `never` 来自联合成员缺失）：在对应的 stub 类型联合里补兜底成员（Task 5 已在 `Message` 联合末尾加了 `{ type: string; [key: string]: any }`，同样手法用于其他 stub 联合）。

配方 C（当谓词类型本身来自缺失模块）：确认该类型的 stub 已生成（Task 4-8 覆盖），`any` 谓词不会产生 `never`。

**禁止**用 `as any` 压制属性访问本身——那会同时关掉真正的拼写检查。

- [ ] **步骤 3：验证**

执行：`bunx tsc --noEmit 2>&1 | grep -c "on type 'never'"`
期望：`0`

- [ ] **步骤 4：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 5：提交**

```bash
git add src/
git commit -m "fix(phase2): 消除 never 收窄类类型错误（91 处）"
```

---

### Task 11：清零 `unknown` 传播类错误（TS2339 / TS2322 / TS2345 / TS2538 / TS2365 / TS2349 / TS2488）

**文件：**
- 修改：`src/cli/structuredIO.ts`、`src/cli/print.ts`、`src/commands/fast/fast.tsx`、`src/commands/clear/conversation.ts`、`src/commands/ide/ide.tsx`、`src/components/PromptInput/PromptInput.tsx` 等出现 `unknown` 的文件

**接口：**
- 输入：当前 tsc 输出中所有 `unknown` 相关错误
- 输出：`unknown` 相关错误归零或降到个别需记录的残留

**背景：** `unknown` 是残余错误里最大的单一来源。两种根因：① 消费端把 `unknown` 当具体类型用（如 `src/cli/structuredIO.ts:355` `process.env[key] = value`，`value` 来自 `Object.entries(message.variables)`）；② 缺失/不完整的类型让本应具体的值变成 `unknown`。

- [ ] **步骤 1：分类清单**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -E "TS2339|TS2322|TS2345|TS2538|TS2365|TS2349|TS2488" | grep "unknown" | wc -l
bunx tsc --noEmit 2>&1 | grep -E "TS2339|TS2322|TS2345|TS2538|TS2365|TS2349|TS2488" | grep -v "unknown" | wc -l
```
把两张清单分别存入 `.tmp/unknown.txt` 与 `.tmp/other.txt` 供本任务与 Task 12 使用。

- [ ] **步骤 2：按配方修复 unknown 类**

配方 A（`unknown` 来自 stub 联合/对象）：回该 stub 文件把成员类型从 `unknown` 改为 `any`（`Record<string, unknown>` → `Record<string, any>`）。这符合全局约束——stub 的目的是"让消费方的属性访问成立"，`unknown` 会让访问失败。

配方 B（`unknown` 来自 `JSON.parse`/`Object.entries` 等标准库）：在产生点铸型，不要在消费端改宽签名。例（`src/cli/structuredIO.ts:353-356`）：

```ts
        const keys = Object.keys(message.variables)
        for (const [key, value] of Object.entries(message.variables as Record<string, string>)) {
          process.env[key] = value
        }
```

配方 C（`unknown` 来自 `catch (e: unknown)` 后的属性访问）：用现有类型守卫或 `e instanceof Error` 收窄；没有守卫时 `(e as { message?: string }).message`。

**Review Focus 第 3 条约束：** 每个修复只允许改"产生点"或"读点"的局部铸型，不允许把函数签名/接口字段从具体类型改成 `unknown`/`any` 来迁消费端。

- [ ] **步骤 3：验证**

执行：
```bash
bunx tsc --noEmit 2>&1 | grep -cE "TS2339|TS2322|TS2345|TS2538|TS2365|TS2349|TS2488"
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```
期望：第一条较步骤 1 的第一项归零或接近归零；第二条继续下降

- [ ] **步骤 4：回归门**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0

- [ ] **步骤 5：提交**

```bash
git add src/
git commit -m "fix(phase2): 消除 unknown 传播类类型错误"
```

---

### Task 12：残余错误码逐类清零

**文件：**
- 视错误而定（预计集中在 `src/components/`、`src/hooks/`、`src/screens/REPL.tsx`、`src/main.tsx`、`src/utils/hooks.ts`）

**接口：**
- 输入：Task 11 之后的 tsc 全量输出
- 输出：`tsc --noEmit` 退出码 0、零 error

**各码配方**（按预期残留量排序）：

| 错误码 | 预期残留 | 配方 |
|---|---|---|
| TS2305 Module has no exported member | 少量 | 消费方 import 的名字在目标模块（真实文件）里不存在 → 若目标模块是 Task 4-8 建的 stub，把名字补进去；若是真实文件，检查是否该从另一条再导出链来（Task 6 手法） |
| TS2304 Cannot find name | ~32 | 两类：① ANT-only 标识符（`GateOverridesWarning`、`ExperimentEnrollmentNotice`、`resolveAntModel`、`getAntModelOverrideConfig`、`UltraplanLaunchDialog`、`TungstenPill`、`launchUltraplan`）——它们只出现在 Task 9 刚归一化出来的 `false &&` 死分支里。**把死分支表达式替换为 `false`，不要删整条语句**（形如 `t21 = false && <GateOverridesWarning />` → `t21 = false`；删语句会让 `t21` 从 `false` 变 `undefined`，虽同属假值但改变了求值结果）；② 真实漏声明（`apiMetricsRef` 等）——补 `const`/`declare` |
| TS2460 declares X locally, but exported as Y | 少量 | 再导出链名字冲突：把冲突名字从 stub 的 `export` 列表移除或改名，保留消费方实际 import 的那个 |
| TS2554 Expected N arguments | ~25 | 调用点参数数与签名不符：优先补 stub 签名为 `(...args: any[])`；真实文件则补默认参数 |
| TS2749 refers to a value, used as a type | ~16 | 消费端 `import { X }` 当类型用 → 在该模块 stub 里同时提供 `export type X = any`（草稿已双写，核对漏网） |
| TS2698 Spread types may only be created from object types | ~12 |  spread 源是 stub 的 `any`/非对象 → 把 stub 成员改成对象形状或 `as Record<string, any>` |
| TS2739 / TS2353 / TS2769 | ~17 | 对象字面量属性缺失/多余、重载不匹配 → 补 stub 字段（`{}` 形状的桩最常见，如 `src/components/PromptInput/Notifications.tsx:179` 要 `{ current, queue }`） |
| TS2786 cannot be used as a JSX component | 3 | Context 被当组件用（`ScrollChromeContext`、`ModalContext`）→ stub 里给 `React.createContext(...)` 形状或 `as any`，注释说明 |
| TS2578 Unused '@ts-expect-error' directive | ~11 | 指令已失效 → 删掉该行指令（先确认它压制的错误真的没了） |
| TS5097 / TS1360 / TS2820 / TS2677 / TS2344 / TS2306 / TS2604 / TS2464 / TS2440 / TS2362 / TS2551 / TS2352 | 零星 | 逐个按报错信息修；`allowImportingTsExtensions` 已在 Task 1 开启 |
| `import { use } from 'react'`（7 个文件，React 18 无此导出） | ~16 | **不得升级 React**（全局约束）。新建 `src/types/react-use-shim.d.ts`，内容如下——注意 `export {}` 不可省：没有它，`declare module 'react'` 是**替换**整个 react 类型声明（会摧毁全部组件类型）；有它才是**增强**：<br><br>`// src/types/react-use-shim.d.ts`<br>`export {}`<br>`declare module 'react' {`<br>`  // TODO: Phase 4 - React 18 无 use() API，运行期需确认这 7 个文件的实际用法`<br>`  export const use: any`<br>`}` |

- [ ] **步骤 1：循环清零**

重复以下循环直到 `grep -c "error TS"` 为 `0`：

```bash
bunx tsc --noEmit 2>&1 | grep -oE "error TS[0-9]+" | sort | uniq -c | sort -rn | head -5
# 取数量最多的码，按上表配方处理；每次只处理一个码
bunx tsc --noEmit 2>&1 | grep -c "error TS"
```

**每轮规则：** ① 先确认该码的全部实例属于同一根因再动手；② 修复必须是最小改动（优先改 stub，其次改调用点铸型，最后才考虑改真实文件）；③ 每处理完一个码跑一次回归门。

- [ ] **步骤 2：React `use` 导出处理**

按上表最后一行建 `src/types/react-use-shim.d.ts`（若步骤 1 循环里这 7 个文件的错误仍在）。

- [ ] **步骤 3：终局验证**

执行：
```bash
bunx tsc --noEmit; echo "tsc exit: $?"
```
期望：无任何输出，`tsc exit: 0`

- [ ] **步骤 4：全量回归**

执行：
```bash
bun build src/entrypoints/cli.tsx --target=bun --outdir=dist; echo "build exit: $?"
bun -e "import { feature } from './src/types/bun-bundle.js'; console.log('runtime shim:', feature('SIMPLE'))"
```
期望：`build exit: 0`；`runtime shim: true`

- [ ] **步骤 5：提交**

```bash
git add src/
git commit -m "fix(phase2): tsc --noEmit 零错误"
```

---

### Task 13：修订 spec 并收口 Phase 2

**文件：**
- 修改：`docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md`
- 修改：`docs/superpowers/plans/2026-09-23-phase-2-typecheck.md`（勾选步骤）

**接口：**
- 输入：Task 1-12 的实测数据
- 输出：spec 的 Phase 2 状态、§9.1/§9.2/§9.4/§9.5 与实测一致

- [ ] **步骤 1：把实测结论写进 spec §0 修订记录表**

至少补这几行（数字用本计划各任务提交信息里记录的实测值，不要照抄下面的约数）：

```markdown
| 2026-09-23 | §9.1 | 「6 条声明对应的桩文件已创建」之外，新增记录：`src/types/message.ts`、`src/types/tools.ts`、`src/types/utils.ts`、`src/constants/querySource.ts`、`src/entrypoints/sdk/controlTypes.ts` 等 ~95 个类型/工具模块在泄露源码里同样不存在，Phase 2 以声明桩补齐（引用点最多的是 `types/message.ts`，183 处） |
| 2026-09-23 | §2.2 / §9.3 | Bun 1.3.14 **原生实现** `bun:bundle` 的 `feature()` 并在构建期 DCE：bunfig.toml 未定义的旗标构建期为 `false`，我们写的 `src/types/bun-bundle.js`（恒 true）只在 `bun run` 运行期生效。两者语义分歧需在 Phase 3 复核 |
| 2026-09-23 | §3 Phase 2 | 原 4 项任务（补类型定义/修模块引用/Bun 桩/SDK 类型）实测不足以清零：基线 1,922 处错误，四类高杠杆修复后仍剩约 1,500 处真实类型错误，需按错误码逐类清理。Phase 2 实际工作量 1-2 周 → 记录为 <实际> |
```

- [ ] **步骤 2：更新 §9.1 缺失模块清单**

把 Phase 2 新建的全部桩文件按「§9.5 构建期缺失」同款表格补进 §9.1 或 §9.5（哪个更贴近放哪个，保持一份，不要两处重复）。

- [ ] **步骤 3：更新 §3 Phase 2 状态**

把 Phase 2 的「任务」和「交付物」两段标注为已完成，交付物补上实测命令输出（`tsc --noEmit` 零错误、`bun build` 退出码 0）。

- [ ] **步骤 4：在 §10 下一步行动把 Phase 3 列为立即执行**

- [ ] **步骤 5：验证文档自洽**

执行：
```bash
grep -n "Phase 2" docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md | head -10
```
期望：Phase 2 段落反映完成状态，无「待办」字样与实测矛盾

- [ ] **步骤 6：提交**

```bash
git add docs/
git commit -m "docs(phase2): 按实测修订 spec 的 Phase 2 状态与缺失模块清单"
```

---

## 总检查清单

- [ ] Task 1：`lib: ES2024` + `@types/bun` + `allowImportingTsExtensions`，TS2867/TS2550/TS5097 归零
- [ ] Task 2：`@anthropic-ai/sdk` 升级至 0.127.x，SDK 相关 105 处错误归零
- [ ] Task 3：6 个遗漏外部包安装 + `vscode-jsonrpc/node.js` paths 映射，外部包 TS2307 归零
- [ ] Task 4：`scripts/phase2/gen_missing_module_stubs.py` 建成，~95 个缺失模块草稿生成，TS2307 568→<100
- [ ] Task 5：`src/types/message.ts` 判别联合重建（183 引用点）
- [ ] Task 6：`src/types/` 其余 + `entrypoints/sdk` 再导出链补齐，TS2460/TS2315 归零
- [ ] Task 7：13 个中流量模块形状精修
- [ ] Task 8：长尾 ~60 模块 + 20 个 claude-api `.md`，本地 TS2307 归零
- [ ] Task 9：95 处构建变体字面量归一化，TS2367 归零；18 个 `.js` 文件运行期导出名核对
- [ ] Task 10：`never` 收窄类 TS2339 归零
- [ ] Task 11：`unknown` 传播类错误归零
- [ ] Task 12：残余错误码逐类清零，`tsc --noEmit` 退出码 0
- [ ] Task 13：spec §0/§3/§9 修订收口
- [ ] 全程：每个任务后 `bun build src/entrypoints/cli.tsx --target=bun --outdir=dist` 退出码 0
