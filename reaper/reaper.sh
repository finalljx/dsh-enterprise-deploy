#!/usr/bin/env bash
# 空闲回收器：唯一的职责是 scale，不碰业务逻辑。
#
# 两个方向：
#   拉起：desiredState=running 但副本为 0 -> scale 1
#   休眠：无 open turn 且静默超阈值     -> scale 0，并把 desiredState 置 sleeping
#
# 关键守卫：休眠前再查一次实时状态。列表到 scale 之间用户可能刚好回来。
set -uo pipefail

: "${CONTROL_URL:?CONTROL_URL is required}"
: "${CONTROL_TOKEN:?CONTROL_TOKEN is required}"
NAMESPACE="${NAMESPACE:-dsh-users}"
IDLE_SECONDS="${IDLE_SECONDS:-900}"
INTERVAL="${INTERVAL:-5}"

auth=(-H "Authorization: Bearer ${CONTROL_TOKEN}")

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }

scale_up() {
  local handle="$1"
  local replicas
  replicas=$(kubectl -n "$NAMESPACE" get deploy "dsh-${handle}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  [ -z "$replicas" ] && return 0
  [ "$replicas" = "0" ] || return 0
  log "wake ${handle}"
  kubectl -n "$NAMESPACE" scale "deploy/dsh-${handle}" --replicas=1 >/dev/null 2>&1 || log "wake ${handle} failed"
}

scale_down() {
  local handle="$1" subject="$2" seen="$3"
  # 二次确认：openTurn 已关且 lastActivityAt 未变
  local now
  now=$(curl -fsS -m 3 "${auth[@]}" "${CONTROL_URL}/internal/lookup?subject=$(printf '%s' "$subject" | jq -sRr @uri)" 2>/dev/null || true)
  [ -z "$now" ] && return 0
  local open last
  open=$(printf '%s' "$now" | jq -r '.openTurn')
  last=$(printf '%s' "$now" | jq -r '.lastActivityAt')
  if [ "$open" != "false" ] || [ "$last" != "$seen" ]; then
    log "skip ${handle}: activity changed"
    return 0
  fi
  log "sleep ${handle} (idle since ${last})"
  kubectl -n "$NAMESPACE" scale "deploy/dsh-${handle}" --replicas=0 >/dev/null 2>&1 \
    && curl -fsS -m 3 "${auth[@]}" -H 'content-type: application/json' \
         -d "$(jq -nc --arg s "$subject" '{subject:$s,desiredState:"sleeping",clearPod:true}')" \
         "${CONTROL_URL}/internal/desired" >/dev/null 2>&1
}

while true; do
  # 方向一：拉起
  curl -fsS -m 5 "${auth[@]}" "${CONTROL_URL}/internal/wanted" 2>/dev/null \
    | jq -r '.[].handle' 2>/dev/null \
    | while read -r handle; do
        [ -n "$handle" ] && scale_up "$handle"
      done

  # 方向二：休眠
  curl -fsS -m 5 "${auth[@]}" \
    "${CONTROL_URL}/internal/idle?olderThanSeconds=${IDLE_SECONDS}" 2>/dev/null \
    | jq -r '.[] | "\(.handle)\t\(.subject)\t\(.lastActivityAt)"' 2>/dev/null \
    | while IFS=$'\t' read -r handle subject seen; do
        [ -n "${handle:-}" ] && scale_down "$handle" "$subject" "$seen"
      done

  sleep "$INTERVAL"
done
