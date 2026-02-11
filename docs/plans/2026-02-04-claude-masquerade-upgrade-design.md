# Claude 伪装功能升级设计方案

> 创建时间：2026-02-04
>
> 参考文档：`/usr/src/workspace/github/QQhuxuhui/new-api/docs/CLIProxyAPI与new-api伪装实现对比分析.md`

---

## 1. 概述

本次升级旨在将 CLIProxyAPIPlus 项目的 Claude 伪装功能提升到与 new-api 同等水平，并根据真实 Claude CLI 2.1.29 抓包数据进行精确校准。

### 1.1 升级目标

- 更新请求头至最新 Claude CLI 2.1.29 标准
- 实现会话池管理，替代当前的随机 User ID 生成
- 添加伪装追踪记录功能
- 实现 TLS 指纹精确控制

### 1.2 优先级

| 优先级 | 功能 | 风险降低 |
|--------|------|----------|
| P0 | 请求头更新 | 消除明显的版本不一致检测点 |
| P0 | 会话池管理 | 消除每次请求随机身份的异常行为 |
| P1 | 追踪记录 | 提供调试和问题排查能力 |
| P1 | TLS 指纹 | 消除传输层指纹不匹配风险 |

### 1.3 已有功能（保持不变）

当前项目已具备以下功能，本次升级不需要修改：

- **系统提示词注入**：`checkSystemInstructions()` 和 `checkSystemInstructionsWithMode()`
- **敏感词混淆**：`cloak_obfuscate.go` 中的零宽空格混淆
- **伪装模式配置**：`cloak.mode` 支持 auto/always/never

---

## 2. 请求头更新

### 2.1 变更清单

**文件**：`internal/runtime/executor/claude_executor.go`

**函数**：`applyClaudeHeaders()`

| 请求头 | 当前值 | 目标值 | 操作 |
|--------|--------|--------|------|
| User-Agent | `claude-cli/1.0.83 (external, cli)` | `claude-cli/2.1.29 (external, cli)` | 更新 |
| X-Stainless-Runtime-Version | `v24.3.0` | `v24.13.0` | 更新 |
| X-Stainless-Package-Version | `0.55.1` | `0.70.0` | 更新 |
| X-Stainless-Arch | `arm64` | `x64` | 更新 |
| X-Stainless-Os | `MacOS` | `Linux` | 更新 |
| X-Stainless-Timeout | `60` | `600` | 更新 |
| X-Stainless-Helper-Method | `stream` | (删除) | **删除** |
| Accept-Encoding | `gzip, deflate, br, zstd` | `gzip, br` | 更新 |

### 2.2 Anthropic-Beta 变更

**当前值**：
```
claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14
```

**目标值**：
```
claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05
```

### 2.3 Anthropic-Beta 处理策略

**决策**：使用固定 baseBetas，仅合并请求体中的 extraBetas，**删除 HTTP 请求头透传**。

理由：
- 保证核心伪装特征固定一致
- 请求体中的 `betas` 字段是客户端明确指定的额外功能需求，应该支持
- HTTP 请求头透传可能引入不可控的值，影响伪装一致性

### 2.4 实现代码

```go
func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string) {
    // ... 认证头设置 ...

    // Anthropic-Beta 固定值（不再从 ginHeaders 透传）
    baseBetas := "claude-code-20250219,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05"

    // 仅合并请求体中的 extraBetas（来自 betas 字段）
    if len(extraBetas) > 0 {
        existingSet := make(map[string]bool)
        for _, b := range strings.Split(baseBetas, ",") {
            existingSet[strings.TrimSpace(b)] = true
        }
        for _, beta := range extraBetas {
            beta = strings.TrimSpace(beta)
            if beta != "" && !existingSet[beta] {
                baseBetas += "," + beta
                existingSet[beta] = true
            }
        }
    }
    r.Header.Set("Anthropic-Beta", baseBetas)

    // Stainless SDK 特征头（8个，删除 Helper-Method）
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Lang", "js")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime", "node")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime-Version", "v24.13.0")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Os", "Linux")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Arch", "x64")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Package-Version", "0.70.0")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Retry-Count", "0")
    misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Timeout", "600")

    // User-Agent
    misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", "claude-cli/2.1.29 (external, cli)")

    // 其他头
    r.Header.Set("Accept-Encoding", "gzip, br")
    // ... 其余不变 ...
}
```

