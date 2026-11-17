#!/bin/sh
# 启动 DSH，并把本次进程的 launch token 上报给控制面。
#
# 顺序不能变：
#   1. 校验环境
#   2. 交叉校验身份（pod 标签 vs 卷上标记）
#   3. 覆盖平台 profile（企业配置由平台拥有，用户不可改）
#   4. 启动 dsh web（TLS，绑定 pod IP）
#   5. 从 stdout 取 launch token，注册到控制面
set -eu

: "${DSH_HOME:=/data/dsh}"
: "${DSH_USER_ID:?DSH_USER_ID is required}"
: "${POD_IP:?POD_IP is required}"
: "${DSH_PUBLIC_HOST:?DSH_PUBLIC_HOST is required}"
: "${CONTROL_URL:?CONTROL_URL is required}"
: "${CONTROL_TOKEN:?CONTROL_TOKEN is required}"
: "${PRINCIPAL_URI:?PRINCIPAL_URI is required}"
: "${DSH_LABEL:?DSH_LABEL is required}"   # 证明确实挂在带 ownership 标签的 Pod 上
: "${DSH_HANDLE:?DSH_HANDLE is required}" # 部署名后缀，来自注解 dsh.corp/handle

HANDLE="$DSH_HANDLE"

MARKER="$DSH_HOME/.owner.json"
ACTUAL=$(sed -n 's/.*"userId"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$MARKER" 2>/dev/null | head -n1)
if [ "$DSH_USER_ID" != "$ACTUAL" ]; then
  echo "FATAL owner mismatch: pod='$DSH_USER_ID' volume='${ACTUAL:-<none>}'" >&2
  exit 1
fi

mkdir -p "$DSH_HOME/profiles/web"
# 首次启动：从镜像里预置的 profile 复制（含活动上报插件），不需要联网
if [ ! -f "$DSH_HOME/profiles/web/package.json" ] && [ -d /opt/dsh/profile-seed/profiles/web ]; then
  cp -a /opt/dsh/profile-seed/profiles/web/. "$DSH_HOME/profiles/web/"
fi
install -m 0644 /opt/dsh/profile/cordis.patch.yml "$DSH_HOME/profiles/web/cordis.patch.yml"

# 老卷可能是在插件装好之前创建的：缺了就补一次，否则 DSH 会因必需插件未激活而拒绝启动
if [ ! -d "$DSH_HOME/profiles/web/node_modules/@corp/dsh-plugin-activity" ] \
   && [ -d /opt/dsh/profile-seed/profiles/web/node_modules ]; then
  cp -a /opt/dsh/profile-seed/profiles/web/node_modules "$DSH_HOME/profiles/web/"
fi

# kubelet 挂卷时 fsGroup 会把文件权限放宽到 660，DSH 拒绝「组可读」的凭据文件
if [ -f "$DSH_HOME/.credentials.yaml" ]; then
  chmod 600 "$DSH_HOME/.credentials.yaml"
fi

LOG=/tmp/dsh-web.log
: > "$LOG"

dsh web \
  --host "$POD_IP" \
  --port 8443 \
  --public-url "${PUBLIC_SCHEME:-https}://${DSH_PUBLIC_HOST}" \
  --trusted-host "$DSH_PUBLIC_HOST" \
  --tls-cert /tls/tls.crt \
  --tls-key /tls/tls.key \
  --no-open >"$LOG" 2>&1 &
DSH_PID=$!

cleanup() { kill -TERM "$DSH_PID" 2>/dev/null || true; wait "$DSH_PID" 2>/dev/null || true; }
trap 'cleanup; exit 0' TERM INT

TOKEN=""
i=0
while [ "$i" -lt 120 ]; do
  TOKEN=$(sed -n 's/.*[?&]token=\([A-Za-z0-9_-]*\).*/\1/p' "$LOG" 2>/dev/null | head -n1 || true)
  [ -n "$TOKEN" ] && break
  if ! kill -0 "$DSH_PID" 2>/dev/null; then
    echo "FATAL dsh exited before printing a launch token" >&2
    cat "$LOG" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 0.5
done
[ -n "$TOKEN" ] || { echo "FATAL no launch token within 60s" >&2; exit 1; }

PAYLOAD=$(jq -nc \
  --arg handle "$HANDLE" \
  --arg subject "$DSH_USER_ID" \
  --arg publicHost "$DSH_PUBLIC_HOST" \
  --arg podIP "$POD_IP" \
  --arg principalUri "$PRINCIPAL_URI" \
  --arg launchToken "$TOKEN" \
  '{handle:$handle, subject:$subject, publicHost:$publicHost, podIP:$podIP, principalUri:$principalUri, launchToken:$launchToken}')

curl -fsS -m 5 -X POST "$CONTROL_URL/internal/register" \
  -H "Authorization: Bearer $CONTROL_TOKEN" \
  -H "content-type: application/json" \
  --data "$PAYLOAD" >/dev/null \
  || { echo "FATAL register with control failed" >&2; exit 1; }

echo "registered ${DSH_USER_ID} -> ${POD_IP}" >&2

wait "$DSH_PID" || true
