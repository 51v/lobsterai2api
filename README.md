# lobsterai2api

把有道龙虾（LobsterAI）的免费积分转换成 OpenAI 兼容 API 的桥接服务。

- 语言: Go（零外部依赖，纯标准库）
- 服务端口: `:8367`（可通过 config 或 `LB2A_LISTEN` 修改）

## 架构

```
client (OpenAI SDK)
   │ POST /v1/chat/completions (Bearer sk-lobster-local)
   ▼
server: pool 挑号（积分最高、健康）→ 检查 token → 转换 body → 转发上游
   ▼
upstream: UPSTREAM_BASE/api/proxy/v1/chat/completions
   ▼ 出错 → 分类 → 冷却/禁用 → 换号重试（最多 3 次）
```

## 构建

```bash
go build -o lobsterai2api.exe ./cmd/server
go build -o login.exe ./cmd/login
go build -o credit.exe ./cmd/credit
```

## 登录（新增账号）

```bash
./login.sh
# 或手动：
./login.exe url   # 打印登录 URL（本地回调服务器已就绪）
# 浏览器打开 URL → 手机号/微信登录
./login.exe poll  # 等待回调 → exchange → 落盘 auths/lobsterai-<uid>.json
```

## 运行

```bash
./lobsterai2api.exe -config config.json
```

## 积分查询

```bash
./credit.sh        # 人类可读
./credit.exe       # JSON 输出（供脚本消费）
```

## 测试

```bash
# 非流式
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer sk-lobster-local" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":false}'

# 流式
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer sk-lobster-local" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 模型列表
curl -s http://127.0.0.1:8367/v1/models -H "Authorization: Bearer sk-lobster-local"

# 状态
curl -s http://127.0.0.1:8367/status
```

## 配置

见 `config.json` / `config.example.json`。环境变量前缀 `LB2A_*`：

| 变量 | 含义 |
|---|---|
| `LB2A_LISTEN` | 监听地址 |
| `LB2A_API_KEY` | 本地鉴权 key |
| `LB2A_AUTH_DIR` | auth 文件目录 |
| `LB2A_STATE_FILE` | 池状态文件 |
| `LB2A_HARD_CREDIT` / `LB2A_SOFT_RATE` | 冷却时长 |
| `LB2A_ERR_THRESHOLD` / `LB2A_ERR_COOLDOWN` | 错误阈值与冷却 |
| `LB2A_TIMEOUT_SECONDS` | 上游超时 |

## 已知限制 / TODO

- 每日签到端点未知，`DailyCheckin` 目前是 no-op（抓包确定后实现）
- 上游 SSE 格式与 chat body 兼容性待真实账号验证（首次联调时 dump 原始响应）
- 动态模型列表来自 `GET /api/models/available`（缓存 1h，失败回退静态表）
