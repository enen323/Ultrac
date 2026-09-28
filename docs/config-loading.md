# Ultrac 配置加载链路（模型 / MCP / 权限）

> 本文档记录 Ultrac（Claude Code fork）读取配置（模型、MCP、权限等）的整条链路，
> 含优先级顺序、入口函数、文件路径与关键代码位置。
> 代码行号基于排查时的快照，文件名稳定、行号可能随版本漂移，以函数名为准。
>
> **本 fork 已把所有配置/数据目录前缀从 `claude` 改为 `ultrac`**，见文末 §7。

---

## 0. 总览

配置体系分两层：

1. **统一 settings 合并**：所有 `settings*.json` 由 `loadSettingsFromDisk()` 读盘并合并成一份 `SettingsJson`。
2. **各领域各自取用**：模型 / MCP / 权限从这份合并结果 + 各自的专属文件（`<cwd>/.mcp.json`、`~/.ultrac.json`）里读取自己关心的字段。

核心认知：

- **环境变量（如 `ANTHROPIC_MODEL`、`ANTHROPIC_BASE_URL`）不参与 settings 合并**，而是在"使用点"单独读取。
- **企业 `managed-*` 文件普遍具有最高优先级或独占控制权**（settings / MCP / 权限均如此）。
- **配置/数据根目录有两级收口点**：用户级走 `getUltracConfigHomeDir()`（`envUtils.ts`，145 处调用），
  项目级走 `getRelativeSettingsFilePathForSource()`（`settings.ts`）。改前缀只需动这两处 + 少数绕过收口点的地方。

---

## 1. 统一 settings 合并

### 1.1 优先级（从低到高，后合并者覆盖先合并者）

```
plugin 基础设置  <  user  <  project  <  local  <  flag(--settings)  <  policy(managed)
```

- 顺序定义在 `src/utils/settings/constants.ts:7` 的 `SETTING_SOURCES`。
- `constants.ts:159` 的 `getEnabledSettingSources()` 保证 `policySettings` / `flagSettings` 始终追加，不可被 `--setting-sources` 关闭。

```ts
// src/utils/settings/constants.ts:7
export const SETTING_SOURCES = [
  'userSettings', 'projectSettings', 'localSettings',
  'flagSettings', 'policySettings',
] as const

// src/utils/settings/constants.ts:159
export function getEnabledSettingSources(): SettingSource[] {
  const allowed = getAllowedSettingSources()
  const result = new Set<SettingSource>(allowed)
  result.add('policySettings')   // 始终包含
  result.add('flagSettings')     // 始终包含
  return Array.from(result)
}
```

### 1.2 合并核心函数

`src/utils/settings/settings.ts:645` 的 `loadSettingsFromDisk()`（无缓存、真实读盘），
`settings.ts:812` 的 `getInitialSettings()` 是其带会话级缓存的公开入口。

```ts
// src/utils/settings/settings.ts:645
function loadSettingsFromDisk(): SettingsWithErrors {
  const pluginSettings = getPluginSettingsBase()   // 最低优先级 base
  let mergedSettings: SettingsJson = {}
  if (pluginSettings) mergedSettings = mergeWith(mergedSettings, pluginSettings, settingsMergeCustomizer)
  for (const source of getEnabledSettingSources()) {
    if (source === 'policySettings') {
      // 第一源胜出：remote > MDM(HKLM/plist) > managed-settings.json > HKCU
      // ...
      continue
    }
    const filePath = getSettingsFilePathForSource(source)
    if (filePath) {
      const { settings, errors } = parseSettingsFile(filePath)
      if (settings) mergedSettings = mergeWith(mergedSettings, settings, settingsMergeCustomizer)
    }
    if (source === 'flagSettings') {
      // 再 merge --settings 的内联 JSON（getFlagSettingsInline）
    }
  }
  return { settings: mergedSettings, errors: allErrors }
}

// src/utils/settings/settings.ts:812
export function getInitialSettings(): SettingsJson {
  const { settings } = getSettingsWithErrors()
  return settings || {}
}
```

合并规则：数组字段由 `settingsMergeCustomizer` 做 concat + 去重；其余字段 deep merge。

### 1.3 各 source 对应的文件路径

`src/utils/settings/settings.ts:274` `getSettingsFilePathForSource()`：

