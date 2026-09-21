declare module '../daemon/workerRegistry.js';
declare module '../daemon/main.js';
declare module '../cli/bg.js';
declare module '../cli/handlers/templateJobs.js';
declare module '../environment-runner/main.js';
declare module '../self-hosted-runner/main.js';

declare module '@ant/claude-for-chrome-mcp' {
  export const BROWSER_TOOLS: unknown[]
  export interface ClaudeForChromeContext {}
  export interface Logger {}
  export interface PermissionMode {}
  export function createClaudeForChromeMcpServer(): Promise<unknown>
}

declare module '@ant/computer-use-mcp' {
  export const DEFAULT_GRANT_FLAGS: number
  export const API_RESIZE_PARAMS: unknown
  export function targetImageSize(): Promise<unknown>
  export function bindSessionContext(): Promise<unknown>
  export interface ComputerUseSessionContext {}
  export interface CuCallToolResult {}
  export interface CuPermissionRequest {}
  export interface CuPermissionResponse {}
  export interface ScreenshotDims {}
  export function buildComputerUseTools(): Promise<unknown[]>
  export function createComputerUseMcpServer(): Promise<unknown>
  export interface ComputerExecutor {}
  export interface DisplayGeometry {}
  export interface FrontmostApp {}
  export interface InstalledApp {}
  export interface ResolvePrepareCaptureResult {}
  export interface RunningApp {}
  export interface ScreenshotResult {}
}

declare module '@ant/computer-use-mcp/types' {
  export interface ComputerUseHostAdapter {}
  export interface Logger {}
  export interface CoordinateMode {}
  export interface CuSubGates {}
  export interface CuPermissionRequest {}
  export interface CuPermissionResponse {}
  export const DEFAULT_GRANT_FLAGS: number
}

declare module '@ant/computer-use-mcp/sentinelApps' {
  export function getSentinelCategory(): Promise<unknown>
}

declare module '@ant/computer-use-swift' {
  export interface ComputerUseAPI {}
}

declare module '@ant/computer-use-input' {
  export interface ComputerUseInput {}
  export interface ComputerUseInputAPI {}
}
