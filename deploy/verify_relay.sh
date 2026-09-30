#!/usr/bin/env bash
# 逐个模型走 new-api 真实中继，验证端到端可用。
set -uo pipefail
DB="${NEWAPI_DB:-/opt/new-api/data/new-api.db}"
API="${NEWAPI_BASE:-http://127.0.0.1:3001}/v1/chat/completions"
MODELS=(spark-x2.5 glm-5.2 deepseek-v4-pro deepseek-v4-flash)
OUT="${RELAY_OUT:-/tmp/relay}"
mkdir -p "$OUT"; rm -f "$OUT"/*.json

KEY=""
for i in 1 2 3 4 5; do
  KEY=$(sqlite3 "file:$DB?mode=ro" "SELECT key FROM tokens WHERE status=1 ORDER BY id LIMIT 1;" 2>/dev/null || true)
  [ -n "$KEY" ] && break
  sleep 4
done
[ -n "$KEY" ] || { echo "✗ 读不到中继 key"; exit 1; }
echo "中继 key 长度=${#KEY}"

for M in "${MODELS[@]}"; do
  curl -s --max-time 90 "$API" \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"model\":\"$M\",\"reasoning_effort\":\"none\",\"messages\":[{\"role\":\"user\",\"content\":\"回复：好\"}],\"max_tokens\":64}" \
    > "$OUT/$M.json"
done
python3 "$(dirname "$0")/parse_relay.py" "$OUT"/*.json