---

## 3. 会话池管理

### 3.1 设计目标

替代当前每次请求生成随机 User ID 的行为，实现：
- 每个认证凭证维护独立的会话池
- 会话 UUID 定期轮换，Hash 部分保持稳定
- 一致性哈希确保相同 API Key 映射到相同会话

### 3.2 User ID 格式语义

```
user_[64-hex-hash]_account__session_[uuid-v4]
     ↑                              ↑
     账户标识（固定）                会话标识（轮换）
```

### 3.3 Hash 部分来源优先级

1. **优先**：从下游客户端首次请求的 `metadata.user_id` 中提取
2. **后备**：使用 channel hash（基于 API Key 的 SHA256 前 64 位）

### 3.4 文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/runtime/executor/session_pool.go` | 新建 | 会话池核心实现 |
| `internal/runtime/executor/cloak_utils.go` | 修改 | 集成会话池 |
| `internal/config/config.go` | 修改 | 添加配置项 |

### 3.5 核心结构

```go
// session_pool.go

const (
    defaultMaxSessions       = 5
    defaultRotationInterval  = 6 * time.Hour
    defaultGracePeriod       = 5 * time.Minute
)

type SessionEntry struct {
    UUID      string
    CreatedAt time.Time
    ActiveAt  time.Time  // 激活时间（软轮换用）
    RetireAt  time.Time  // 退役时间（软轮换用）
}

type AuthSessionPool struct {
    authID           string           // 认证 ID
    hashPart         string           // user_id 的 hash 部分
    hashSource       string           // "client" 或 "channel"
    sessions         []SessionEntry
    maxSessions      int
    rotationInterval time.Duration
    lastRotation     time.Time
    mu               sync.RWMutex
}

type SessionPoolManager struct {
    pools            map[string]*AuthSessionPool  // key = auth ID
    defaultMax       int
    rotationInterval time.Duration
    mu               sync.RWMutex
}
```

### 3.6 Hash 提取逻辑

```go
// 从 user_id 提取 hash 部分
func extractHashFromUserID(userID string) (string, bool) {
    if !strings.HasPrefix(userID, "user_") {
        return "", false
    }
    parts := strings.Split(userID, "_account__session_")
    if len(parts) != 2 {
        return "", false
    }
    hashPart := strings.TrimPrefix(parts[0], "user_")
    if len(hashPart) != 64 || !isHexString(hashPart) {
        return "", false
    }
    return hashPart, true
}

// 生成 channel hash（后备方案）
func generateChannelHash(apiKey string) string {
    sum := sha256.Sum256([]byte(apiKey))
    return hex.EncodeToString(sum[:32])  // 64 字符
}
```

### 3.7 会话选择策略

```go
// 一致性哈希选择（相同 API Key 映射到相同会话）
func (p *AuthSessionPool) SelectSessionByKey(apiKey string, now time.Time) string {
    active := p.getActiveSessions(now)
    if len(active) <= 1 {
        return active[0] // 或默认值
    }

    targetHash := hashToUint64(apiKey)
    // 使用虚拟节点 + 最近邻算法
    // ...
}

// 加权随机选择（第一个会话概率最高）
func selectWeightedSession(sessions []string) string {
    n := len(sessions)
    totalWeight := n * (n + 1) / 2  // N + (N-1) + ... + 1
    pick := cryptoRandIntn(totalWeight)

    cumulative := 0
    for i := 0; i < n; i++ {
        cumulative += (n - i)  // 权重递减
        if pick < cumulative {
            return sessions[i]
        }
    }
    return sessions[n-1]
}
```

