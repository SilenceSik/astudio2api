#!/usr/bin/env bash
# 用 new-api 自己的渠道测试接口验证 astudio2api（id=166）真的能转发。
# 凭据只从环境文件读入变量，不打印。
set -uo pipefail

ENVF="${NEWAPI_ENV_FILE:-/root/.cnb/newapi.env}"
CID="${CHANNEL_ID:-1}"

[ -f "$ENVF" ] || { echo "✗ 找不到 $ENVF，改用下面第 2 段的手工验证"; exit 1; }

NEWAPI_TOKEN="$(grep -oP '(?<=NEWAPI_TOKEN=).*' "$ENVF" | head -1)"
NEWAPI_BASE="$(grep -oP '(?<=NEWAPI_BASE=).*' "$ENVF" | head -1)"
NEWAPI_BASE="${NEWAPI_BASE:-http://127.0.0.1:3001}"
[ -n "$NEWAPI_TOKEN" ] || { echo "✗ 没读到 NEWAPI_TOKEN"; exit 1; }

echo "=== ① new-api 是否已加载该渠道 ==="
curl -s --max-time 15 -H "Authorization: Bearer $NEWAPI_TOKEN" \
  "$NEWAPI_BASE/api/channel/$CID" \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
c = d.get('data') or {}
if not c:
    print('  ✗ 渠道未找到:', str(d)[:200]); sys.exit(1)
print('  name       =', c.get('name'))
print('  status     =', c.get('status'), '(1=启用)')
print('  base_url   =', c.get('base_url'))
print('  test_model =', c.get('test_model'))
print('  group      =', c.get('group'))
"

echo
echo "=== ② 让 new-api 亲自跑一次渠道测试（用 test_model 真实打上游）==="
curl -s --max-time 120 -H "Authorization: Bearer $NEWAPI_TOKEN" \
  "$NEWAPI_BASE/api/channel/test/$CID?model=xopdsv4flash0731in" \
  | python3 -c "
import sys, json
raw = sys.stdin.read()
try:
    d = json.loads(raw)
except Exception:
    print('  原始返回:', raw[:300]); sys.exit(1)
ok = d.get('success')
msg = d.get('message') or ''
print('  success =', ok)
print('  message =', msg[:200])
print()
print('  →', '✓ 渠道可用，new-api 已能真实转发' if ok else '✗ 渠道测试失败')
"