| 源 | 文件名 | 目录 |
|---|---|---|
| user | `settings.json`（cowork 模式 `cowork_settings.json`） | `~/.ultrac/`（`ULTRAC_CONFIG_DIR` 可覆盖，见 `envUtils.ts:getUltracConfigHomeDir`） |
| project | `settings.json` | `<cwd>/.ultrac/` |
| local | `settings.local.json` | `<cwd>/.ultrac/`（gitignored） |
| policy | `managed-settings.json` + `managed-settings.d/*.json` | managed 目录（**未改前缀**，见 §7） |
| flag | 由 `--settings <file\|json>` 指定 | — |

```ts
// src/utils/settings/settings.ts:298
export function getRelativeSettingsFilePathForSource(source: 'projectSettings' | 'localSettings'): string {
  switch (source) {
    case 'projectSettings': return join('.ultrac', 'settings.json')
    case 'localSettings':   return join('.ultrac', 'settings.local.json')
  }
}
```

全局配置文件（`GlobalConfig`，含 MCP 的 user/local scope）路径由 `src/utils/env.ts:14` `getGlobalUltracFile()` 决定：

```ts
// src/utils/env.ts:14
export const getGlobalUltracFile = memoize((): string => {
  // 旧版 .config.json 兼容分支
  if (getFsImplementation().existsSync(join(getUltracConfigHomeDir(), '.config.json'))) {
    return join(getUltracConfigHomeDir(), '.config.json')
  }
  // 正式路径：~/.ultrac.json（或 -local-oauth / -staging-oauth 后缀变体）
  const filename = `.ultrac${fileSuffixForOauthConfig()}.json`
  return join(process.env.ULTRAC_CONFIG_DIR || homedir(), filename)
})
```

### 1.4 CLI flag 极早解析

`--settings` / `--setting-sources` 在 `src/main.tsx` 的 `eagerLoadSettings()`（约 `main.tsx:502`）中极早解析，
先于 `init()`，分别写入 `flagSettings` 源与 `allowedSettingSources`。

---

## 2. 模型配置

入口：`src/utils/model/model.ts`

- `getUserSpecifiedModelSetting()` —— `model.ts:61`
- `getMainLoopModel()` —— `model.ts:92`
- `getDefaultMainLoopModel()` —— `model.ts:206`（按订阅类型选默认：Max/TeamPremium → Opus，其余 → Sonnet）

### 2.1 解析优先级（从高到低）

```
① /model 命令覆盖（getMainLoopModelOverride，会话内）
② --model flag（启动时）
③ ANTHROPIC_MODEL 环境变量
④ settings.model 字段
⑤ 内置默认 getDefaultMainLoopModel()
```

```ts
// src/utils/model/model.ts:61
export function getUserSpecifiedModelSetting(): ModelSetting | undefined {
  let specifiedModel: ModelSetting | undefined
  const modelOverride = getMainLoopModelOverride()
  if (modelOverride !== undefined) {
    specifiedModel = modelOverride
  } else {
    const settings = getSettings_DEPRECATED() || {}
    specifiedModel = process.env.ANTHROPIC_MODEL || settings.model || undefined  // env 在使用点读
  }
  if (specifiedModel && !isModelAllowed(specifiedModel)) return undefined  // availableModels 白名单约束
  return specifiedModel
}

// src/utils/model/model.ts:92
export function getMainLoopModel(): ModelName {
  const model = getUserSpecifiedModelSetting()
  if (model !== undefined && model !== null) return parseUserSpecifiedModel(model)
  return getDefaultMainLoopModel()
}
```

### 2.2 相关环境变量与字段

- `ANTHROPIC_MODEL`：`model.ts:69` 直接读取（不在 settings 合并中）。
- `ANTHROPIC_BASE_URL`：`src/utils/model/providers.ts:25` `isFirstPartyAnthropicBaseUrl()` 判定；
  `getAPIProvider()`（`providers.ts:6`）区分 `firstParty / bedrock / vertex / foundry`（由 `CLAUDE_CODE_USE_BEDROCK` 等决定）。
