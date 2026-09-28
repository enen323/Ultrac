# 功能桩注册表

**用途：** 定位当前实际存在的桩，供后续还原时参考。
**最后重建：** 2026-09-28（整表重写——删除了「计划桩化但从未执行」的清单，只保留实测事实）

---

## 一、如何查找桩（唯一可靠判据）

每个桩文件的首行带 `// TODO: Phase N` 标记：

```bash
grep -rl "^// TODO: Phase [0-9]" src/ --include=*.ts --include=*.tsx --include=*.js | sort
```

**当前共 35 个。**

> ⚠️ **不要按「哪个模块应该被桩化」去推断。**
>
> 本文件 2026-09-28 之前的版本列了 69 条「Phase 6 还原清单」（OAuth 客户端、遥测、会话存储、插件加载器等）。
> **那些文件实际全都是完整真实现，从未桩化**——照那份清单去还原等于白干。
> 判据只有一个：文件首行有没有 `// TODO: Phase` 标记。

---

## 二、A 类：shim（8 个）—— 替代无法获取的外部包

| 文件 | 替代的包 | 为什么需要 |
|---|---|---|
| `src/types/shims/claude-for-chrome-mcp.ts` | `@ant/claude-for-chrome-mcp` | Anthropic 内部包，npm 上没有 |
| `src/types/shims/computer-use-mcp.ts` | `@ant/computer-use-mcp` | 同上 |
| `src/types/shims/computer-use-mcp-types.ts` | 同上（类型部分） | 同上 |
| `src/types/shims/computer-use-mcp-sentinelApps.ts` | 同上（应用清单） | 同上 |
| `src/types/shims/computer-use-swift.ts` | `@ant/computer-use-swift` | 同上（macOS 原生） |
| `src/types/shims/computer-use-input.ts` | `@ant/computer-use-input` | 同上 |
| `src/types/shims/color-diff-napi.ts` | `color-diff-napi` | npm 上是 Anthropic 抢注的空壳（`module.exports = {}`，description "This package name has been reserved."） |
| `src/types/shims/compiler-runtime.ts` | ~~`react/compiler-runtime`~~ | **已废弃**——React 19 自带真实现，tsconfig paths 映射已删除（2026-09-28）。文件保留但不再被引用 |

### shim 取舍原则

| 情况 | 判据 | 处置 |
|---|---|---|
| 公开包，npm 有真实现 | 打开 `node_modules/<包>` 有真实代码 | **不要 shim**，删掉 tsconfig `paths` 映射 |
| 私有包 / npm 上是空壳 | `index.js` 是 `module.exports = {}`，描述含 "reserved" | **必须 shim** |

**教训**：`react/compiler-runtime` 曾被 shim 成 `new Array(size)`，丢掉了「跨渲染持久化」语义，导致 React Compiler 的 memo 缓存全部失效，表现为 `undefined is not an object (evaluating 'store.setState')` 这类看似无关的错误。

---

## 三、B 类：本地缺失模块桩（27 个）—— 泄露源码里文件不存在

这些模块被源码引用，但泄露包解压后磁盘上没有对应文件，建桩以保证构建与运行可解析。

| 文件 |
|---|
| `src/assistant/AssistantSessionChooser.tsx` |
| `src/cli/bg.ts` |
| `src/cli/handlers/templateJobs.ts` |
| `src/commands/agents-platform/index.ts` |
| `src/commands/assistant/assistant.ts` |
| `src/components/agents/SnapshotUpdateDialog.tsx` |
| `src/daemon/main.ts` |
| `src/daemon/workerRegistry.ts` |
| `src/entrypoints/sdk/coreTypes.generated.ts` |
| `src/entrypoints/sdk/runtimeTypes.ts` |
| `src/entrypoints/sdk/toolTypes.ts` |
| `src/environment-runner/main.ts` |
| `src/ink/devtools.ts` |
| `src/ink/global.d.ts` |
| `src/self-hosted-runner/main.ts` |
| `src/services/compact/cachedMicrocompact.ts` |
| `src/services/compact/snipCompact.ts` |
| `src/services/contextCollapse/index.js` |
| `src/tools/REPLTool/REPLTool.ts` |
| `src/tools/SuggestBackgroundPRTool/SuggestBackgroundPRTool.ts` |
| `src/tools/TungstenTool/TungstenLiveMonitor.ts` |
| `src/tools/TungstenTool/TungstenTool.ts` |
| `src/tools/VerifyPlanExecutionTool/VerifyPlanExecutionTool.ts` |
| `src/tools/WorkflowTool/constants.ts` |
| `src/types/connectorText.ts` |
| `src/utils/filePersistence/types.ts` |
| `src/utils/protectedNamespace.ts` |

**说明**：各桩文件首行/注释里写了自己的用途和已知调用契约，还原时以文件内注释为准，不必依赖本表。

---

## 四、可达性（2026-09-28 实测）

用 `bun build --metafile` 对账的结果：

| 分类 | 数量 | 含义 |
|---|---|---|
| 进 bundle（会被真实加载） | 17 | 形状必须正确，否则运行时报错 |
| 被 DCE（当前永不执行） | 18 | 被 `feature()` 构建期删除，暂无需处理 |

**注意**：这个分类依赖当前 `bunfig.toml`（零 feature 旗标定义）。一旦启用某个 feature，可达集会变化，之前"不可达"的桩可能变成"可达"。改 `bunfig.toml` 或加 `--feature` 后需重新对账。

---

## 五、还原流程

1. `grep -rl "^// TODO: Phase" src/` 列出当前所有桩
2. 读该文件内的注释，了解用途与已知调用契约
3. 实现真实功能后，删掉 `// TODO: Phase` 标记
4. 验证：`bun build` 通过 + 实际运行路径正常（不要只看 `--version`）
5. 若是 shim，同时清理 `tsconfig.json` 里对应的 `paths` 映射
