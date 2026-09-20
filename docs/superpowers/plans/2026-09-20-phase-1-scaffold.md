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

---

### Task 1：创建 `package.json`

**文件：**
- 新建：`package.json`

**接口：**
- 输入：无（第一个任务）
- 输出：包含 `name`、`version`、`private: true`、`type: "module"`、`dependencies`、`scripts.build` 的 `package.json`

- [ ] **步骤 1：编写 `package.json`**

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

- [ ] **步骤 2：验证 JSON 合法**

执行：`bun run node -e "JSON.parse(require('fs').readFileSync('package.json','utf8')); console.log('OK')"`
期望输出：`OK`

- [ ] **步骤 3：安装依赖**

执行：`bun install`
期望：安装到 `node_modules/`，生成 `bun.lock`

- [ ] **步骤 4：提交**

```bash
git add package.json bun.lock
git commit -m "feat: 添加 package.json 并声明核心依赖"
```

---

### Task 2：创建 `tsconfig.json`

**文件：**
- 新建：`tsconfig.json`

**接口：**
- 输入：无（独立任务）
- 输出：`tsconfig.json`，包含 `paths` 别名（`src/*` 和 `bun:bundle`）、`moduleResolution: "bundler"`、`jsx: "react-jsx"`、`skipLibCheck: true`

- [ ] **步骤 1：编写 `tsconfig.json`**

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

- [ ] **步骤 2：验证 JSON 合法**

执行：`bun run node -e "JSON.parse(require('fs').readFileSync('tsconfig.json','utf8')); console.log('OK')"`
期望输出：`OK`

- [ ] **步骤 3：提交**

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

- [ ] **步骤 1：编写 `bunfig.toml`**

```toml
[define]
MACRO.VERSION = "\"1.0.0-dev\""
```

- [ ] **步骤 2：验证 Bun 能正确解析**

执行：`bun -e "console.log('bunfig OK')"`
期望输出：`bunfig OK`（Bun 会自动从当前目录加载 `bunfig.toml`，解析错误会抛出异常）

- [ ] **步骤 3：提交**

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

- [ ] **步骤 1：编写扩展后的 MACRO 声明**

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

- [ ] **步骤 2：提交**

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

- [ ] **步骤 1：创建类型声明文件**

创建 `src/types/bun-bundle.d.ts`：

```ts
declare module 'bun:bundle' {
  export function feature(name: string): boolean;
}
```

- [ ] **步骤 2：创建运行时桩文件**

创建 `src/types/bun-bundle.js`：

```js
export function feature(name) {
  return true;
}
```

- [ ] **步骤 3：验证模块可被正确解析**

执行：`bun -e "import { feature } from './src/types/bun-bundle.js'; console.log('feature(SIMPLE):', feature('SIMPLE'))"`
期望输出：`feature(SIMPLE): true`

- [ ] **步骤 4：提交**

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

- [ ] **步骤 1：修复 import 路径**

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

- [ ] **步骤 2：提交**

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

- [ ] **步骤 1：移除已存在文件的声明**

将 `src/types/missing-modules.d.ts` 整体替换为：

```ts
declare module '../daemon/workerRegistry.js';
declare module '../daemon/main.js';
declare module '../cli/bg.js';
declare module '../cli/handlers/templateJobs.js';
declare module '../environment-runner/main.js';
declare module '../self-hosted-runner/main.js';
```

- [ ] **步骤 2：创建 `src/daemon/workerRegistry.ts`**

```ts
// TODO: Phase 6 - 还原 daemon worker 逻辑
export async function runDaemonWorker(kind: string): Promise<void> {
  // STUB: 空实现
}
```

- [ ] **步骤 3：创建 `src/daemon/main.ts`**

```ts
// TODO: Phase 6 - 还原 daemon 主进程
export async function daemonMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [ ] **步骤 4：创建 `src/cli/bg.ts`**

```ts
// TODO: Phase 6 - 还原后台会话管理
export async function psHandler(args: string[]): Promise<void> {}
export async function logsHandler(sessionId: string): Promise<void> {}
export async function attachHandler(sessionId: string): Promise<void> {}
export async function killHandler(sessionId: string): Promise<void> {}
export async function handleBgFlag(args: string[]): Promise<void> {}
```

- [ ] **步骤 5：创建 `src/cli/handlers/templateJobs.ts`**

```ts
// TODO: Phase 6 - 还原模板任务命令
export async function templatesMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [ ] **步骤 6：创建 `src/environment-runner/main.ts`**

```ts
// TODO: Phase 6 - 还原 environment runner
export async function environmentRunnerMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [ ] **步骤 7：创建 `src/self-hosted-runner/main.ts`**

```ts
// TODO: Phase 6 - 还原 self-hosted runner
export async function selfHostedRunnerMain(args: string[]): Promise<void> {
  // STUB: 空实现
}
```

- [ ] **步骤 8：提交**

```bash
git add src/types/missing-modules.d.ts \
  src/daemon/workerRegistry.ts src/daemon/main.ts \
  src/cli/bg.ts src/cli/handlers/templateJobs.ts \
  src/environment-runner/main.ts src/self-hosted-runner/main.ts
git commit -m "feat: 为 cli.tsx 引用的缺失模块添加桩文件"
```

---

### Task 8：验证 `bun build` 成功

**文件：**
- 无（仅验证）

**接口：**
- 输入：以上所有任务完成后的产物
- 输出：`dist/cli.js`（或错误信息）

- [ ] **步骤 1：运行构建**

执行：`bun build src/entrypoints/cli.tsx --target=bun --outdir=dist`
期望：无错误，生成 `dist/cli.js`

- [ ] **步骤 2：如有错误，分类并修复**

可能的错误类别：
1. **Cannot find module** — 依赖缺失或路径别名错误 → 修复 import 或添加依赖
2. **源代码文件中的语法/类型错误** → 最小化修复以通过构建
3. **Bun API 类型缺失** → 添加到 `src/types/node-globals.d.ts`

循环：修复 → 重新构建 → 重复直到通过。

- [ ] **步骤 3：提交成功的构建**

```bash
git add dist/
git commit -m "build: bun build src/entrypoints/cli.tsx 构建成功"
```

---

## 总检查清单

- [ ] Task 1：`package.json` 已创建 + 依赖已安装
- [ ] Task 2：`tsconfig.json` 含路径别名
- [ ] Task 3：`bunfig.toml` 含 `MACRO.VERSION` 定义
- [ ] Task 4：`MACRO` 命名空间已扩展（`VERSION`、`PACKAGE_URL`、`NATIVE_PACKAGE_URL`、`BUILD_TIME`、`FEEDBACK_CHANNEL`、`ISSUES_EXPLAINER`）
- [ ] Task 5：`bun:bundle` `feature()` 类型 + 运行时桩
- [ ] Task 6：`../main.js` → `../main.tsx` import 修复
- [ ] Task 7：6 个桩文件已创建，`missing-modules.d.ts` 已清理
- [ ] Task 8：`bun build src/entrypoints/cli.tsx` 成功
