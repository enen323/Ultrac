# 功能桩注册表

**用途：** 核心路径完成后，按此清单逐个还原功能桩。
**创建时间：** 2026-09-19
**最后更新：** 2026-09-19

---

## 使用说明

每个桩条目包含：
- **标记方式**：代码中的 `// TODO: Phase N` 注释
- **还原标准**：明确的功能验收条件
- **依赖关系**：还原前需要完成的其他桩

---

## Phase 6 还原清单

### 6.1 遥测与监控

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 1 | OpenTelemetry 初始化 | `src/utils/telemetry/instrumentation.ts` | `// TODO: Phase 6` | 指标、日志、链路追踪正常上报 | 无 |
| 2 | 1P 事件日志 | `src/services/analytics/firstPartyEventLogger.ts` | `// TODO: Phase 6` | 事件正常批量上报 | 1 |
| 3 | gRPC 导出器 | `src/utils/telemetry/instrumentation.ts` | `// TODO: Phase 6` | gRPC 连接正常 | 1 |
| 4 | Beta session tracing | `src/utils/telemetry/betaSessionTracing.ts` | `// TODO: Phase 6` | Beta 用户链路追踪正常 | 1 |

**还原顺序：** 1 → 2 → 3 → 4

### 6.2 OAuth 与认证

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 5 | OAuth 客户端 | `src/services/oauth/client.ts` | `// TODO: Phase 5` | 登录流程正常，token 刷新正常 | 无 |
| 6 | 密钥链访问 | `src/utils/secureStorage/keychain.ts` | `// TODO: Phase 5` | 密钥安全存储/读取 | 5 |
| 7 | mTLS 配置 | `src/utils/mtls.ts` | `// TODO: Phase 6` | 双向 TLS 正常 | 无 |
| 8 | AWS/GCP 凭证预取 | `src/utils/auth.ts` | `// TODO: Phase 5` | BedRock/GCP 凭证预取正常 | 5 |

**还原顺序：** 5 → 6 → 8 → 7

### 6.3 网络与代理

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 9 | 全局 HTTP 代理 | `src/utils/proxy.ts` | `// TODO: Phase 6` | 代理配置生效，请求走代理 | 无 |
| 10 | CA 证书配置 | `src/utils/caCertsConfig.ts` | `// TODO: Phase 6` | 自定义 CA 证书生效 | 无 |
| 11 | Upstream proxy | `src/upstreamproxy/upstreamproxy.ts` | `// TODO: Phase 6` | CCR 环境代理中继正常 | 9 |

**还原顺序：** 9 → 10 → 11

### 6.4 远程设置与策略

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 12 | 远程托管设置 | `src/services/remoteManagedSettings/` | `// TODO: Phase 6` | 远程配置正常加载/刷新 | 无 |
| 13 | 策略限制 | `src/services/policyLimits/` | `// TODO: Phase 5` | 策略限制生效（如禁用远程控制） | 12 |
| 14 | GrowthBook 特性开关 | `src/services/analytics/growthbook.ts` | `// TODO: Phase 6` | 特性开关正常，灰度发布可用 | 无 |

**还原顺序：** 12 → 13 → 14

### 6.5 会话与会话存储

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 15 | 会话注册表 | `src/utils/sessionStorage.ts` | `// TODO: Phase 5` | 会话正常注册/查询/删除 | 无 |
| 16 | 会话恢复 | `src/utils/sessionStorage.ts` | `// TODO: Phase 5` | `claude --resume` 正常恢复 | 15 |
| 17 | Session Ingress | `src/services/api/sessionIngress.ts` | `// TODO: Phase 6` | 会话数据上传正常 | 15 |
| 18 | 文件下载 | `src/services/api/filesApi.ts` | `// TODO: Phase 6` | 文件下载/解析正常 | 无 |

**还原顺序：** 15 → 16 → 17 → 18

### 6.6 插件与 Skills

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 19 | 插件加载器 | `src/utils/plugins/pluginLoader.ts` | `// TODO: Phase 6` | 插件正常加载/初始化 | 无 |
| 20 | 插件 CLI 命令 | `src/services/plugins/pluginCliCommands.ts` | `// TODO: Phase 6` | `/plugin marketplace/list` 正常 | 19 |
| 21 | Skills 加载 | `src/skills/` | `// TODO: Phase 6` | 内置/自定义 Skills 正常加载 | 无 |
| 22 | Skill Change Detector | `src/utils/skills/skillChangeDetector.ts` | `// TODO: Phase 6` | Skills 变更自动检测 | 21 |