- `ANTHROPIC_SMALL_FAST_MODEL` / `ANTHROPIC_DEFAULT_OPUS_MODEL` / `_SONNET_` / `_HAIKU_`：覆盖默认模型。
- settings 字段：`model`、`availableModels`（allowlist，由 `isModelAllowed()` 校验）、`modelOverrides`（如 Bedrock ARN 映射）。
  类型见 `src/utils/settings/types.ts:375`。

---

## 3. MCP 配置

入口：`src/services/mcp/config.ts:1071` `getClaudeCodeMcpConfigs()`。
MCP **不只读 settings**，还读 `<cwd>/.mcp.json` 与 `~/.ultrac.json`。

### 3.1 主流程

```ts
// src/services/mcp/config.ts:1071
export async function getClaudeCodeMcpConfigs(dynamicServers, extraDedupTargets) {
  const { servers: enterpriseServers } = getMcpConfigsByScope('enterprise')

  // 企业 managed-mcp.json 存在 → 独占，忽略其他所有源
  if (doesEnterpriseMcpConfigExist()) {
    const filtered = {}
    for (const [name, cfg] of Object.entries(enterpriseServers))
      if (isMcpServerAllowedByPolicy(name, cfg)) filtered[name] = cfg
    return { servers: filtered, errors: [] }
  }

  const { servers: userServers }    = mcpLocked ? noServers : getMcpConfigsByScope('user')
  const { servers: projectServers } = mcpLocked ? noServers : getMcpConfigsByScope('project')
  const { servers: localServers }   = mcpLocked ? noServers : getMcpConfigsByScope('local')
  // ... plugin MCP 服务器加载、内容去重 ...

  // 项目级 .mcp.json 只收"已批准"的服务器
  const approvedProjectServers = {}
  for (const [name, cfg] of Object.entries(projectServers))
    if (getProjectMcpServerStatus(name) === 'approved') approvedProjectServers[name] = cfg

  // 合并顺序：plugin < user < project(已批准) < local
  const configs = Object.assign({}, dedupedPluginServers, userServers, approvedProjectServers, localServers)

  // 最后统一按企业策略过滤（denylist 优先）
  const filtered = {}
  for (const [name, cfg] of Object.entries(configs))
    if (isMcpServerAllowedByPolicy(name, cfg)) filtered[name] = cfg
  return { servers: filtered, errors: mcpErrors }
}
```

### 3.2 各 scope 读取位置（`getMcpConfigsByScope`，`config.ts:888`）

| scope | 来源 |
|---|---|
| project | `<cwd>/.mcp.json`（**文件名未改**，不含 claude 字样），并从 cwd 向上遍历父目录，`Object.assign` 让更靠近 cwd 的覆盖父目录（`config.ts:909-961`） |
| user | `~/.ultrac.json` 的 `GlobalConfig.mcpServers`（`config.ts:962`） |
| local | `~/.ultrac.json` 内当前 project 条目的 `mcpServers`（`config.ts:979`） |
| enterprise | `<managed-dir>/managed-mcp.json`（`config.ts:996`，`getEnterpriseMcpFilePath()`） |

### 3.3 项目级 `.mcp.json` 的批准逻辑

`src/services/mcp/utils.ts:351` `getProjectMcpServerStatus()` —— 这也是"新装 MCP 要你点 approve"的来源：

```ts
// src/services/mcp/utils.ts:351
export function getProjectMcpServerStatus(serverName): 'approved' | 'rejected' | 'pending' {
  const settings = getSettings_DEPRECATED()
  if (settings?.disabledMcpjsonServers?.some(n => normalizeNameForMCP(n) === normalizeNameForMCP(serverName)))
    return 'rejected'
  if (settings?.enabledMcpjsonServers?.some(...) || settings?.enableAllProjectMcpServers)
    return 'approved'
  return 'pending'   // 否则弹窗让你批准
}
```

相关 settings 字段：`enabledMcpjsonServers` / `disabledMcpjsonServers` / `enableAllProjectMcpServers`。

### 3.4 企业 allow/deny 策略

- `isMcpServerDenied()`（`config.ts:364`）：denylist 优先。
- `isMcpServerAllowedByPolicy()`（`config.ts:417`）：denylist 命中直接拒绝；allowlist 未定义=全部允许，空数组=全部拒绝；stdio/remote 分别按 command/url/serverName 匹配。
- `shouldAllowManagedMcpServersOnly()`（`config.ts:1485`，`allowManagedMcpServersOnly` 开关）：开启后 allowlist 只从 `policySettings` 读。

