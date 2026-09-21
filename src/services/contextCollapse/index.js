// TODO: Phase 6 - 还原 contextCollapse 模块
// 当前桩：提供 getStats、subscribe、isContextCollapseEnabled 的最小实现

export function getStats() {
  return {
    collapsedSpans: 0,
    collapsedMessages: 0,
    stagedSpans: 0,
    health: {
      emptySpawnWarningEmitted: false,
      totalErrors: 0,
      totalSpawns: 0,
      totalEmptySpawns: 0,
      lastError: undefined,
    },
  };
}

export function subscribe(callback) {
  // Minimal stub: return unsubscribe function
  return () => {};
}

export function isContextCollapseEnabled() {
  return false;
}
