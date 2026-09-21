# Phase 1 — 项目脚手架实施计划

> **对代理worker：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 按任务逐步执行此计划。步骤使用复选框 (`- [ ]`) 语法跟踪进度。

**目标：** `bun build src/entrypoints/cli.tsx` 成功，零构建错误。

**架构：** 创建三个根目录配置文件（`package.json`、`tsconfig.json`、`bunfig.toml`）来声明依赖、路径别名和宏注入。为两个仅构建时存在的构造（`MACRO` 命名空间、`bun:bundle` `feature()`）提供运行时定义。修复 `cli.tsx` 中断裂的 import 路径，为源码中不存在但在运行时动态 import 的模块添加桩文件。

**技术栈：** Bun（运行时 + 打包器）、TypeScript

**规格说明：** `docs/superpowers/specs/2026-09-19-reconstruct-runnable-project-design.md`

## 全局约束

- 运行时：Bun（代码使用 `Bun.*` API 和 `bun:bundle` 特性门控）
- 交付物：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist` 成功
- `MACRO.VERSION` 默认值：`"1.0.0-dev"`（可通过 `bunfig.toml` 的 define 覆写）
- `bun:bundle` `feature()` 桩实现：Phase 1 所有门控返回 `true`
- `src/*` 路径别名映射到 `./src/*`
- 所有桩导出均为 `export async function`，空操作或返回空值
- 每个任务完成前必须提交 commit
- **`@ant/*` 作用域的 4 个包（`computer-use-mcp`、`computer-use-swift`、`computer-use-input`、`claude-for-chrome-mcp`）是 Anthropic 内部包，公开 npm registry 上不存在。禁止 `bun add`，禁止在 `node_modules/` 手写 stub。** 处理方式见 spec §9.4-A。
- **`react/compiler-runtime` 不是可安装包**（`react@18.3.12` 的 `exports` 无此子路径），需要本地 shim。见 spec §9.4-B。
- **`@anthropic-ai/claude-agent-sdk`、`@anthropic-ai/mcpb`、`@anthropic-ai/sandbox-runtime` 是公开包**，可正常 `bun add`。不要因为名字像内部包就跳过它们。

---

### Task 1：创建 `package.json`

> **2026-09-20 修订：** 原步骤 1 的 10 个依赖经实测**不足以让 `bun build` 成功**——真实外部依赖见下方「修订后的依赖清单」（2026-09-21 逐包清点为 **66 个**，此前写作 62 个是当时的分组计数误差）。下方保留原始内容作为历史记录，实际依赖集以「修订后的依赖清单」为准。

> **2026-09-21 修订：** 依赖集在 62 个基础上微调两处（Task 8a 执行）：`commander` **移除**（全仓库 grep 无 `from 'commander'` / `require('commander')` 引用，是原 10 个依赖里唯一未被源码引用的包）；`@anthropic-ai/foundry-sdk` **加入**（`src/services/api/client.ts:192` 动态 import `AnthropicFoundry`，spec §9.2 的 grep 提取漏录，已同步补录）。

**文件：**
- 新建：`package.json`

**接口：**
- 输入：无（第一个任务）
- 输出：包含 `name`、`version`、`private: true`、`type: "module"`、`dependencies`、`scripts.build` 的 `package.json`

- [x] **步骤 1：编写 `package.json`**

在仓库根目录创建 `package.json`：

```json
{
  "name": "claude-code-reconstructed",
  "version": "1.0.0-dev",
  "private": true,
  "type": "module",
  "scripts": {
    "build": "bun build src/entrypoints/cli.tsx --target=bun --outdir=dist"
  },
  "dependencies": {
    "@anthropic-ai/sdk": "^0.27.0",
    "chalk": "^5.4.1",
    "commander": "^12.1.0",
    "@commander-js/extra-typings": "^12.1.0",
    "ink": "^5.0.0",
    "react": "^18.3.1",
    "react-reconciler": "^0.29.2",
    "lodash-es": "^4.18.0",
    "strip-ansi": "^7.1.0",
    "figures": "^6.0.1"
  },
  "devDependencies": {
    "@types/react": "^18.3.12",
    "@types/lodash-es": "^4.14.202",
    "typescript": "^5.6.3"
  }
}
```

- [x] **步骤 2：验证 JSON 合法**

执行：`bun run node -e "JSON.parse(require('fs').readFileSync('package.json','utf8')); console.log('OK')"`
期望输出：`OK`

- [x] **步骤 3：安装依赖**

执行：`bun install`
期望：安装到 `node_modules/`，生成 `bun.lock`

- [x] **步骤 4：提交**

```bash
git add package.json bun.lock
git commit -m "feat: 添加 package.json 并声明核心依赖"
```

#### 修订后的依赖清单

**需要 `bun add` 的公开包（外部依赖共 66 个，排除 4 个内部 `@ant/*` 后的可安装部分；2026-09-21 逐包清点确认，此前「62 个」为分组计数误差）：**

```
@alcalzone/ansi-tokenize          @anthropic-ai/claude-agent-sdk
@anthropic-ai/mcpb                @anthropic-ai/sandbox-runtime
@anthropic-ai/sdk                 @aws-sdk/client-bedrock-runtime
@commander-js/extra-typings       @growthbook/growthbook
@modelcontextprotocol/sdk         @opentelemetry/api
@opentelemetry/api-logs           @opentelemetry/core
@opentelemetry/resources          @opentelemetry/sdk-logs
@opentelemetry/sdk-metrics        @opentelemetry/sdk-trace-base
@opentelemetry/semantic-conventions
ajv asciichart auto-bind axios bidi-js chalk chokidar
cli-boxes code-excerpt color-diff-napi diff emoji-regex
env-paths execa figures fuse.js get-east-asian-width
google-auth-library highlight.js https-proxy-agent ignore
indent-string ink jsonc-parser lodash-es lru-cache marked
picomatch p-map proper-lockfile qrcode react react-reconciler
semver shell-quote stack-utils strip-ansi supports-hyperlinks
tree-kill type-fest undici usehooks-ts vscode-jsonrpc
vscode-languageserver-protocol vscode-languageserver-types
wrap-ansi ws xss zod
```

**devDependencies：** `@types/react`、`@types/lodash-es`、`typescript`

**不要安装的 4 个（内部包，改用声明 + shim）：** `@ant/computer-use-mcp`、`@ant/computer-use-swift`、`@ant/computer-use-input`、`@ant/claude-for-chrome-mcp`

**验证方式：** 装完后跑 `bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`，把报错的 "Cannot find module" 逐个对照 spec §9.2 / §9.4 分类处理——公开包补装，内部包和 `react/compiler-runtime` 写 shim。

---

### Task 2：创建 `tsconfig.json`

**文件：**
- 新建：`tsconfig.json`

**接口：**
- 输入：无（独立任务）
- 输出：`tsconfig.json`，包含 `paths` 别名（`src/*` 和 `bun:bundle`）、`moduleResolution: "bundler"`、`jsx: "react-jsx"`、`skipLibCheck: true`

- [x] **步骤 1：编写 `tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ES2022",
    "moduleResolution": "bundler",
    "lib": ["ES2022"],
    "jsx": "react-jsx",
    "jsxImportSource": "react",
    "strict": false,
    "esModuleInterop": true,
    "allowSyntheticDefaultImports": true,
    "forceConsistentCasingInFileNames": true,
    "skipLibCheck": true,
    "resolveJsonModule": true,
    "isolatedModules": true,
    "noEmit": true,
    "baseUrl": ".",
    "paths": {
      "src/*": ["./src/*"],
      "bun:bundle": ["./src/types/bun-bundle.d.ts", "./src/types/bun-bundle.js"]
    }
  },
  "include": ["src/**/*.ts", "src/**/*.tsx"],
  "exclude": ["node_modules", "dist"]
}
```

- [x] **步骤 2：验证 JSON 合法**

执行：`bun run node -e "JSON.parse(require('fs').readFileSync('tsconfig.json','utf8')); console.log('OK')"`
期望输出：`OK`

- [x] **步骤 3：提交**

```bash
git add tsconfig.json
git commit -m "feat: 添加 tsconfig.json 并配置路径别名"
```

---

### Task 3：创建 `bunfig.toml`

**文件：**
- 新建：`bunfig.toml`

**接口：**
- 输入：无（独立任务）
- 输出：定义了构建期 `MACRO.VERSION` 的 `bunfig.toml`

- [x] **步骤 1：编写 `bunfig.toml`**

```toml
[define]
MACRO.VERSION = "\"1.0.0-dev\""
```

- [x] **步骤 2：验证 Bun 能正确解析**

执行：`bun -e "console.log('bunfig OK')"`
期望输出：`bunfig OK`（Bun 会自动从当前目录加载 `bunfig.toml`，解析错误会抛出异常）

- [x] **步骤 3：提交**

```bash
git add bunfig.toml
git commit -m "feat: 添加 bunfig.toml 并定义 MACRO.VERSION"
```

---

### Task 4：扩展 `MACRO` 类型声明

**文件：**
- 修改：`src/types/node-globals.d.ts:13-15`

**接口：**
- 输入：全仓库 `MACRO.*` 成员使用情况的 grep 结果
- 输出：完整的 `MACRO` 命名空间类型声明，包含 `VERSION`、`PACKAGE_URL`、`NATIVE_PACKAGE_URL`、`BUILD_TIME`、`FEEDBACK_CHANNEL`、`ISSUES_EXPLAINER`

已使用的成员（来自 grep）：
- `MACRO.VERSION` — 约 80 处引用
- `MACRO.PACKAGE_URL` — `src/cli/update.ts`、`src/utils/autoUpdater.ts`、`src/utils/localInstaller.ts`、`src/utils/nativeInstaller/installer.ts`、`src/utils/doctorDiagnostic.ts`、`src/components/AutoUpdater.tsx`、`src/components/PackageManagerAutoUpdater.tsx`
- `MACRO.NATIVE_PACKAGE_URL` — `src/utils/autoUpdater.ts:428`
- `MACRO.BUILD_TIME` — `src/commands/version.ts`、`src/services/analytics/metadata.ts`、`src/services/api/logging.ts`
- `MACRO.FEEDBACK_CHANNEL` — `src/utils/auth.ts`、`src/services/api/errors.ts`、`src/services/mcp/headersHelper.ts`
- `MACRO.ISSUES_EXPLAINER` — `src/constants/prompts.ts`、`src/tools/AgentTool/built-in/claudeCodeGuideAgent.ts`

- [x] **步骤 1：编写扩展后的 MACRO 声明**

替换 `src/types/node-globals.d.ts` 的第 13-15 行：

```ts
declare const MACRO: {
  VERSION: string;
  PACKAGE_URL: string;
  NATIVE_PACKAGE_URL: string;
  BUILD_TIME: string;
  FEEDBACK_CHANNEL: string;
  ISSUES_EXPLAINER: string;
};
```

- [x] **步骤 2：提交**

```bash
git add src/types/node-globals.d.ts
git commit -m "feat: 扩展 MACRO 类型声明以包含所有使用的成员"
```

---

### Task 5：提供 `bun:bundle` feature() 运行时桩

**文件：**
- 新建：`src/types/bun-bundle.d.ts`、`src/types/bun-bundle.js`

**接口：**
- 输入：无（独立类型 + 运行时桩）
- 输出：`bun:bundle` `feature()` 函数的 TypeScript 类型声明 + 运行时实现

已使用的特性门控（来自 grep — 80+ 文件）：
- `feature('ABLATION_BASELINE')`、`feature('DUMP_SYSTEM_PROMPT')`、`feature('CHICAGO_MCP')`
- `feature('DAEMON')`、`feature('BRIDGE_MODE')`、`feature('BG_SESSIONS')`
- `feature('TEMPLATES')`、`feature('BYOC_ENVIRONMENT_RUNNER')`、`feature('SELF_HOSTED_RUNNER')`
- `feature('SIMPLE')`、`feature('AUTO_COMPACT')` 等更多

Phase 1 所有门控返回 `true`（不做 DCE）。

- [x] **步骤 1：创建类型声明文件**

创建 `src/types/bun-bundle.d.ts`：

```ts
declare module 'bun:bundle' {
  export function feature(name: string): boolean;
}
```

- [x] **步骤 2：创建运行时桩文件**

创建 `src/types/bun-bundle.js`：

```js
export function feature(name) {
  return true;
}
```

- [x] **步骤 3：验证模块可被正确解析**

执行：`bun -e "import { feature } from './src/types/bun-bundle.js'; console.log('feature(SIMPLE):', feature('SIMPLE'))"`
期望输出：`feature(SIMPLE): true`

- [x] **步骤 4：提交**

```bash
git add src/types/bun-bundle.d.ts src/types/bun-bundle.js
git commit -m "feat: 添加 bun:bundle feature() 类型声明和运行时桩实现"
```

---

### Task 6：修复 `cli.tsx` 中断裂的 `../main.js` import

**文件：**
- 修改：`src/entrypoints/cli.tsx:296`

**接口：**
- 输入：`src/main.tsx` 存在于 `src/entrypoints/` 的相对路径 `../main.tsx` 处
- 输出：能正确解析到实际文件的动态 import

问题：`cli.tsx` 第 296 行执行 `await import('../main.js')`，但实际文件是 `src/main.tsx`。Bun/TypeScript 不会从 `.js` import 说明符自动解析到 `.tsx`。

- [x] **步骤 1：修复 import 路径**

在 `src/entrypoints/cli.tsx` 第 296 行，将：

```ts
const {
  main: cliMain
} = await import('../main.js');
```

改为：

```ts
const {
  main: cliMain
} = await import('../main.tsx');
```

- [x] **步骤 2：提交**

```bash
git add src/entrypoints/cli.tsx
git commit -m "fix: 将 import ../main.js 改为 ../main.tsx"
```

---

### Task 7：清理 `missing-modules.d.ts` 并添加桩文件

**文件：**
- 修改：`src/types/missing-modules.d.ts`
- 新建：`src/daemon/workerRegistry.ts`、`src/daemon/main.ts`、`src/cli/bg.ts`、`src/cli/handlers/templateJobs.ts`、`src/environment-runner/main.ts`、`src/self-hosted-runner/main.ts`

**接口：**
- 输入：文件存在性检查结果（6 个模块为 MISSING，`config.ts` 和 `sinks.ts` 为 EXISTS）
- 输出：删除已存在文件的声明，为不存在的文件创建桩 .ts 文件

当前 `missing-modules.d.ts` 声明了 9 个模块：
- `../daemon/workerRegistry.js` — 缺失 → 创建桩
- `../daemon/main.js` — 缺失 → 创建桩
- `../cli/bg.js` — 缺失 → 创建桩
- `../cli/handlers/templateJobs.js` — 缺失 → 创建桩
- `../environment-runner/main.js` — 缺失 → 创建桩
- `../self-hosted-runner/main.js` — 缺失 → 创建桩
- `../main.js` — 实际存在 `main.tsx` → 移除声明（Task 6 已修复）
- `../utils/config.js` — 实际存在 `config.ts` → 移除声明
- `../utils/sinks.js` — 实际存在 `sinks.ts` → 移除声明

- [x] **步骤 1：移除已存在文件的声明**

将 `src/types/missing-modules.d.ts` 整体替换为：

```ts
declare module '../daemon/workerRegistry.js';
declare module '../daemon/main.js';
declare module '../cli/bg.js';
declare module '../cli/handlers/templateJobs.js';
declare module '../environment-runner/main.js';
declare module '../self-hosted-runner/main.js';
```

- [x] **步骤 2：创建 `src/daemon/workerRegistry.ts`**

```ts
// TODO: Phase 6 - 还原 daemon worker 逻辑
export async function runDaemonWorker(kind: string): Promise<void> {
  // STUB: 空实现
}
```

- [x] **步骤 3：创建 `src/daemon/main.ts`**

```ts
// TODO: Phase 6 - 还原 daemon 主进程
export async function daemonMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [x] **步骤 4：创建 `src/cli/bg.ts`**

