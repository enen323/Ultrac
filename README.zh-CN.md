# Ultrac

[English](README.md) | **简体中文**

Ultrac 是一个桌面端 AI 编码 agent。它的引擎是一个静态 Go 二进制，以无头 RPC 方式
运行；Electron 壳提供聊天式界面，并把该二进制作为子进程来驱动。引擎读取你的代码库，
运行工具（bash、文件编辑/写入、read、ls、grep、find），调用语言模型，并循环执行
*思考 → 行动 → 观察*，直到任务完成。同时也提供 CLI 供直接使用。

## 长什么样

同一个引擎有两副面孔：

- **桌面端** —— 一个 Electron 壳，以无头 RPC 方式运行该二进制，呈现聊天式界面
  （代码在 `desktop/`）。这是使用 Ultrac 的主要方式。
- **CLI** —— 跑一条 prompt，在终端里看工具执行，或把事件日志以 JSON 行流输出。

## 安装

前置要求：**Go ≥ 1.26**，以及 `~/.ultrac/settings.json` 里的一份 provider 声明
（见「配置」）。

```bash
go build -o bin/ultrac ./cmd/ultrac
```

## 使用

```bash
# 单条 prompt —— agent 跑完工具后退出
./bin/ultrac -p "Read go.mod and say the module name in one sentence"

# 事件流，每行一个 JSON 对象
./bin/ultrac --mode json -p "List the Go files under internal/session"

# 恢复该目录下最近的会话
./bin/ultrac -c "now also count the test files"

# 无头 RPC 模式（stdin/stdout，供桌面端或你自己的客户端使用）
./bin/ultrac --mode rpc

# 诊断
./bin/ultrac config              # 配置层级 + provider/凭据摘要
./bin/ultrac sessions [dir]      # 列出某目录的会话
```

## 桌面端

```bash
make build                                       # 构建内核：bin/ultrac(.exe)
cd desktop/shell-spa && npm install && npm run build   # 构建页面 → dist/
cd ../shell && pnpm install && pnpm start             # 启动壳
```

壳会 spawn `ultrac --mode rpc`，通过子进程的 stdin/stdout 与它通信——没有 HTTP、
没有端口、没有 token。

## 配置

provider 凭据写在 `~/.ultrac/settings.json`。一个 section 让你**不改代码、不重编译**
接入新 provider——连同它的密钥：

```json
{
  "providers": {
    "my-proxy": {
      "baseUrl": "https://proxy.example.com/v1",
      "api": "openai-completions",
      "apiKey": "sk-my-proxy-key",
      "models": [
        { "id": "my-model", "name": "My Model", "contextWindow": 128000, "maxTokens": 8192 }
      ]
    }
  }
}
```

- `baseUrl` 写到 **API 根**（`/v1`）；适配器自己拼 `/chat/completions` 或
  `/v1/messages`。
- 只声明 `apiKey` 对内置 provider 也合法。
- 必须显式指定模型：`--model provider/id`，或沿用会话里记录的上一个模型。没有模型时
  CLI 报 `no model configured` 并退出。

## 结构

```
cmd/ultrac/      # CLI：flags、print 模式、RPC 模式、诊断
internal/        # 引擎：agent loop、provider、session、tools、transport
desktop/         # Electron 壳 + React SPA
```

## 许可

内部使用。所有商标归各自所有者。
