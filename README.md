# lobsterai2api

网易有道 LobsterAI 的 OpenAI 兼容 API 反代 —— 多账号池、按积分负载均衡、SSE 流式、令牌自动刷新。

**本仓库为二次开发版**，在上游 [xinxinshuhao-create/lobsterai2api](https://github.com/xinxinshuhao-create/lobsterai2api) 基础上新增：

- **内置 Web 控制台（`/console`）**：页面化添加账号（OAuth 授权链接生成 + 回调 URL 回填）、账号池数据展示（积分/状态/令牌到期）、一键刷新积分、模型列表
- **Dockerfile**：仓库自带开箱即用的两阶段构建（国内网络已切阿里云 Alpine 源）

- 语言：Go（纯标准库，零外部依赖）
- 端口：`:8367`（可通过配置或 `LB2A_LISTEN` 修改）

## 架构

```
客户端 (OpenAI SDK)
   │ POST /v1/chat/completions (Bearer ***)
   ▼
服务端：账号池挑号（积分最高且健康）→ 校验/刷新 token → 转发
   ▼
上游 chat API（仅 SSE）
   ▼ 出错 → 分类 → 冷却/禁用 → 轮换下一账号（单请求最多 3 次）
```

## Web 控制台（二开功能）

与 API 同端口挂载在 `/console`，共用 `api_key` 鉴权，无任何外部依赖（纯 Go 标准库 + 内嵌 HTML）。

### 添加账号（授权回填）

1. 控制台点「开始授权」→ 生成登录链接（15 分钟有效）
2. **无痕窗口**打开链接，用有道账号（手机号）登录
3. 登录后页面跳转到打不开的 `http://127.0.0.1:xxxxx/auth/callback?code=...&state=...` —— **这是正常现象**
4. 把地址栏完整回调 URL 粘贴回控制台，点「提交并添加账号」
5. 服务端完成 exchange → 凭据落盘 `auths/lobsterai-<uid>.json` → 入池 → 立即查询积分

同一浏览器加多个账号必须用无痕窗口（或先退出前一个号），否则会重复授权同一个号。一条回调 URL 的授权码只能用一次。

### 数据展示

- 统计卡：账号数 / 总积分 / 可用账号（15 秒自动刷新）
- 账号明细：昵称、UID、积分、状态（可用/冷却中/已禁用）、原因、令牌到期时间
- 一键「刷新积分」：立即向上游查询所有账号余额
- 可用模型列表、接入信息（API 地址 + Key）一览

### 控制台 API

挂载在 `/console/api/` 下：`login/start`、`login/callback`、`accounts`、`refresh-credits`、`models`

## 构建

```bash
go build -o lobsterai2api ./cmd/server
go build -o login ./cmd/login
go build -o credit ./cmd/credit
```

## 登录（命令行方式，也可用 Web 控制台代替）

```bash
./login.sh
# 或手动：
./login url   # 打印登录 URL（本地回调服务器就绪）
./login poll  # 等待回调 → exchange → 保存 auths/lobsterai-<uid>.json
# 浏览器打开 URL → 手机号/微信登录
```

## 运行

```bash
./lobsterai2api -config config.json
```

## Docker 部署（推荐）

```bash
git clone https://github.com/51v/lobsterai2api.git
cd lobsterai2api

# 1. 准备配置（api_key 自己编一个足够长的随机串）
cp config.example.json config.json
mkdir -p auths data
chown -R 10001:10001 auths data config.json
chmod 640 config.json

# 2. 构建镜像
docker build -t lobsterai2api:local .

# 3. 启动（仅本机访问用 -p 127.0.0.1:8367:8367）
docker run -d --name lobsterai2api --restart unless-stopped \
  -e TZ=Asia/Shanghai \
  -e LB2A_UPSTREAM_BASE=https://lobsterai-server.youdao.com \
  -e LB2A_LOGIN_PORTAL=https://lobsterai.youdao.com \
  -p 8367:8367 \
  -v "$PWD/auths":/app/auths \
  -v "$PWD/data":/app/data \
  -v "$PWD/config.json":/app/config.json:ro \
  lobsterai2api:local
```

详细部署文档见 [DEPLOY.md](DEPLOY.md)。

## 接口验证

```bash
# 非流式
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer <你的api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"你好"}],"stream":false}'

# 流式
curl -sN http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer <你的api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 模型列表
curl -s http://127.0.0.1:8367/v1/models -H "Authorization: Bearer <你的api_key>"

# 状态
curl -s http://127.0.0.1:8367/status

# Web 控制台
# 浏览器打开 http://127.0.0.1:8367/console?key=<你的api_key>
```

## 配置

见 `config.example.json`。环境变量前缀 `LB2A_*`：

| 变量 | 说明 |
|---|---|
| `LB2A_LISTEN` | 监听地址 |
| `LB2A_API_KEY` | 本地鉴权 key |
| `LB2A_AUTH_DIR` | 凭据文件目录 |
| `LB2A_STATE_FILE` | 账号池状态文件 |
| `LB2A_HARD_CREDIT` / `LB2A_SOFT_RATE` | 冷却时长 |
| `LB2A_ERR_THRESHOLD` / `LB2A_ERR_COOLDOWN` | 错误阈值与冷却 |
| `LB2A_TIMEOUT_SECONDS` | 上游超时 |
| `LB2A_UPSTREAM_BASE` | 上游 API 地址（必填） |
| `LB2A_LOGIN_PORTAL` | OAuth 登录门户（登录时必填） |

## 功能特性

- **多账号池** —— 自动加载 `auths/` 下凭据，每请求挑积分最高的健康账号
- **OpenAI 兼容** —— `/v1/chat/completions`（流式+非流式）、`/v1/models`、`/status`、`/healthz`
- **Web 控制台** —— 页面化加号（授权回填）、账号数据展示（二开新增）
- **OAuth 登录** —— 浏览器登录 + 回调回填，凭据自动落盘
- **令牌刷新** —— JWT 过期解析、到期前 10 分钟主动刷新、会话死亡自动禁用
- **错误分类** —— 余额不足冷却 12h、429 短冷却 60s、连续错误 3 次→10m、刷新被拒→禁用
- **请求级轮换** —— 单请求最多换 3 个账号
- **调度器** —— 定时余额刷新 + 账号解冻 + token 保活
- **动态模型列表** —— 从上游拉取，缓存 1h，失败回退静态表

## 已知限制

- 上游按账号权限区分模型可见性：免费积分号可用 `deepseek-flash`、`deepseek-v4-pro`、`deepseek-v4-flash`、`deepseek-v4-flash-vision-exp`、`glm-5.3-flash`、`MiniMax-M3` 等；其余旗舰模型（qwen3.8-max、kimi-k2.7-code、glm-5.3 等）需订阅套餐或加油包，调用会返回上游 40301「模型不可见或无访问权限」
- 上游每日签到端点未逆向，`DailyCheckin` 目前是 no-op，需外部签到脚本（参考 [DEPLOY.md](DEPLOY.md) 第七节）

## License

MIT
