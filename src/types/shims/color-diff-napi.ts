// TODO: Phase 6 - 还原 color-diff-napi 真实实现
//
// 背景：npm 上的 `color-diff-napi` 是被 Anthropic 抢注的空壳包
//   （module.exports = {}，description "This package name has been reserved."，UNLICENSED）。
//   源码注释自述 "The TS port of color-diff works in all build modes" ——
//   原版是内部 TS 实现，包名虽带 -napi 但并非原生模块。
//
// 当前策略：本桩只为满足 ESM 静态导入（否则模块加载阶段直接 SyntaxError），
//   实际功能通过 CLAUDE_CODE_SYNTAX_HIGHLIGHT=0 走降级路径关闭。
//
// 两个消费方都已正确处理 null 返回值：
//   src/components/StructuredDiff.tsx:52  →  if (!ColorDiff) return null
//   src/components/HighlightedCode.tsx:44 →  if (!ColorFile) { t2 = null }
//
// 真实调用契约（从消费方反推，供 Phase 6 还原参考）：
//   new ColorDiff(patch, firstLine, filePath, fileContent).render(theme, width, dim)
//   new ColorFile(code, filePath)

export type SyntaxTheme = any

export class ColorDiff {
  constructor(..._args: any[]) {}

  /** 返回 null 表示不可用，调用方会据此降级 */
  render(..._args: any[]): null {
    return null
  }
}

export class ColorFile {
  constructor(..._args: any[]) {}

  /** 返回 null 表示不可用，调用方会据此降级 */
  render(..._args: any[]): null {
    return null
  }
}

export function getSyntaxTheme(_themeName: string): any {
  return undefined as any
}

export default undefined as any