**还原顺序：** 21 → 22 → 19 → 20

### 6.7 工具实现（Phase 5-6 逐步补全）

| # | 工具 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 23 | AgentTool | `src/tools/AgentTool/` | `// TODO: Phase 5` | 子 Agent 创建/执行正常 | 5 |
| 24 | TaskCreateTool | `src/tools/TaskCreateTool/` | `// TODO: Phase 6` | 任务创建/管理正常 | 无 |
| 25 | TodoWriteTool | `src/tools/TodoWriteTool/` | `// TODO: Phase 6` | TODO 列表管理正常 | 无 |
| 26 | WebFetchTool | `src/tools/WebFetchTool/` | `// TODO: Phase 5` | URL 内容抓取正常 | 无 |
| 27 | WebSearchTool | `src/tools/WebSearchTool/` | `// TODO: Phase 5` | Web 搜索正常 | 无 |
| 28 | SkillTool | `src/tools/SkillTool/` | `// TODO: Phase 6` | Skill 调用正常 | 21 |
| 29 | MCPTool | `src/tools/MCPTool/` | `// TODO: Phase 6` | MCP 工具调用正常 | 无 |
| 30 | PowerShellTool | `src/tools/PowerShellTool/` | `// TODO: Phase 6` | Windows PowerShell 执行正常 | 无 |
| 31 | NotebookEditTool | `src/tools/NotebookEditTool/` | `// TODO: Phase 6` | Jupyter notebook 编辑正常 | 无 |
| 32 | SentinelHookTool | `src/tools/SentinelHookTool/` | `// TODO: Phase 6` | Hook 触发正常 | 无 |

**还原顺序：** 23 → 26 → 27 → 24 → 25 → 28 → 29 → 30 → 31 → 32

### 6.8 Daemon 与后台任务

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 33 | Daemon 主进程 | `src/daemon/main.ts` | `// TODO: Phase 6` | `claude daemon` 正常启动 | 无 |
| 34 | Daemon Worker | `src/daemon/workerRegistry.ts` | `// TODO: Phase 6` | Worker 进程正常 spawn/监控 | 33 |
| 35 | Background Tasks | `src/utils/backgroundTasks.ts` | `// TODO: Phase 6` | 后台任务正常调度 | 无 |

**还原顺序：** 33 → 34 → 35

### 6.9 Bridge 与 Remote Control

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 36 | Bridge Main | `src/bridge/bridgeMain.ts` | `// TODO: Phase 6` | `claude remote-control` 正常启动 | 5, 12, 14 |
| 37 | Bridge UI | `src/bridge/bridgeUI.ts` | `// TODO: Phase 6` | Bridge 登录 UI 正常渲染 | 36 |
| 38 | Bridge API | `src/bridge/bridgeApi.ts` | `// TODO: Phase 6` | Bridge API 调用正常 | 36 |
| 39 | JWT 工具 | `src/bridge/jwtUtils.ts` | `// TODO: Phase 6` | JWT 生成/验证正常 | 36 |
| 40 | Trusted Device | `src/bridge/trustedDevice.ts` | `// TODO: Phase 6` | 设备信任管理正常 | 39 |
| 41 | Poll Config | `src/bridge/pollConfig.ts` | `// TODO: Phase 6` | 轮询配置正常 | 36 |

**还原顺序：** 36 → 38 → 39 → 40 → 37 → 41

### 6.10 其他边缘功能

