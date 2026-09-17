# LobsterAI2API 部署文档（含 Web 控制台二开版）

> 基于开源项目 [xinxinshuhao-create/lobsterai2api](https://github.com/xinxinshuhao-create/lobsterai2api) 反代网易有道 LobsterAI，转换为标准 OpenAI API。本版本在其基础上二次开发，内置 Web 控制台（`/console`），支持页面化添加账号（OAuth 回调回填）与账号池数据展示。
>
> 参考教程：[反代「网易有道 LobsterAI」并实现自动签到获取额度](https://qianling.pw/lobsterai2api/)（千灵）

## 一、项目简介

- **功能**：把有道 LobsterAI 转成 OpenAI 兼容 API（`/v1/chat/completions`、`/v1/models`），多账号池自动负载均衡、令牌自动刷新、错误自动冷却/换号
- **语言**：Go（纯标准库，零外部依赖）
- **二开增强**：内置 Web 控制台
  - 添加账号：页面生成授权链接 → 浏览器登录 → 把跳转后的回调 URL 粘贴回页面即可，不再依赖命令行登录工具
  - 数据展示：账号数/总积分/可用数统计、账号明细（积分、冷却/禁用状态、令牌到期）、一键刷新积分、模型列表
- **端口**：`:8367`（可改）

## 二、环境要求

- Linux 服务器（本部署在 Debian/Ubuntu，Docker 29.x）
- Docker Engine + Docker Compose v2（或直接 `docker run`）
- 中国大陆网络环境（构建时已切换阿里云 Alpine 源，Go 模块无外部依赖无需代理）

## 三、部署步骤

### 1. 获取代码

```bash
git clone --depth 1 https://github.com/xinxinshuhao-create/lobsterai2api.git /data/lobsterai2api
cd /data/lobsterai2api
```

（二开版含 `Dockerfile` 与 `internal/console/`，若用原版仓库需自行补这两个部分）

### 2. 写入 Dockerfile

仓库本身不含 Dockerfile，需自建（本部署已含在二开版中）：

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lobsterai2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
RUN sed -i 's|https://dl-cdn.alpinelinux.org|https://mirrors.aliyun.com|g' /etc/apk/repositories \
 && apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /app app
WORKDIR /app
COPY --from=build /out/ /app/
RUN mkdir -p /app/auths /app/data \
 && chown -R 10001:10001 /app
USER 10001
EXPOSE 8367
ENTRYPOINT ["/app/lobsterai2api", "-config", "/app/config.json"]
```

> 注意 `sed` 换阿里源那行：国内网络直连 Alpine 官方 CDN 会卡死（实测 APKINDEX 拉取挂起 9 分钟超时），必须换源。

### 3. 写入 config.json

```json
{
  "listen": ":8367",
  "api_key": "自己编一个足够长的随机串",
  "auth_dir": "/app/auths",
  "state_file": "/app/data/state.json",
  "cooldown": {
    "hard_credit": "12h",
    "soft_rate": "60s",
    "err_threshold": 3,
    "err_cooldown": "10m"
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22]
  },
  "upstream": {
    "timeout_seconds": 180,
    "base_url": "https://lobsterai-server.youdao.com"
  }
}
```

要点：

- `api_key`：本地 API 鉴权串，也是 Web 控制台的访问密钥，务必随机且够长（52 位随机串为宜）
- `schedule` 两个时刻做「真实签到（+100 积分）+ 余额刷新 + 账号解冻」，签到已内置于 Go 代码（见第七节）
- `upstream.base_url`：上游地址，也可用环境变量 `LB2A_UPSTREAM_BASE` 覆盖

### 4. 构建并启动

```bash
cd /data/lobsterai2api
mkdir -p auths data
chown -R 10001:10001 auths data config.json
chmod 640 config.json

docker build -t lobsterai2api:local .

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

端口绑定说明：

- `-p 127.0.0.1:8367:8367`：仅本机访问（更安全）
- `-p 8367:8367`：所有网卡可访问（局域网设备直连，本部署采用此方式）

### 5. 验证

```bash
docker ps --filter name=lobsterai2api          # Up 状态
docker logs lobsterai2api                      # listening on :8367
curl -s http://127.0.0.1:8367/healthz          # ok
curl -s http://127.0.0.1:8367/console -o /dev/null -w '%{http_code}\n'  # 200
```

## 四、Web 控制台使用

访问地址：`http://<服务器IP>:8367/console?key=<config.json里的api_key>`

### 添加账号（授权回填流程）

1. 打开控制台，点「**开始授权**」——生成一次新的登录链接（15 分钟有效）
2. **无痕窗口**打开该链接，用有道账号（手机号）登录
3. 登录成功后页面会跳转到一个打不开的 `http://127.0.0.1:xxxxx/auth/callback?code=...&state=...` 地址——**这是正常现象**
4. 把浏览器地址栏里的**完整回调 URL** 复制，粘贴回控制台第 3 步的输入框，点「提交并添加账号」
5. 页面提示添加成功：显示昵称、积分，账号自动入池

注意：

- 同一浏览器加多个账号必须用无痕窗口（或先退出前一个号），否则会重复授权同一个号
- 一条回调 URL 中的授权码只能用一次，失败后需重新「开始授权」
- 新账号积分显示 0 属正常，点「刷新积分」或等定时刷新即可

### 数据展示

- 顶部统计卡：账号数 / 总积分 / 可用账号
- 账号池表格：昵称、UID、积分、状态（可用/冷却中/已禁用）、原因、令牌到期时间（15 秒自动刷新）
- 「刷新积分」按钮：立即向上游查询所有账号余额
- 可用模型：当前池内可用模型列表（实测 25 个，含 deepseek-v4、glm-5.x、qwen3.x、kimi-k2.x、MiniMax、doubao-seed 等）
- 接入信息卡：API 地址与 Key 一览

## 五、API 使用

```bash
# 模型列表
curl -s http://<IP>:8367/v1/models -H "Authorization: Bearer <KEY>"

# 对话（非流式）
curl -s http://<IP>:8367/v1/chat/completions \
  -H "Authorization: Bearer <KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"你好"}],"stream":false}'

# 对话（流式）—— stream: true
```

OpenAI SDK 接入：`base_url = http://<IP>:8367/v1`，`api_key` 同上。

## 六、额度说明

- 注册赠送 300 积分，14 天有效
- 邀请赠送 300 积分，365 天有效
- 每日签到 +100 积分，30 天有效
- 消耗比率（以 deepseek-flash 实测估算）：非缓存输入约 200 积分/1M tokens，输出约为输入 4 倍；各模型倍率不同，以 `/api/models/available` 返回的 `costMultiplier` 为准

## 七、每日自动签到

签到已内置于 Go 服务，开箱即用，无需外部脚本。

### 自动签到

服务启动 5 秒后立即对全部账号签到一次；之后每天 9:00 和 21:00 自动执行（可在 `config.json` 的 `schedule.checkin_hours` 调整）。

签到流程：调官方 update 接口拿 clientVersion → slot → context 判 `claimedToday` → POST `actions/check_in`。同一账号同一天幂等，重复触发不会多加分。

### 手动签到

Web 控制台的账号池卡片有「签到」按钮，点击即手动触发全部账号签到，结果实时回显。

### 签到验证

查看容器日志确认：

```bash
docker logs lobsterai2api | grep checkin
# 预期输出: checkin 93881: ✅ +100 积分
```

或看 Web 控制台账号列表的「最后签到」列。

## 八、运维备忘

- **数据目录**：`auths/`（账号凭据）、`data/state.json`（池状态），均在宿主机，容器重建不丢
- **加账号后无需重启**：控制台添加的账号实时入池；手工往 `auths/` 放文件则需 `docker restart lobsterai2api`
- **令牌自动刷新**：到期前 10 分钟自动刷新；session 失效会自动禁用该账号（页面显示「已禁用」），重新走一遍授权即可
- **升级**：`git pull` → `docker build -t lobsterai2api:local .` → `docker rm -f lobsterai2api` → 重跑 `docker run`
- **防火墙**：如需局域网访问，放行 8367/tcp（UFW：`ufw allow 8367/tcp`）

## 九、常见坑

1. **构建卡在 `fetch dl-cdn.alpinelinux.org`**：国内网络问题，Dockerfile 里换阿里源（本部署已内置）
2. **登录后跳转 127.0.0.1 打不开**：正常，回调设计如此；把整条 URL 粘贴回控制台即可
3. **添加账号报 `no FilePath set`**：旧版二开 bug，已修复（`SaveAtomic` 前必须先赋值 `FilePath`）
4. **回调 URL 提交报 state 无效/过期**：授权码一次性消耗或超过 15 分钟，重新「开始授权」
5. **新账号积分为 0**：尚未触发余额刷新，点控制台「刷新积分」
6. **`/status` 与控制台数据一致**：都来自内存池，重启后从 `auths/` + `state.json` 恢复
