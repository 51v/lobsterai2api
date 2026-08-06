#!/usr/bin/env bash
# login.sh — LobsterAI OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#
# 流程:
#   1. login url 起本地回调服务器，打印授权 URL
#   2. 你在浏览器打开 URL 完成登录（回调自动完成 exchange）
#   3. login poll 读取结果 → 落盘 auths/lobsterai-<uid>.json
#   4. 重启 lobsterai2api 加载新账号
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="lobsterai2api"
PORT="${LB2A_LISTEN:-8367}"

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  LobsterAI OAuth 登录"
echo "============================================================"
echo ""

echo "正在启动本地回调服务器并生成登录链接..."
AUTH_URL=$("$LOGIN_BIN" url) || {
    echo "启动回调服务器失败"
    exit 1
}

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

UID_=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")
AUTH_FILE=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('auth_file',''))")

if [[ -z "$UID_" || -z "$AUTH_FILE" ]]; then
    echo "无法获取 uid/auth_file，请检查 token 是否有效"
    exit 1
fi

echo "已保存: $AUTH_FILE"

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    COUNT=$(curl -s "http://127.0.0.1:${PORT}/status" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $UID_"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "============================================================"
