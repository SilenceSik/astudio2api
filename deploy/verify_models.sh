#!/usr/bin/env bash
# 逐个验证 AStudio 渠道的每个模型是否真的可用（走 new-api 容器视角）。
#
# 为什么必须经容器：网关绑的是宿主的 0.0.0.0:8788，容器里 127.0.0.1 不通，
# 要经 docker 网络网关 172.21.0.1 —— 这也是 new-api 实际走的路径。
#
# 模型清单从网关自身拉，不再手写：手写清单会随时间漂移，
# 出现「脚本报全通、实际漏测了几个」的假绿。
set -u
ENVF="${ASTUDIO_ENV_FILE:-/opt/astudio2api/.env}"
[ -f "$ENVF" ] || { echo "✗ 找不到 $ENVF"; exit 1; }
KEY="$(grep -oP '(?<=ASTUDIO_API_KEY=).*' "$ENVF")"
[ -n "$KEY" ] || { echo "✗ .env 里没有 ASTUDIO_API_KEY"; exit 1; }
BASE="${ASTUDIO_HOST_URL:-http://172.21.0.1:8788}"

MODELS="$(curl -s --max-time 20 "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | python3 -c 'import sys,json;print(" ".join(m["id"] for m in json.load(sys.stdin).get("data",[])))')"
[ -n "$MODELS" ] || { echo "✗ 拉不到模型清单（网关未就绪或 key 无效）"; exit 1; }

echo "网关：$BASE"

echo "模型                          状态     响应摘要"
echo "-----------------------------  -------- ------------------------------------------"
for M in $MODELS; do
  # 注意：`docker run` 之后的参数由**宿主 shell** 展开，所以这里必须用宿主的
  # $KEY；写成 -e ASTUDIO_KEY=$KEY 再在 -H 里引用 $ASTUDIO_KEY 是错的
  # （那个变量只存在于容器内，宿主会把它展开成字面字符串，鉴权必失败）。
  # 想让它不出现在 ps 里得用 --env-file，这里不值得绕。
  OUT=$(docker run --rm --network new-api_default \
    curlimages/curl:latest -s --max-time 90 \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"model\":\"$M\",\"reasoning_effort\":\"none\",\"messages\":[{\"role\":\"user\",\"content\":\"reply ok\"}],\"max_tokens\":60}" \
    "$BASE/v1/chat/completions" 2>&1)

  CODE=$(printf '%s' "$OUT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('PARSE_FAIL'); sys.exit()
if 'error' in d:
    print('ERR:' + str(d['error'])[:60]); sys.exit()
ch = d.get('choices') or []
if not ch:
    print('NO_CHOICES'); sys.exit()
m = ch[0].get('message') or {}
t = (m.get('content') or m.get('reasoning_content') or '').strip()
print('OK ' + (t[:44] if t else '(空)'))
" 2>/dev/null)

  printf "%-29s %s\n" "$M" "$CODE"
done
