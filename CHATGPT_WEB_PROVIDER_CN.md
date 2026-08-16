# ChatGPT Web 原生 Provider 接入说明

本文说明 `chatgpt-web` Provider 在 CLIProxyAPIPlus 中的设计、认证文件、账号池、Linux 部署和 OpenAI 兼容接口。

> 此 Provider 使用 ChatGPT 网页私有协议，不是 OpenAI 官方 API。网页协议可能变化，只应使用你本人或已明确授权的账号。遇到 Arkose、Turnstile 等交互验证时，本实现会返回错误，不会绕过验证。

## 1. 整合方式

该实现完全使用仓库原有的 Go 技术栈，没有启动 Python 服务或旁路网关：

- `ChatGPTWebExecutor` 接入原有 Executor 接口；
- 每个账号仍是原 `auth-dir` 下的一份 JSON 文件；
- 复用原 Auth Manager 的多账号轮询、失败重试、冷却、禁用和热加载；
- 复用原来的全局代理和账号级 `proxy_url`；
- 复用现有 `/v1/chat/completions`、`/v1/images/generations`、模型注册、API Key 校验和错误输出；
- 私有协议集中在 `internal/runtime/executor/helps/chatgpt_web_*.go`，没有修改其他 Provider 的请求逻辑。

为避免与原有 Codex 图片模型冲突，本 Provider 使用两个独立模型名：

| 模型 | 接口 | 用途 |
| --- | --- | --- |
| `chatgpt-web` | `/v1/chat/completions` | 文字对话 |
| `chatgpt-web-image` | `/v1/images/generations` | 文生图 |

## 2. 账号认证文件

找到 `config.yaml` 中的 `auth-dir`。默认配置通常是：

```yaml
auth-dir: "~/.cli-proxy-api"
```

在该目录中为每个 ChatGPT 账号新建一份 JSON，例如 `chatgpt-web-account-01.json`：

```json
{
  "type": "chatgpt-web",
  "email": "account-01@example.com",
  "access_token": "YOUR_AUTHORIZED_CHATGPT_ACCESS_TOKEN",
  "proxy_url": "socks5://127.0.0.1:10808"
}
```

也可以只提供 Cookie，服务会通过 `/api/auth/session` 换取 access token：

```json
{
  "type": "chatgpt-web",
  "email": "account-02@example.com",
  "cookie": "YOUR_AUTHORIZED_CHATGPT_COOKIE"
}
```

规则：

- `type` 必须是 `chatgpt-web`；
- `access_token` 和 `cookie` 至少填写一个；优先使用 `access_token`；
- `email` 只是管理标签，不参与登录；
- `proxy_url` 可省略，省略后使用 `config.yaml` 的全局 `proxy-url`；
- 不要把真实 token、Cookie 或认证文件提交到 Git；Linux 上建议将文件权限设为 `600`。

认证文件还支持以下可选字段，通常无需修改：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `base_url` | `https://chatgpt.com` | ChatGPT Web 根地址 |
| `requirements_token` | 自动生成 | 可选 Sentinel 初始 token |
| `text_model` | `gpt-5-5` | 网页对话内部模型标识 |
| `image_model` | `auto` | 网页生图内部模型标识 |
| `text_bootstrap` | `false` | 文字请求前是否访问首页 |
| `text_prepare` | `false` | 文字是否调用 prepare |
| `image_bootstrap` | `true` | 生图请求前是否访问首页 |
| `image_prepare` | `true` | 生图是否调用 `f/conversation/prepare` |
| `requirements_v2` | `true` | 是否优先使用 Sentinel prepare/finalize |
| `poll_interval_seconds` | `6` | 图片状态轮询间隔 |
| `max_image_bytes` | `20971520` | 单张图片最大字节数 |
| `max_pow_iterations` | `500000` | Sentinel PoW 最大迭代次数 |

`user_agent`、`client_version` 和 `client_build_number` 也可按账号覆盖，但这些字段容易随网页更新而变化，只有确认协议漂移时才建议调整。

## 3. 多账号池

每个账号放一份 `type=chatgpt-web` 的 JSON 文件即可形成账号池，无需增加新的池配置。例如：

```text
~/.cli-proxy-api/
├── chatgpt-web-account-01.json
├── chatgpt-web-account-02.json
└── chatgpt-web-account-03.json
```

默认轮询配置：

```yaml
routing:
  strategy: round-robin
```

请求会由原 Auth Manager 在可用账号间选择。上游返回 `401`、`403`、`429` 或其他带状态码的错误时，也沿用原系统的重试、账号冷却和故障转移策略。可以继续使用原有的 `disabled`、`priority`、`prefix` 和账号级排除模型能力。

## 4. Linux 部署

### 4.1 Docker Compose

在 `dev` 分支工作区中构建本地代码：

```bash
cp config.example.yaml config.yaml
mkdir -p auths logs
docker compose up -d --build
docker compose logs -f cli-proxy-api
```

默认 Compose 映射关系：

- `./config.yaml` → `/CLIProxyAPI/config.yaml`
- `./auths` → `/root/.cli-proxy-api`
- `./logs` → `/CLIProxyAPI/logs`
- API 端口为 `8317`