```ts
// TODO: Phase 6 - 还原后台会话管理
export async function psHandler(args: string[]): Promise<void> {}
export async function logsHandler(sessionId: string): Promise<void> {}
export async function attachHandler(sessionId: string): Promise<void> {}
export async function killHandler(sessionId: string): Promise<void> {}
export async function handleBgFlag(args: string[]): Promise<void> {}
```

- [x] **步骤 5：创建 `src/cli/handlers/templateJobs.ts`**

```ts
// TODO: Phase 6 - 还原模板任务命令
export async function templatesMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [x] **步骤 6：创建 `src/environment-runner/main.ts`**

```ts
// TODO: Phase 6 - 还原 environment runner
export async function environmentRunnerMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [x] **步骤 7：创建 `src/self-hosted-runner/main.ts`**

```ts
// TODO: Phase 6 - 还原 self-hosted runner
export async function selfHostedRunnerMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [x] **步骤 8：提交**

```bash
git add src/types/missing-modules.d.ts \
  src/daemon/workerRegistry.ts src/daemon/main.ts \
  src/cli/bg.ts src/cli/handlers/templateJobs.ts \
  src/environment-runner/main.ts src/self-hosted-runner/main.ts
git commit -m "feat: 为 cli.tsx 引用的缺失模块添加桩文件"
```

---

### Task 8a：补全依赖集（66 个公开包 + `@anthropic-ai/foundry-sdk`）

> **2026-09-21 修订：** 原 Task 8 只有「跑构建、见错修错」四步，但 2026-09-21 实测 `bun build` 报 90 条 error、31 个唯一无法解析的模块，实际工作量拆成 Task 8a-8d 四个子任务顺序执行。原 Task 8 的验证目标（`bun build` 退出码 0、生成 `dist/cli.js`）不变，由 Task 8d 收口。

**文件：**
- 修改：`package.json`（dependencies 替换为修订后的依赖清单）
- 修改：`bun.lock`（`bun install` 生成）

**接口：**
- 输入：Task 1 的「修订后的依赖清单」（62 个公开包）+ `@anthropic-ai/foundry-sdk`
- 输出：`bun install` 成功，`node_modules/` 含全部公开依赖

**背景：** Task 1 提交的 `package.json` 只含原 10 个依赖。实测 `bun build` 需要 66 个公开依赖（spec §9.2）。本任务把依赖集补齐。

**规则：**
- dependencies 恰好为修订后的 66 个包 + `@anthropic-ai/foundry-sdk`（共 67 个）
- 原 10 个依赖中保留的 9 个（去掉 `commander`）沿用 `package.json` 中已有的版本范围（`^0.27.0` 等），不要改动
- 新增的 58 个包不写死版本，由 `bun add` 解析最新版后落入 `package.json`
- `react` 保持 `^18.3.1`、`ink` 保持 `^5.0.0`、`react-reconciler` 保持 `^0.29.2`——这三个的兼容性不能动
- devDependencies 保持 `@types/react`、`@types/lodash-es`、`typescript` 不变
- **禁止** `bun add` 4 个 `@ant/*` 内部包（`@ant/computer-use-mcp`、`@ant/computer-use-swift`、`@ant/computer-use-input`、`@ant/claude-for-chrome-mcp`）
- **禁止** `bun add react/compiler-runtime`（不可安装，Task 8b 处理）

**步骤：**

- [ ] 步骤 1：安装新增包

用 `bun add <pkg>...` 安装（可分批），确认最终 `package.json` 的 dependencies 与清单一致。

- [ ] 步骤 2：确认安装无错误

执行：`bun install`
期望：无解析错误，`bun.lock` 更新。

- [ ] 步骤 3：验证 JSON 合法

执行：`bun run node -e "JSON.parse(require('fs').readFileSync('package.json','utf8')); console.log('OK')"`
期望输出：`OK`

- [ ] 步骤 4：验证 SDK 可导入

执行：`bun -e "import { Anthropic } from '@anthropic-ai/sdk'; console.log('sdk OK')"`
期望输出：`sdk OK`

- [ ] 步骤 5：提交

```bash
git add package.json bun.lock
git commit -m "feat: 补全 62 个公开依赖并添加 foundry-sdk"
```

**注意：** `@commander-js/extra-typings` 安装后 bun 可能自动带上 `commander` peer 依赖——允许，不手动删除。若安装后 `bun build` 报 `@commander-js/extra-typings` 相关解析错误，在报告里说明，不要自行改 import。

---

### Task 8b：为 5 个不可安装的外部模块提供声明 + shim

**文件：**
- 修改：`tsconfig.json`（`paths` 增加 5 条映射）
- 修改：`src/types/missing-modules.d.ts`（增加 5 条 `declare module`）
- 新建：5 个运行时 shim（放 `src/types/shims/` 下，命名自定，`.ts` 后缀）

**接口：**
- 输入：spec §9.4（两类无法通过 npm 获取的依赖）
- 输出：`bun build` 不再报这 5 个模块的 "Could not resolve"

**背景：** bun 的 error 输出有上限，当前只报出 `@ant/claude-for-chrome-mcp`；但 `@ant/computer-use-mcp`、`@ant/computer-use-swift`、`@ant/computer-use-input` 源码确实引用（spec §9.4-A 引用点表），必须一并处理，否则 Task 8d 迭代时会再冒出来。

**5 个模块与源码引用点：**

| 模块 | 引用点 |
|---|---|
| `@ant/claude-for-chrome-mcp` | `src/skills/bundled/claudeInChrome.ts:1`、`src/utils/claudeInChrome/mcpServer.ts:6`、`src/utils/claudeInChrome/setup.ts:1` |
| `@ant/computer-use-mcp` | `src/utils/computerUse/{executor,gates,hostAdapter,inputLoader,mcpServer,setup,wrapper}.ts`、`src/components/permissions/ComputerUseApproval/ComputerUseApproval.tsx` |
| `@ant/computer-use-swift` | `src/utils/computerUse/swiftLoader.ts` |
| `@ant/computer-use-input` | `src/utils/computerUse/inputLoader.ts` |
| `react/compiler-runtime` | 62 个文件，统一 `import { c as _c } from "react/compiler-runtime"` |

**规则：**
- 每个 `@ant/*` 包：shim 导出该包引用点实际 import 的**全部命名导出**（自己 grep 引用点收集，逐个对照）。函数类导出遵循全局约束「所有桩导出均为 `export async function`，空操作或返回空值」；对象/常量类导出给空对象或空数组
- `react/compiler-runtime`：至少导出 `c`。实现给最小可用版（`c(size)` 返回可索引数组，React Compiler 编译产物按 `$[n]` 下标读写缓存），不要给会在运行时崩的形状。若源码还用了 `useMemoCache` 等导出，一并提供
- `declare module` 写进 `src/types/missing-modules.d.ts`（spec §9.4-A：与 missing-modules.d.ts 同一套机制）
- `tsconfig.json` 的 `paths` 增加 5 条，指向对应 shim（参考现有 `"bun:bundle"` 条目的 `.d.ts` + `.js` 双条目写法；已实测 bun build 遵循 tsconfig paths）。**不要动 `src/*` 和 `bun:bundle` 两条既有映射**
- shim 文件顶部加 `// TODO: Phase 6 - 还原 <包名> 真实实现`

**步骤：**

- [ ] 步骤 1：收集导出清单

grep 全部引用点，列出每个包的命名导出清单。

- [ ] 步骤 2：写 shim 与声明

创建 5 个 shim + 5 条 `declare module` + 5 条 paths 映射。

- [ ] 步骤 3：验证

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：错误列表里不再出现这 5 个模块名（其他错误允许存在）。

- [ ] 步骤 4：提交

```bash
git add tsconfig.json src/types/
git commit -m "feat: 为 @ant/* 内部包和 react/compiler-runtime 添加声明与 shim"
```

---

### Task 8c：为 26 个构建期缺失的本地模块建桩

**文件：**
- 新建：23 个桩文件（下表；26 个 specifier 中 `connectorText` 3 种写法、`TungstenTool` 2 处引用各归并为一个文件）

**接口：**
- 输入：下表（模块、目标文件、引用点）
- 输出：`bun build` 不再报这些模块的 "Could not resolve"

**背景：** 这些文件在泄露源码解压里**本来就不存在**（同级仓库 `claude-code/` 的 1911 个源文件同样没有；公开 npm 包 `@anthropic-ai/claude-code` 只有打包产物，没有 TS 源码），无法还原，只能建桩。它们全部是边缘/生成类模块（Tungsten、REPL 工具、assistant、agents-platform、verify skill 内容、SDK 类型再导出、devtools、compact），符合 spec「核心路径完整还原，边缘功能功能桩标记」策略。

| # | 引用点写的 specifier | 要建的文件 | 引用点 |
|---|---|---|---|
| 1 | `../types/connectorText.js` 等 3 种写法 | `src/types/connectorText.ts` | `src/utils/messages.ts:40`、`src/services/api/claude.ts:44`、`src/services/api/logging.ts:17` |
| 2 | `../global.d.ts` | `src/ink/global.d.ts` | `src/ink/components/Box.tsx:2`（副作用 import） |
| 3 | `../services/compact/snipCompact.js` | `src/services/compact/snipCompact.ts` | `src/utils/attachments.ts:3973`（动态 import） |
| 4 | `../tools/TungstenTool/TungstenLiveMonitor.js` | `src/tools/TungstenTool/TungstenLiveMonitor.ts` | `src/screens/REPL.tsx:270`（动态 import） |
| 5 | `../tools/WorkflowTool/constants.js` | `src/tools/WorkflowTool/constants.ts` | `src/constants/tools.ts:29` |
| 6 | `./assistant/AssistantSessionChooser.js` | `src/assistant/AssistantSessionChooser.tsx` | `src/dialogLaunchers.tsx:63`（动态 import） |
| 7 | `./cachedMicrocompact.js` | `src/services/compact/cachedMicrocompact.ts` | `src/services/compact/microCompact.ts:66` |
| 8 | `./commands/agents-platform/index.js` | `src/commands/agents-platform/index.ts` | `src/commands.ts:50` |
| 9 | `./commands/assistant/assistant.js` | `src/commands/assistant/assistant.ts` | `src/dialogLaunchers.tsx:77`（动态 import） |
| 10 | `./components/agents/SnapshotUpdateDialog.js` | `src/components/agents/SnapshotUpdateDialog.tsx` | `src/dialogLaunchers.tsx:36`（动态 import） |
| 11 | `./coreTypes.generated.js` | `src/entrypoints/sdk/coreTypes.generated.ts` | `src/entrypoints/sdk/coreTypes.ts:19` |
| 12 | `./devtools.js` | `src/ink/devtools.ts` | `src/ink/reconciler.ts:36` |
| 13 | `./protectedNamespace.js` | `src/utils/protectedNamespace.ts` | `src/utils/envUtils.ts:142`（require + typeof import 断言，需导出 `checkProtectedNamespace`） |
| 14 | `./sdk/runtimeTypes.js` | `src/entrypoints/sdk/runtimeTypes.ts` | `src/entrypoints/agentSdkTypes.ts:26` |
| 15 | `./sdk/toolTypes.js` | `src/entrypoints/sdk/toolTypes.ts` | `src/entrypoints/agentSdkTypes.ts:31` |
| 16 | `./tools/REPLTool/REPLTool.js` | `src/tools/REPLTool/REPLTool.ts` | `src/tools.ts:18`（require，需导出 `REPLTool`） |
| 17 | `./tools/SuggestBackgroundPRTool/SuggestBackgroundPRTool.js` | `src/tools/SuggestBackgroundPRTool/SuggestBackgroundPRTool.ts` | `src/tools.ts:22`（require，需导出 `SuggestBackgroundPRTool`） |
| 18 | `./tools/TungstenTool/TungstenTool.js` | `src/tools/TungstenTool/TungstenTool.ts` | `src/tools.ts:60`（静态 import `TungstenTool`）、`src/commands/clear/caches.ts:96`（动态 import） |
| 19 | `./tools/VerifyPlanExecutionTool/VerifyPlanExecutionTool.js` | `src/tools/VerifyPlanExecutionTool/VerifyPlanExecutionTool.ts` | `src/tools.ts:93`（require，需导出 `VerifyPlanExecutionTool`） |
| 20 | `./types.js` | `src/utils/filePersistence/types.ts` | `src/utils/filePersistence/filePersistence.ts:37` |
| 21 | `./verify/SKILL.md` | `src/skills/bundled/verify/SKILL.md` | `src/skills/bundled/verifyContent.ts:6` |
| 22 | `./verify/examples/cli.md` | `src/skills/bundled/verify/examples/cli.md` | `src/skills/bundled/verifyContent.ts:4` |
| 23 | `./verify/examples/server.md` | `src/skills/bundled/verify/examples/server.md` | `src/skills/bundled/verifyContent.ts:5` |

**规则：**
- 每个桩文件顶部加 `// TODO: Phase 6 - 还原 <模块名>`（`.md` 文件用首行说明）
- 函数导出遵循全局约束「所有桩导出均为 `export async function`，空操作或返回空值」
- 非函数导出（类型、常量、类、React 组件）：导出引用点实际使用的名字，形状取最小可用值（常量给空对象/空数组/假值；类给空 class 或最小实现；类型给宽松别名）。Phase 1 的门槛只是 `bun build`（不做类型检查），类型精确度留给 Phase 2
- `src/ink/global.d.ts`：`export {}` 即可（已实测 bun build 能把 `.d.ts` 副作用 import 打进 bundle）
- 3 个 `.md` 文件：给最小合法 markdown 内容（已实测 bun 默认把 `.md` 当 text 内联；内容会被 `verifyContent.ts` 当字符串导出）
- 引用点是 `require(...) as typeof import(...)` 的（envUtils），shim 要同时满足运行时值和类型两侧
- 不改任何引用点文件，不往 `missing-modules.d.ts` 加这些本地模块的声明（它们现在是真实文件）

**步骤：**

- [ ] 步骤 1：读引用点，收集命名导出

逐个读上表引用点的 import 语句，收集每个目标文件需要导出的名字。

- [ ] 步骤 2：建 23 个桩文件

- [ ] 步骤 3：验证

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：错误列表里不再出现上表任何 specifier。

- [ ] 步骤 4：提交

```bash
git add src/
git commit -m "feat: 为构建期缺失的 26 个本地模块建桩"
```

---

### Task 8d：迭代修复直到 `bun build` 通过

**文件：**
- 视错误而定（预期：`package.json`、`tsconfig.json`、`src/types/`、少量 `src/` 修复、`docs/` 按 spec §0 修订）

**接口：**
- 输入：Task 8a-8c 的产物
- 输出：`dist/cli.js` 生成，构建退出码 0

**规则：**
- 执行 `bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
- bun 的 error 输出有上限，每轮只报最先出现的一批。按 spec §9.2 / §9.4 分类处理新错误：
  1. **Cannot find module / Could not resolve** — 先判断包属于哪类，**不要一律 `bun add`**：
     - 公开包（spec §9.2 标 ✅）→ `bun add <pkg>`，并按 spec §0 在 §9.2 补录
     - `@ant/*` 4 个内部包 → `declare module` + 本地 shim，**不装**
     - `react/compiler-runtime` → 已有 shim（Task 8b）
  2. **本地 `.js`/`.md`/`.d.ts` 缺失** → 建桩（同 Task 8c 规则）
  3. **源代码文件中的语法/类型错误** → 最小化修复以通过构建
  4. **Bun API 类型缺失** → 添加到 `src/types/node-globals.d.ts`
- 循环：修复 → 重新构建 → 重复直到退出码 0
- **不要** `git add dist/`（构建产物，`.gitignore` 已忽略）
- 每轮分类若推翻 spec 判断，当场按 spec §0 修订文档

**步骤：**

- [ ] 步骤 1：迭代修复

- [ ] 步骤 2：确认构建通过

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：退出码 0，生成 `dist/cli.js`。

- [ ] 步骤 3：提交成功的构建

```bash
git add package.json bun.lock tsconfig.json src/ docs/
git commit -m "build: bun build src/entrypoints/cli.tsx 构建成功"
```

---

## 总检查清单

- [x] Task 1：`package.json` 已创建 + 依赖已安装
- [x] Task 2：`tsconfig.json` 含路径别名
- [x] Task 3：`bunfig.toml` 含 `MACRO.VERSION` 定义
- [x] Task 4：`MACRO` 命名空间已扩展（`VERSION`、`PACKAGE_URL`、`NATIVE_PACKAGE_URL`、`BUILD_TIME`、`FEEDBACK_CHANNEL`、`ISSUES_EXPLAINER`）
- [x] Task 5：`bun:bundle` `feature()` 类型 + 运行时桩
- [x] Task 6：`../main.js` → `../main.tsx` import 修复
- [x] Task 7：6 个桩文件已创建，`missing-modules.d.ts` 已清理
- [ ] Task 8a：依赖集补齐（66 个公开包 + `@anthropic-ai/foundry-sdk`，移除未引用的 `commander`）
- [ ] Task 8b：5 个不可安装外部模块（4 个 `@ant/*` + `react/compiler-runtime`）的声明 + shim
- [ ] Task 8c：23 个构建期缺失本地模块的桩文件（覆盖 26 个 specifier）
- [ ] Task 8d：`bun build src/entrypoints/cli.tsx` 成功