### 3.8 软轮换机制

```go
func (p *AuthSessionPool) rotateOldestSession(now time.Time) {
    // 1. 找到最旧的活跃会话
    // 2. 标记其在 gracePeriod 后退役
    p.sessions[oldestIdx].RetireAt = now.Add(defaultGracePeriod)

    // 3. 创建新会话，在旧会话退役后才激活
    newSession := SessionEntry{
        UUID:      generateRandomUUID(),
        CreatedAt: now,
        ActiveAt:  now.Add(defaultGracePeriod),
    }
    p.sessions = append(p.sessions, newSession)
}
```

### 3.9 配置项

```yaml
claude-api-key:
  - api-key: "sk-xxx"
    cloak:
      mode: "auto"
      max-sessions: 5           # 会话池大小
      rotation-interval: "6h"   # 轮换间隔
```

---

## 4. 追踪记录

### 4.1 设计目标

实现原始请求与伪装后请求的对比追踪，便于调试和问题排查。

### 4.2 文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/runtime/executor/masquerade_trace.go` | 新建 | 追踪存储实现 |
| `internal/runtime/executor/claude_executor.go` | 修改 | 采集并写入追踪 |
| `internal/api/masquerade_trace_handler.go` | 新建 | API 接口（可选） |

### 4.3 核心结构

```go
// masquerade_trace.go

const MaxTraceRecords = 100

type MasqueradeTraceRecord struct {
    ID              string            `json:"id"`
    Timestamp       int64             `json:"timestamp"`
    Model           string            `json:"model"`
    AuthID          string            `json:"auth_id"`
    AuthLabel       string            `json:"auth_label"`

    // 原始请求
    OriginalHeaders map[string]string `json:"original_headers"`
    OriginalBody    string            `json:"original_body"`

    // 伪装后请求
    MaskedHeaders   map[string]string `json:"masked_headers"`
    MaskedBody      string            `json:"masked_body"`

    // User ID 对比
    OriginalUserID  string            `json:"original_user_id"`
    MaskedUserID    string            `json:"masked_user_id"`
    OriginalSession string            `json:"original_session"`
    MaskedSession   string            `json:"masked_session"`
}

type MasqueradeTraceStore struct {
    records [MaxTraceRecords]*MasqueradeTraceRecord
    index   int
    count   int
    mutex   sync.RWMutex
}
```

### 4.4 API 接口

| 接口 | 方法 | 说明 |
|------|------|------|
| `/api/masquerade/traces` | GET | 获取追踪列表（轻量摘要） |
| `/api/masquerade/traces/:id` | GET | 获取单条完整记录 |
| `/api/masquerade/traces` | DELETE | 清空所有记录 |

### 4.5 配置项

```yaml
masquerade-trace:
  enable: true
  max-records: 100
```

---

## 5. TLS 指纹升级

### 5.1 设计目标

引入 utls 库，实现 Node.js v24 TLS 指纹模拟，确保传输层特征与请求头版本一致。

### 5.2 依赖添加

```go
// go.mod
github.com/refraction-networking/utls v1.8.1
golang.org/x/net/http2
```

### 5.3 文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/runtime/executor/tls_fingerprint.go` | 新建 | Node.js v24 指纹定义 |
| `internal/runtime/executor/utls_transport.go` | 新建 | 自定义 TLS Transport |
| `internal/runtime/executor/proxy_helpers.go` | 修改 | 集成 utls Transport |

### 5.4 指纹定义