因此认证文件可直接放入项目的 `auths/` 目录。更新文件后原 watcher 会自动重新加载；生产环境仍建议通过受控发布流程更新秘密文件。

### 4.2 原生 Go 构建

```bash
go build -o CLIProxyAPI ./cmd/server
cp config.example.yaml config.yaml
mkdir -p auths logs
./CLIProxyAPI --config ./config.yaml
```

如将 `auth-dir` 配置为 `./auths`，请在启动目录下放置账号 JSON。公网部署应在前面配置 HTTPS 反向代理，并保留足够长的读取超时，因为图片生成可能持续数分钟。

## 5. OpenAI 兼容接口

以下示例假设：

- 服务地址：`http://127.0.0.1:8317/v1`
- `config.yaml` 已配置客户端 API Key：`YOUR_GATEWAY_API_KEY`

### 5.1 模型列表

```bash
curl http://127.0.0.1:8317/v1/models \
  -H 'Authorization: Bearer YOUR_GATEWAY_API_KEY'
```

当至少一份 ChatGPT Web 认证文件可用时，结果中应包含 `chatgpt-web` 和 `chatgpt-web-image`。

### 5.2 文字对话

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_GATEWAY_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "chatgpt-web",
    "messages": [
      {"role": "system", "content": "你是一个简洁的中文助手"},
      {"role": "user", "content": "用三句话介绍香港"}
    ],
    "stream": false
  }'
```

`stream=true` 会返回标准 SSE 结构，但网页上游结果是在完成后一次性转换，不是逐 token 实时转发。多轮对话由客户端每次重新提交完整 `messages`，服务不持久化 conversation。

### 5.3 图片生成

```bash
curl http://127.0.0.1:8317/v1/images/generations \
  -H 'Authorization: Bearer YOUR_GATEWAY_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "chatgpt-web-image",
    "prompt": "雨夜里的香港电车，电影感，无文字",
    "size": "1536x1024",
    "quality": "high",
    "n": 1,
    "response_format": "b64_json"
  }'
```

当前支持：

- `n=1`；
- `1024x1024`、`1536x1024`、`1024x1536`；
- `response_format=b64_json`；请求 `url` 时由原图片处理器返回 data URL；
- 仅文生图，不支持 edit、mask、参考图和图片流式输出。

尺寸和 quality 会转换为网页生图提示意图，不保证上游最终像素严格等于请求值。

## 6. Python SDK 示例

```python
import base64
from openai import OpenAI

client = OpenAI(
    api_key="YOUR_GATEWAY_API_KEY",
    base_url="http://127.0.0.1:8317/v1",
)

chat = client.chat.completions.create(
    model="chatgpt-web",
    messages=[{"role": "user", "content": "你好"}],
)
print(chat.choices[0].message.content)

image = client.images.generate(
    model="chatgpt-web-image",
    prompt="雨后的竹林，薄雾，电影光线，无文字",
    size="1024x1024",
    n=1,
    response_format="b64_json",
)
with open("result.png", "wb") as file:
    file.write(base64.b64decode(image.data[0].b64_json))
```

## 7. 常见错误

| 错误码/状态 | 含义 | 建议 |
| --- | --- | --- |
| `credentials_missing` | 未提供 token 或 Cookie | 检查账号 JSON 字段和 `auth-dir` |
| `session_expired` | Cookie 无法换取 access token | 重新登录并更新自己的认证文件 |
| `upstream_auth_error` / 401 / 403 | 上游会话失效或账号要求验证 | 更新凭据，并在官网完成必要的交互验证 |
| `arkose_required` | 上游要求 Arkose | 本实现不会绕过，需在官方页面完成验证 |
| `requirements_missing` | Sentinel 协议或浏览器参数变化 | 检查协议版本和账号代理出口 |
| `conduit_missing` | 图片 prepare 没有返回 conduit token | 检查网页私有接口是否变化 |
| `image_asset_missing` | 对话完成但没有图片资源 | 检查 SSE 和 conversation JSON 格式 |
| `image_download_failed` | 图片资源下载失败 | 检查签名 URL、代理与域名限制 |
| `image_too_large` | 图片超过账号配置上限 | 在可控范围内提高 `max_image_bytes` |

日志和故障报告中不要输出 Authorization、Cookie、Sentinel token、完整签名图片 URL 或认证文件内容。

## 8. 维护边界

网页协议变化时，优先检查以下隔离文件：

- `internal/runtime/executor/helps/chatgpt_web_client.go`：认证、浏览器请求头、Sentinel 和 PoW；
- `internal/runtime/executor/helps/chatgpt_web_conversation.go`：文字、生图、SSE 和轮询；
- `internal/runtime/executor/helps/chatgpt_web_assets.go`：资源指针、重定向和图片下载；
- `internal/runtime/executor/chatgpt_web_executor.go`：OpenAI 请求/响应适配；
- `internal/registry/chatgpt_web_models.go`：模型注册。

修改私有协议后应运行相关单元测试、全量 `go test ./...`，并使用已授权账号分别完成一次文字和图片冒烟测试。关键生产业务应优先使用 OpenAI 官方 API。
