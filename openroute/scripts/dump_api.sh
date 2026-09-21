#!/usr/bin/env bash
# 导出各只读接口的真实响应，用于校准前端的 TypeScript 类型定义。
#
# 用法：
#   ./scripts/dump_api.sh http://127.0.0.1:18888 admin 密码
#
# 输出到 stdout，便于与 frontend/src/api/types.ts 逐项对照。

set -euo pipefail

BASE="${1:-http://127.0.0.1:18888}"
USER="${2:-admin}"
PASS="${3:?需要密码}"

API="$BASE/api/v1"

TOKEN=$(curl -s -X POST "$API/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER\",\"password\":\"$PASS\"}" \
  | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4)

[ -n "$TOKEN" ] || { echo "登录失败" >&2; exit 1; }

show() {
  local label="$1"; shift
  echo "════════ $label"
  curl -s -H "Authorization: Bearer $TOKEN" "$@" | head -c 900
  echo
  echo
}

echo "### 只读接口响应快照（用于校准前端类型）"
echo

show "GET /traffic/overview"        "$API/traffic/overview"
show "GET /traffic/dashboard"       "$API/traffic/dashboard"
show "GET /traffic/timeseries"      "$API/traffic/timeseries"
show "GET /traffic/top?dim=rule"    "$API/traffic/top?dimension=rule"
show "GET /probe/overview"          "$API/probe/overview"
show "GET /nodes"                   "$API/nodes"
show "GET /nodes/1/metrics"         "$API/nodes/1/metrics"
show "GET /nodes/1/metrics/realtime" "$API/nodes/1/metrics/realtime"
show "GET /nodes/1/drift"           "$API/nodes/1/drift"
show "GET /node-groups"             "$API/node-groups"
show "GET /device-groups"           "$API/device-groups"
show "GET /device-groups-schema"    "$API/device-groups-schema?type=inbound"
show "GET /forward-rules"           "$API/forward-rules"
show "GET /rule-groups"             "$API/rule-groups"
show "GET /users"                   "$API/users"
show "GET /users/1/traffic"         "$API/users/1/traffic"
show "GET /users/1/rules"           "$API/users/1/rules"
show "GET /user-groups"             "$API/user-groups"
show "GET /alerts"                  "$API/alerts"
show "GET /alerts/history"          "$API/alerts/history"
show "GET /snapshots"               "$API/snapshots"
show "GET /migrations"              "$API/migrations"
show "GET /backups"                 "$API/backups"
show "GET /tasks"                   "$API/tasks"
show "GET /audit-logs"              "$API/audit-logs"
show "GET /settings"                "$API/settings"
show "GET /api-tokens"              "$API/api-tokens"
show "GET /system/info"             "$API/system/info"
show "GET /system/status"           "$API/system/status"
show "GET /system/version"          "$API/system/version"
show "GET /system/errors"           "$API/system/errors"
