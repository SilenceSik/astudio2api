#!/usr/bin/env bash
# 把 astudio2api 注册为 new-api 渠道。幂等：同名渠道已存在则更新，不重复插入。
#
# ⚠️ 两个必须同时维护的地方（漏一个就会出现「模型无可用渠道」）：
#   1. channels   —— 渠道自身的配置
#   2. abilities  —— new-api 的「分组 × 模型 → 渠道」路由表（派生表，不会自动同步）
# 只改 channels 不改 abilities，请求会报
#   model_not_found: 分组 X 下模型 Y 无可用渠道（distributor）
set -euo pipefail

DB="${NEWAPI_DB:-/opt/new-api/data/new-api.db}"
ENVF="${ASTUDIO_ENV_FILE:-/opt/astudio2api/.env}"
NAME="${CHANNEL_NAME:-astudio2api}"
BASE="${ASTUDIO_HOST_URL:-http://172.21.0.1:8788}"
GRP="${NEWAPI_GROUPS:-openai}"
REMARK='讯飞 AStudio 反代（多账号池 + 自动签到）'

# 对外模型名 = 网关实际暴露的「友好名」，**不带前缀**。
#
# 为什么不加 astudio- 前缀：网关自己会把友好名归一成上游代号
# （canonicalModel，见 go/server.go），new-api 这边不需要 model_mapping，
# 用户直接用 glm-5.2 这种自然名即可（与上游命名一致）。
#
# ⚠️ new-api 的模型名是全局的：同名模型可能被别的渠道也声明，按 priority 择优
# 路由，请求不一定落到本渠道。想独占就把本渠道 priority 抬到高于对方。
MODELS_LIST=(
  spark-x2.5
  glm-5.2
  deepseek-v4-pro
  deepseek-v4-flash
)
# test_model 必须是上面这个列表里的名字之一，否则渠道测试恒报「没有可用账号」。
TESTMODEL='deepseek-v4-flash'

[ -f "$ENVF" ] || { echo "✗ 找不到 $ENVF"; exit 1; }
[ -f "$DB" ]   || { echo "✗ 找不到 $DB"; exit 1; }

KEY="$(grep -oP '(?<=ASTUDIO_API_KEY=).*' "$ENVF")"
[ -n "$KEY" ] || { echo "✗ .env 里没有 ASTUDIO_API_KEY"; exit 1; }

MODELS="$(printf '%s\n' "${MODELS_LIST[@]}" | paste -sd, -)"
MAPPING=''

SETTING='{"force_format":false,"thinking_to_content":false,"proxy":"","pass_through_body_enabled":false,"system_prompt":"","system_prompt_override":false}'
CHINFO='{"is_multi_key":false,"multi_key_size":0,"multi_key_status_list":null,"multi_key_polling_index":0,"multi_key_mode":""}'
SETTINGS='{"allow_service_tier":false,"disable_store":false,"allow_safety_identifier":false,"allow_include_obfuscation":false,"allow_inference_geo":false,"disable_task_polling_sleep":false,"upstream_model_update_check_enabled":false,"upstream_model_update_auto_sync_enabled":false,"upstream_model_update_ignored_models":[],"upstream_model_update_last_detected_models":[],"upstream_model_update_last_check_time":0}'

EXISTING="$(sqlite3 "$DB" "SELECT id FROM channels WHERE name='$NAME';" | head -1)"

if [ -n "$EXISTING" ]; then
  echo "→ 渠道已存在（id=$EXISTING），更新配置"
  CID="$EXISTING"
  sqlite3 "$DB" <<SQL
UPDATE channels SET
  key='$KEY', base_url='$BASE', models='$MODELS', model_mapping='$MAPPING',
  "group"='$GRP', test_model='$TESTMODEL', status=1, remark='$REMARK'
WHERE id=$CID;
SQL
else
  echo "→ 新建渠道"
  sqlite3 "$DB" <<SQL
INSERT INTO channels (
  type, key, status, name, weight, created_time, test_model, base_url,
  models, model_mapping, "group", priority, auto_ban, remark,
  setting, channel_info, settings
) VALUES (
  1, '$KEY', 1, '$NAME', 0, strftime('%s','now'), '$TESTMODEL', '$BASE',
  '$MODELS', '$MAPPING', '$GRP', 7, 1, '$REMARK',
  '$SETTING', '$CHINFO', '$SETTINGS'
);
SQL
  CID="$(sqlite3 "$DB" "SELECT id FROM channels WHERE name='$NAME';" | head -1)"
fi

# 重建 abilities（分组 × 模型 → 渠道）。不重建就会出现「无可用渠道」。
echo "→ 同步 abilities（$(printf '%s\n' "${MODELS_LIST[@]}" | wc -l) 模型 × $(echo "$GRP" | tr ',' '\n' | wc -l) 分组）"
# 注意：不用 `read -ra <<<`（here-string）——它在部分 shell 下会让脚本提前退出；
# 用 printf | while read 的 POSIX 写法。
{
  echo "BEGIN;"
  echo "DELETE FROM abilities WHERE channel_id=$CID;"
  printf '%s\n' "${MODELS_LIST[@]}" | while IFS= read -r m; do
    printf '%s\n' "$GRP" | tr ',' '\n' | while IFS= read -r g; do
      [ -n "$g" ] || continue
      printf "INSERT INTO abilities (\"group\", model, channel_id, enabled, priority, weight, tag) VALUES ('%s','%s',%s,1,7,0,'');\n" "$g" "$m" "$CID"
    done
  done
  echo "COMMIT;"
} | sqlite3 "$DB"

echo
echo "=== 落库结果（id=$CID）==="
sqlite3 -line "$DB" "SELECT id,name,type,status,base_url,test_model,priority,\"group\" AS grp FROM channels WHERE id=$CID;"
echo "--- models / model_mapping ---"
sqlite3 -line "$DB" "SELECT models, model_mapping FROM channels WHERE id=$CID;"
echo "--- abilities 行数 ---"
sqlite3 "$DB" "SELECT COUNT(*) FROM abilities WHERE channel_id=$CID;"
echo
echo "重启 new-api 让配置生效：docker restart new-api"