| # | 模块 | 文件 | 标记注释 | 还原标准 | 依赖 |
|---|------|------|----------|----------|------|
| 42 | Voice 输入 | `src/voice/` | `// TODO: Phase 6` | 语音输入正常 | 无 |
| 43 | Computer Use | `src/utils/computerUse/` | `// TODO: Phase 6` | 计算机控制正常 | 无 |
| 44 | Chrome Integration | `src/utils/claudeInChrome/` | `// TODO: Phase 6` | Chrome MCP 正常 | 无 |
| 45 | Environment Runner | `src/environment-runner/` | `// TODO: Phase 6` | BYOC runner 正常 | 无 |
| 46 | Self-Hosted Runner | `src/self-hosted-runner/` | `// TODO: Phase 6` | 自托管 runner 正常 | 无 |
| 47 | Coordinator Mode | `src/coordinator/` | `// TODO: Phase 6` | 多 Agent 协调正常 | 23 |
| 48 | Assistant (KAIROS) | `src/assistant/` | `// TODO: Phase 6` | Assistant 模式正常 | 无 |
| 49 | Buddy Sprite | `src/buddy/` | `// TODO: Phase 6` | Buddy 动画正常 | 无 |
| 50 | LSP Manager | `src/services/lsp/` | `// TODO: Phase 6` | LSP 服务正常 | 无 |
| 51 | Analytics Sinks | `src/utils/sinks.ts` | `// TODO: Phase 6` | 遥测 sink 正常 | 1 |
| 52 | Startup Profiler | `src/utils/startupProfiler.ts` | `// TODO: Phase 6` | 启动性能分析正常 | 无 |
| 53 | MDM 读取 | `src/utils/settings/mdm/rawRead.ts` | `// TODO: Phase 6` | MDM 配置读取正常 | 12 |
| 54 | Keychain 预取 | `src/utils/secureStorage/keychainPrefetch.ts` | `// TODO: Phase 6` | 密钥预取正常 | 6 |
| 55 | 剪贴板 | `src/utils/clipboard.ts` | `// TODO: Phase 6` | 剪贴板读写正常 | 无 |
| 56 | 编辑器集成 | `src/utils/editor.ts` | `// TODO: Phase 6` | 外部编辑器正常 | 无 |
| 57 | 防休眠 | `src/services/preventSleep.ts` | `// TODO: Phase 6` | 系统防休眠正常 | 无 |
| 58 | 通知 | `src/services/notifier.ts` | `// TODO: Phase 6` | 系统通知正常 | 无 |
| 59 | 插件 Marketplace | `src/utils/plugins/marketplaceManager.ts` | `// TODO: Phase 6` | 插件市场正常 | 19 |
| 60 | Swarm / 多 Agent | `src/utils/swarm/` | `// TODO: Phase 6` | 多 Agent 协作正常 | 23, 47 |
| 61 | Git 操作 | `src/utils/git/` | `// TODO: Phase 5` | Git 操作正常（diff/blame） | 无 |
| 62 | GitHub API | `src/utils/github/` | `// TODO: Phase 6` | GitHub API 调用正常 | 5 |
| 63 | Settings Sync | `src/services/settingsSync/` | `// TODO: Phase 6` | 设置同步正常 | 12 |
| 64 | Team Memory Sync | `src/services/teamMemorySync/` | `// TODO: Phase 6` | Team memory 同步正常 | 无 |
| 65 | Compact (上下文压缩) | `src/services/compact/` | `// TODO: Phase 6` | 上下文压缩正常 | 无 |
| 66 | Session Memory | `src/services/SessionMemory/` | `// TODO: Phase 6` | Session memory 正常 | 15 |
| 67 | Extract Memories | `src/services/extractMemories/` | `// TODO: Phase 6` | 记忆提取正常 | 无 |
| 68 | Native Installer | `src/utils/nativeInstaller/` | `// TODO: Phase 6` | 原生安装器正常 | 无 |
| 69 | Update Checker | `src/cli/update.ts` | `// TODO: Phase 6` | 自动更新检查正常 | 无 |

---

## 快速索引

**Phase 5 还原（12 个）：**
- #5 OAuth 客户端
- #6 密钥链访问
- #8 AWS/GCP 凭证预取
- #13 策略限制
- #15 会话注册表
- #16 会话恢复
- #23 AgentTool
- #26 WebFetchTool
- #27 WebSearchTool
- #61 Git 操作

**Phase 6 还原（57 个）：**
- 遥测：1-4
- 网络：7, 9-11
- 远程设置：12, 14
- 会话：#17-18
- 插件/Skills：19-22, 28-29
- 工具：#24-25, #30-32
- Daemon：33-35
- Bridge：36-41
- 其他：42-50, 51-69

---

## 还原流程

1. **确认核心路径完成**：Phase 1-5 全部验证通过
2. **按 Phase 5 → Phase 6 顺序还原**
3. **每个桩还原后：**
   - 删除 `// TODO: Phase N` 注释
   - 实现真实逻辑
   - 运行对应测试/验证
   - 更新本表状态（标记为 ✅）
4. **还原完成后：** 本表归档至 `docs/superpowers/archive/stubs-restored-YYYY-MM-DD.md`

---

## 备注

- 某些模块（如 #15 会话注册表、#16 会话恢复）依赖同一个文件 `src/utils/sessionStorage.ts`，需一次性还原
- 某些模块（如 #23 AgentTool、#47 Coordinator Mode）是其他模块的前置依赖，需优先还原
- 标记为 `Phase 5` 的桩应在 Phase 5 期间还原，而非推迟到 Phase 6