---

## 4. 权限配置

入口：`src/utils/permissions/permissions.ts:473` `hasPermissionsToUseTool()`。
规则来自 `settings.permissions.{allow, deny, ask}`，由 `src/utils/permissions/permissionsLoader.ts:120` 收集。

### 4.1 规则收集

```ts
// src/utils/permissions/permissionsLoader.ts:120
export function loadAllPermissionRulesFromDisk(): PermissionRule[] {
  if (shouldAllowManagedPermissionRulesOnly())   // policySettings 开了此开关 → 只认 managed 规则
    return getPermissionRulesForSource('policySettings')
  const rules: PermissionRule[] = []
  for (const source of getEnabledSettingSources())   // 否则从所有启用的 source 收集
    rules.push(...getPermissionRulesForSource(source))
  return rules
}
```

`settings.permissions` 的 schema（`src/utils/settings/types.ts:42`）：

```ts
PermissionsSchema = z.object({
  allow: z.array(PermissionRuleSchema()).optional(),
  deny:  z.array(PermissionRuleSchema()).optional(),
  ask:   z.array(PermissionRuleSchema()).optional(),
  defaultMode: z.enum(PERMISSION_MODES).optional(),
  disableBypassPermissionsMode: z.enum(['disable']).optional(),
  additionalDirectories: z.array(z.string()).optional(),
}).passthrough()
```

### 4.2 判定管线

`permissions.ts:1158` `hasPermissionsToUseToolInner()`，按以下顺序短路：

```
1a. deny 规则命中整工具            → deny
1b. ask 规则命中整工具             → ask（沙箱可 auto-allow 的 Bash 除外）
1c. tool.checkPermissions(input)   → 工具自身判定（如 Bash 子命令规则）
1d. 工具自身 deny                  → deny
1e. requiresUserInteraction + ask  → ask（bypass 模式也拦不住）
1f. 内容级 ask 规则                → 即便 bypass 也 ask
1g. safetyCheck(.git/.ultrac/.vscode 等) → 即便 bypass 也 ask
2a. mode === bypassPermissions（或 plan + bypass 可用） → allow
2b. allow 规则命中整工具           → allow
3.  其余 passthrough               → ask
```

`hasPermissionsToUseTool()`（`permissions.ts:473`）在外部包装了 `dontAsk` 模式转换（ask → deny）与 auto 模式的 AI 分类器逻辑。

---

## 5. 整条链路图

```
              CLI flags (--settings / --setting-sources / --model)
                            │ 极早解析 (eagerLoadSettings, main.tsx)
                            ▼
     loadSettingsFromDisk() ── 读下列文件并按优先级 merge ──► SettingsJson (合并结果)
       settings.ts:645            user    ~/.ultrac/settings.json
                                   project <cwd>/.ultrac/settings.json
                                   local   <cwd>/.ultrac/settings.local.json
                                   flag    --settings 指定
                                   policy  managed-settings.json (remote>MDM>file>HKCU)
                            │
          ┌─────────────────┼───────────────────┬──────────────────────┐
          ▼                 ▼                   ▼                      ▼
     模型 model.ts      MCP config.ts       权限 permissions.ts     其它 (hooks/env...)
     getUserSpecified-  getClaudeCode-      hasPermissionsToUseTool
     ModelSetting()     McpConfigs()        loadAllPermissionRulesFromDisk()
       读 ANTHROPIC_MODEL  读 .mcp.json        读 settings.permissions.*
       + settings.model     + ~/.ultrac.json    {allow,deny,ask}
       + /model 覆盖         mcpServers         (policy 可锁定独占)
```

---

## 6. 关键文件索引

