#!/bin/sh
# 初始化卷的 owner 标记。
#   有标记            -> 放行，交给主容器交叉校验
#   空卷              -> 写入标记
#   有数据但没标记    -> 拒绝（卷被恢复到了错误的 PVC）
set -eu

MARKER=/data/dsh/.owner.json
: "${DSH_USER_ID:?DSH_USER_ID is required}"

if [ -s "$MARKER" ]; then
  exit 0
fi

if [ -n "$(ls -A /data/dsh 2>/dev/null || true)" ]; then
  echo "FATAL volume is not empty but has no owner marker" >&2
  exit 1
fi

cat > "$MARKER" <<JSON
{"userId":"$DSH_USER_ID","issuedAt":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
JSON
chmod 0600 "$MARKER"
