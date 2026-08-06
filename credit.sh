#!/usr/bin/env bash
# credit.sh — LobsterAI 积分查询（人类可读）
#
# 用法:
#   ./credit.sh
set -euo pipefail

cd "$(dirname "$0")"

CREDIT_BIN="./credit"
if [[ ! -x "$CREDIT_BIN" ]]; then
    go build -o "$CREDIT_BIN" ./cmd/credit
fi

"$CREDIT_BIN" -pretty