| 关注点 | 文件 | 关键符号 / 行号 |
|---|---|---|
| **配置根目录（用户级收口点）** | `src/utils/envUtils.ts` | `getUltracConfigHomeDir:7`、`getTeamsDir:16` |
| **全局配置文件路径** | `src/utils/env.ts` | `getGlobalUltracFile:14` |
| settings 合并核心 | `src/utils/settings/settings.ts` | `loadSettingsFromDisk:645`、`getInitialSettings:812` |
| 优先级顺序 | `src/utils/settings/constants.ts` | `SETTING_SOURCES:7`、`getEnabledSettingSources:159` |
| 文件路径解析 | `src/utils/settings/settings.ts` | `getSettingsFilePathForSource:274`、`getSettingsRootPathForSource:239` |
| managed 目录（未改前缀） | `src/utils/settings/managedPath.ts` | `getManagedFilePath:8` |
| 模型解析 | `src/utils/model/model.ts` | `getUserSpecifiedModelSetting:61`、`getMainLoopModel:92` |
| 模型 provider | `src/utils/model/providers.ts` | `getAPIProvider:6`、`isFirstPartyAnthropicBaseUrl:25` |
| MCP 主流程 | `src/services/mcp/config.ts` | `getClaudeCodeMcpConfigs:1071`、`getMcpConfigsByScope:888` |
| MCP 批准 | `src/services/mcp/utils.ts` | `getProjectMcpServerStatus:351` |
| 权限入口 | `src/utils/permissions/permissions.ts` | `hasPermissionsToUseTool:473`、`hasPermissionsToUseToolInner:1158` |
| 权限规则收集 | `src/utils/permissions/permissionsLoader.ts` | `loadAllPermissionRulesFromDisk:120` |
| 路径安全防护清单 | `src/utils/permissions/filesystem.ts` | `DANGEROUS_FILES:57`、`DANGEROUS_DIRECTORIES:74` |
| 类型定义 | `src/utils/settings/types.ts` | `SettingsJson:1104`、`PermissionsSchema:42` |

---

## 7. 本 fork 的前缀改造（claude → ultrac）

### 7.1 改了什么

| 项 | 改前 | 改后 |
|---|---|---|
| 用户配置/数据目录 | `~/.claude/` | `~/.ultrac/` |
| 项目配置目录 | `<cwd>/.claude/` | `<cwd>/.ultrac/` |
| 全局配置文件 | `~/.claude.json` | `~/.ultrac.json` |
| 配置目录环境变量 | `CLAUDE_CONFIG_DIR` | `ULTRAC_CONFIG_DIR` |
| 取根目录函数 | `getClaudeConfigHomeDir()` | `getUltracConfigHomeDir()` |
| 取全局配置路径函数 | `getGlobalClaudeFile()` | `getGlobalUltracFile()` |

**硬切换**：不再读取任何旧 `~/.claude` 路径，无 fallback。

受影响范围：`src/` 下 **165 个文件**（1:1 行替换，共 629 行）。
若要迁移旧环境，手工搬运即可：

```bash
cp -r ~/.claude/. ~/.ultrac/ && mv ~/.claude.json ~/.ultrac.json
```

### 7.2 有意保留 `claude` 字样的地方

改前缀时下列位置**必须保持原样**，否则会破坏外部契约：

| 位置 | 原因 |
|---|---|
| `.claude-plugin/`（`plugin.json`、`marketplace.json`） | 第三方插件市场的目录约定，改名即无法识别别人的市场 |
| managed 目录：`/etc/claude-code`、`C:\Program Files\ClaudeCode`、`/Library/Application Support/ClaudeCode` | 企业部署契约，改名会让既有 managed 策略失效 |
| `join(getManagedFilePath(), '.claude', ...)` | managed 目录下的子目录，随 managed 根一起保留 |
| `/home/claude/.claude/remote` | Claude Code Remote (CCR) 容器内固定路径，由服务端约定 |
| `CLAUDE.md` / `CLAUDE.local.md` | 本次只改目录不改文件名 |
| `CLAUDE_CODE_*`、`ANTHROPIC_*` 环境变量 | 涉及 215 个变量，且会破坏外部注入脚本 |
| `claude-3-5-*` 等 Bedrock 模型 ID、`com.anthropic.claude_code.*` OTel scope、`claudeAiOauth` 存储字段、`color="claude"` | 非路径类标识符 |

### 7.3 路径安全防护已同步

`src/utils/permissions/filesystem.ts` 的 `DANGEROUS_FILES` / `DANGEROUS_DIRECTORIES`
已从 `.claude.json` / `.claude` 改为 `.ultrac.json` / `.ultrac`，
`.ultrac/worktrees/` 的结构化例外分支同步更新。**若不同步，安全防护会失效。**