```go
// tls_fingerprint.go

// Node.js v24.13.0 TLS 指纹
//
// 重要：以下为基于 Node.js v22 的初始值，需要通过抓包验证并更新到 v24
// 研究方法：
// 1. 安装 Node.js v24.13.0
// 2. 使用 Wireshark 抓取 TLS 握手
// 3. 分析 ClientHello 中的密码套件、扩展顺序
// 4. 使用 ja3er.com 验证 JA3 指纹
//
// TODO: 验证并更新以下值为 Node.js v24.13.0 精确指纹

var nodeJS24CipherSuites = []uint16{
    4866, 4867, 4865, 49199, 49195, 49200, 49196, 158, 49191, 103,
    49192, 107, 163, 159, 52393, 52392, 52394, 49327, 49325, 49315,
    49311, 49245, 49249, 49239, 49235, 162, 49326, 49324, 49314, 49310,
    49244, 49248, 49238, 49234, 49188, 106, 49187, 64, 49162, 49172,
    57, 56, 49161, 49171, 51, 50, 157, 49313, 49309, 49233,
    156, 49312, 49308, 49232, 61, 60, 53, 47, 255,
}

var nodeJS24SupportedGroups = []tls.CurveID{
    tls.X25519, tls.CurveP256, tls.CurveID(30), tls.CurveP521, tls.CurveP384,
    tls.CurveID(256), tls.CurveID(257), tls.CurveID(258), tls.CurveID(259), tls.CurveID(260),
}

var nodeJS24PointFormats = []byte{0, 1, 2}

func newNodeJS24ClientHelloSpec(serverName string) *tls.ClientHelloSpec {
    return &tls.ClientHelloSpec{
        TLSVersMin:         tls.VersionTLS12,
        TLSVersMax:         tls.VersionTLS13,
        CipherSuites:       append([]uint16(nil), nodeJS24CipherSuites...),
        CompressionMethods: []byte{0},
        Extensions: []tls.TLSExtension{
            &tls.SNIExtension{ServerName: serverName},
            &tls.SupportedPointsExtension{SupportedPoints: append([]byte(nil), nodeJS24PointFormats...)},
            &tls.SupportedCurvesExtension{Curves: append([]tls.CurveID(nil), nodeJS24SupportedGroups...)},
            &tls.SessionTicketExtension{},
            &tls.GenericExtension{Id: 22}, // encrypt_then_mac
            &tls.ExtendedMasterSecretExtension{},
            &tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: defaultSignatureSchemes()},
            &tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
            &tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
            &tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
        },
    }
}
```

### 5.5 Transport 实现

```go
// utls_transport.go

type utlsRoundTripper struct {
    serverName   string
    proxyDialer  proxy.Dialer
    connections  map[string]*http2.ClientConn
    mu           sync.Mutex
}

func (rt *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
    // 1. 获取或创建连接
    conn, err := rt.getOrCreateConn(req.URL.Host)
    if err != nil {
        return nil, err
    }

    // 2. 使用 HTTP/2 发送请求
    return conn.RoundTrip(req)
}

func (rt *utlsRoundTripper) dialTLS(addr string) (net.Conn, error) {
    // 1. TCP 连接（可能通过代理）
    var conn net.Conn
    if rt.proxyDialer != nil {
        conn, _ = rt.proxyDialer.Dial("tcp", addr)
    } else {
        conn, _ = net.Dial("tcp", addr)
    }

    // 2. utls 握手
    tlsConn := tls.UClient(conn, &tls.Config{ServerName: rt.serverName}, tls.HelloCustom)
    spec := newNodeJS24ClientHelloSpec(rt.serverName)
    tlsConn.ApplyPreset(spec)
    tlsConn.Handshake()

    return tlsConn, nil
}
```

### 5.6 研究任务

**目标**：验证并更新 TLS 指纹为 Node.js v24.13.0 精确值。

**研究步骤**：

1. 安装 Node.js v24.13.0
   ```bash
   nvm install 24.13.0
   nvm use 24.13.0
   ```

