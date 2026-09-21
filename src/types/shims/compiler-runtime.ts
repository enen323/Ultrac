// TODO: Phase 6 - 还原 react/compiler-runtime 真实实现

/**
 * Create a memoization cache of the given size.
 * React Compiler compiled output reads/writes this cache via `$[n]` subscripts.
 *
 * @param size - Number of cache slots to allocate
 * @returns An array that can be used as a memoization cache
 */
export function c(size: number): unknown[] {
  return new Array(size)
}

/**
 * Hook-based memoization cache (provided for completeness;
 * current source only imports `c`).
 */
export function useMemoCache(size: number): unknown[] {
  return new Array(size)
}
