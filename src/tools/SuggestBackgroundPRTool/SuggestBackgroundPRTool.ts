// TODO: Phase 6 - 还原 SuggestBackgroundPRTool
//
// 注意：Tool 是 type 不是 class（见 src/Tool.ts:362），不能用 `extends Tool`。
// `import type` 在运行时不产生绑定，会在启动时抛 "ReferenceError: Tool is not defined"。

import { z } from 'zod/v4'

const inputSchema = z.object({}).passthrough()

export const SuggestBackgroundPRTool = {
  name: 'suggest_background_pr',
  aliases: [],
  inputSchema,
  async description() {
    return 'SuggestBackgroundPRTool is unavailable in this reconstructed build.'
  },
  async call() {
    return {
      data: {
        success: false,
        error: 'SuggestBackgroundPRTool is unavailable in this reconstructed build.',
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