2. 创建测试脚本抓取 TLS 握手
   ```javascript
   const https = require('https');
   https.get('https://api.anthropic.com', (res) => {
     console.log('Status:', res.statusCode);
   });
   ```

3. 使用 Wireshark 捕获 ClientHello
   - 过滤器: `tcp.port == 443 and ssl.handshake.type == 1`
   - 提取密码套件列表和顺序
   - 提取 TLS 扩展列表和顺序

4. 使用 ja3er.com 验证 JA3 指纹
   ```bash
   node -e "require('https').get('https://ja3er.com/json',(r)=>{let d='';r.on('data',c=>d+=c);r.on('end',()=>console.log(d))})"
   ```

5. 更新 `tls_fingerprint.go` 中的常量值

**当前状态**：使用 Node.js v22 基础值，待验证更新。

---

## 6. 实施计划

### 阶段一：请求头更新（P0）

1. 修改 `applyClaudeHeaders()` 函数
2. 更新所有请求头值
3. 删除 `X-Stainless-Helper-Method`
4. 测试验证

### 阶段二：会话池管理（P0）

1. 新建 `session_pool.go`
2. 修改 `cloak_utils.go` 集成会话池
3. 更新配置结构
4. 测试验证

### 阶段三：追踪记录（P1）

1. 新建 `masquerade_trace.go`
2. 修改 `claude_executor.go` 采集数据
3. 添加 API 接口（可选）
4. 测试验证

### 阶段四：TLS 指纹（P1）

1. 添加 utls 依赖
2. 新建 `tls_fingerprint.go`
3. 新建 `utls_transport.go`
4. 修改 `proxy_helpers.go`
5. 研究 Node.js v24 精确指纹
6. 测试验证

---

## 7. 文件变更汇总

| 文件 | 操作 | 阶段 |
|------|------|------|
| `internal/runtime/executor/claude_executor.go` | 修改 | 1, 3 |
| `internal/runtime/executor/session_pool.go` | 新建 | 2 |
| `internal/runtime/executor/cloak_utils.go` | 修改 | 2 |
| `internal/config/config.go` | 修改 | 2, 3 |
| `internal/runtime/executor/masquerade_trace.go` | 新建 | 3 |
| `internal/api/masquerade_trace_handler.go` | 新建 | 3 |
| `internal/runtime/executor/tls_fingerprint.go` | 新建 | 4 |
| `internal/runtime/executor/utls_transport.go` | 新建 | 4 |
| `internal/runtime/executor/proxy_helpers.go` | 修改 | 4 |
| `go.mod` | 修改 | 4 |

---

## 8. 验证检查清单

### 请求头验证

- [ ] User-Agent 为 `claude-cli/2.1.29 (external, cli)`
- [ ] X-Stainless-Runtime-Version 为 `v24.13.0`
- [ ] X-Stainless-Package-Version 为 `0.70.0`
- [ ] X-Stainless-Timeout 为 `600`
- [ ] 无 X-Stainless-Helper-Method
- [ ] Accept-Encoding 为 `gzip, br`
- [ ] Anthropic-Beta 包含 `prompt-caching-scope-2026-01-05`

### 会话池验证

- [ ] 相同 API Key 映射到相同会话 UUID
- [ ] Hash 部分优先从客户端请求提取
- [ ] 会话每 6 小时软轮换
- [ ] 任意时刻活跃会话数不超过配置值

### 追踪记录验证

- [ ] 记录保存不超过 100 条
- [ ] 可查询原始/伪装对比
- [ ] 环形缓冲正确覆盖旧记录

### TLS 指纹验证

- [ ] JA3 指纹与 Node.js v24.13.0 一致（需抓包验证）
- [ ] 代理模式下指纹正常
- [ ] HTTP/2 连接复用正常
- [ ] X-Stainless-Runtime-Version 与 TLS 指纹版本一致（均为 v24）

---

> 文档版本: 1.0
> 最后更新: 2026-02-04
