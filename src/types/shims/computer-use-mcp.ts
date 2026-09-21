// TODO: Phase 6 - 还原 @ant/computer-use-mcp 真实实现

import type {
  ComputerUseHostAdapter,
  CoordinateMode,
  CuSubGates,
  CuPermissionRequest,
  CuPermissionResponse,
  Logger,
} from './computer-use-mcp-types.js'

/** Default grant flags for computer-use permission requests */
export const DEFAULT_GRANT_FLAGS = 0

/** API resize parameters */
export const API_RESIZE_PARAMS: unknown = {}

/** Target image size for screenshots */
export async function targetImageSize(): Promise<unknown> {
  return {}
}

/** Bind per-session context into the computer-use tool call pipeline */
export async function bindSessionContext(): Promise<unknown> {
  return {}
}

/** Session context for computer-use operations */
export interface ComputerUseSessionContext {
  // stub
}

/** Result of a computer-use tool call */
export interface CuCallToolResult {
  // stub
}

/** Computer-use permission request */
export interface CuPermissionRequest {
  // stub
}

/** Computer-use permission response */
export interface CuPermissionResponse {
  // stub
}

/** Screenshot dimensions */
export interface ScreenshotDims {
  // stub
}

/** Build the set of computer-use tools */
export async function buildComputerUseTools(): Promise<unknown[]> {
  return []
}

/** Create the computer-use MCP server */
export async function createComputerUseMcpServer(): Promise<unknown> {
  return {}
}

/** CLI executor interface */
export interface ComputerExecutor {
  // stub
}

/** Display geometry info */
export interface DisplayGeometry {
  // stub
}

/** Frontmost app info */
export interface FrontmostApp {
  // stub
}

/** Installed app info */
export interface InstalledApp {
  // stub
}

/** Result of resolving prepare-capture */
export interface ResolvePrepareCaptureResult {
  // stub
}

/** Running app info */
export interface RunningApp {
  // stub
}

/** Screenshot result */
export interface ScreenshotResult {
  // stub
}
