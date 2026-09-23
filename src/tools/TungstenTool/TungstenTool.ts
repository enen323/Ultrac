// TODO: Phase 6 - 还原 TungstenTool
//
// 注意：Tool 是 type 不是 class（见 src/Tool.ts:362），不能用 `extends Tool`。
// 用 `import type { Tool }` + `class X extends Tool` 时，类型导入在运行时不产生
// 绑定，会在启动时抛 "ReferenceError: Tool is not defined"（已实测）。
// 真实工具同样是对象字面量形态（见 src/tools/BashTool/BashTool.tsx）。

import { z } from 'zod/v4'

const inputSchema = z.object({}).passthrough()

export const TungstenTool = {
  name: 'tungsten',
  aliases: [],
  inputSchema,
  async description() {
    return 'TungstenTool is unavailable in this reconstructed build.'
  },
  async call() {
    return {
      data: {
        success: false,
        error: 'TungstenTool is unavailable in this reconstructed build.',
      },
    }
  },
  isConcurrencySafe() {
    return true
  },
  isEnabled() {
    return false
  },
  isReadOnly() {
    return true
  },
}

// 调用点：src/commands/clear/caches.ts:96（同步调用，勿改成 async）
export function clearSessionsWithTungstenUsage(): void {}

export function resetInitializationState(): void {}
