#!/usr/bin/env bash
# 隔离验收：任何一条失败都说明架构约束没成立，不要上线。
set -uo pipefail

PRINCIPAL="alice@corp"
USER=alice                 # slug：Deployment/Service 名
LABEL=alice_corp           # dsh.corp/user 标签值：userId 的 @ 换成 _
OTHER=bob
HOST="alice.dsh.corp"
OTHER_HOST="bob.dsh.corp"
NS=dsh-users
SCHEME="${SCHEME:-https}"
SKIP_KATA="${SKIP_KATA:-}"   # 本地 k3d 无 KVM，设 1 跳过微虚拟机检查
FAIL=0

say() { printf '\n=== %s ===\n' "$1"; }
ok()  { printf '  PASS  %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; FAIL=$((FAIL+1)); }

say "1. pod 存在、通过 owner 标记校验，且跑在 Kata 微虚拟机内"
READY_COUNT="$(kubectl -n "$NS" get deploy "dsh-$USER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
if [ "$READY_COUNT" = "1" ]; then
  ok "dsh-$USER ready"
else
  bad "dsh-$USER 未就绪"
fi

POD=$(kubectl -n "$NS" get pod -l "dsh.corp/user=$LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
RC=$(kubectl -n "$NS" get pod "${POD:-no-pod}" -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null)
if [ -n "$SKIP_KATA" ]; then
  printf '  SKIP  runtimeClassName（SKIP_KATA=1）\n'
elif [ "${RC:-}" = "kata-clh-runtime-rs" ]; then
  ok "runtimeClassName=$RC"
else
  bad "runtimeClassName=${RC:-<unset>}，期望 kata-clh-runtime-rs"
fi

if [ -n "$SKIP_KATA" ]; then
  :
elif [ -n "${POD:-}" ]; then
  NODE=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  HOST_KERNEL=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.kernelVersion}' 2>/dev/null)
  GUEST_KERNEL=$(kubectl -n "$NS" exec "$POD" -- uname -r 2>/dev/null)
  if [ -n "${HOST_KERNEL:-}" ] && [ -n "${GUEST_KERNEL:-}" ] && [ "$GUEST_KERNEL" != "$HOST_KERNEL" ]; then
    ok "guest kernel $GUEST_KERNEL 与宿主 $HOST_KERNEL 不同"
  else
    bad "guest/host 内核无法区分（guest=${GUEST_KERNEL:-<空>} host=${HOST_KERNEL:-<空>}），pod 可能没跑在微虚拟机里"
  fi
else
  bad "找不到用户 pod，跳过内核检查"
fi

say "2. 挂错卷必须被准入拒绝"
# 探测 pod 必须是 PSA 合规的：dsh-users 开了 PodSecurity restricted，
# 否则会先被 PodSecurity 拒掉，这条检查就会「因为错误的原因通过」。
BAD_POD=$(cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: bad-binding
  namespace: $NS
  labels: { dsh.corp/user: $LABEL }
  annotations:
    dsh.corp/home-claim: dsh-home-doesnotexist
    dsh.corp/principal: $PRINCIPAL
spec:
  securityContext: { runAsNonRoot: true, runAsUser: 1000, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: c
      image: busybox
      command: ["sleep", "1"]
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ["ALL"] } }
  volumes: [{ name: h, persistentVolumeClaim: { claimName: dsh-home-other } }]
YAML
)
# 只认准入策略自己的提示：PSA 的错误信息里没有 home-claim 字样。
# 先落变量再 grep：pipefail 下 `cmd | grep -q` 会因为 grep 提前退出、
# 上游收到 SIGPIPE(141) 而整体判失败，看起来就像「策略没生效」。
APPLY_OUT="$(printf '%s' "$BAD_POD" | kubectl apply -f - 2>&1 || true)"
if printf '%s' "$APPLY_OUT" | grep -qi 'home-claim\|dsh-pod-home-binding'; then
  ok "准入策略拒绝了错误的 PVC 绑定"
else
  bad "准入策略没有拦住错误绑定（危险）"
  printf '%s' "$BAD_POD" | kubectl delete -f - --ignore-not-found >/dev/null 2>&1
fi

say "3. 用户 pod 之间不可互访"
if [ -n "${POD:-}" ]; then
  if kubectl -n "$NS" exec "$POD" -- timeout 3 curl -sf "http://dsh-$OTHER:8080/" >/dev/null 2>&1; then
    bad "pod 之间可以互访（NetworkPolicy 未生效）"
  else
    ok "pod 之间不可互访"
  fi
else
  bad "找不到用户 pod，跳过网络检查"
fi

say "4. 未认证访问应跳转 SSO，而不是 401/200"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$SCHEME://$HOST/" || true)
if [ "${CODE}" = "302" ] || [ "${CODE}" = "303" ]; then
  ok "未认证访问被重定向（${CODE}）"
else
  bad "未认证访问返回 ${CODE}，期望 302"
fi

say "5. 用 Alice 的会话访问 Bob 的子域必须 403"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -b "${ALICE_COOKIE:?set ALICE_COOKIE to alice session cookie file}" "$SCHEME://$OTHER_HOST/" || true)
if [ "${CODE}" = "403" ]; then
  ok "跨身份访问被拒绝（403）"
else
  bad "跨身份访问返回 ${CODE}，期望 403"
fi

say "6. 浏览器看不到 launch token"
HDRS="$(curl -sI "$SCHEME://$HOST/" 2>/dev/null || true)"
if printf '%s' "$HDRS" | grep -qi 'location:.*token='; then
  bad "重定向里带了 token（危险）"
else
  ok "重定向 URL 不含 token"
fi

say "7. 会话导出后每条 user message 带 actor（需方式 C 落地）"
printf '  提示：%s\n' "当前实现是方式 B，source.actor 尚未写入；见 checklist §6.1"

say "8. 登录页与启动等待页"
LOC=$(curl -sI -H 'Accept: text/html' -H 'Sec-Fetch-Mode: navigate' "$SCHEME://$HOST/" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
case "${LOC}" in
  */_dsh/login*) ok "未登录的浏览器导航落到登录页（${LOC}）" ;;
  *) bad "浏览器导航没有落到登录页（Location=${LOC:-<无>}）" ;;
esac

CODE=$(curl -s -o /dev/null -w '%{http_code}' "$SCHEME://$HOST/_dsh/status" || true)
if [ "$CODE" = "401" ]; then
  ok "状态接口未登录返回 401"
else
  bad "状态接口未登录返回 ${CODE}，期望 401"
fi

# 已登录访问要么直接进工作区（200），要么停在等待页（200 + X-DSH-State），
# 不该看到 502/503 的裸错误页。
CODE=$(curl -s -D /tmp/dsh-verify-h -o /dev/null -w '%{http_code}' -b "${ALICE_COOKIE:?set ALICE_COOKIE to alice session cookie file}" \
  -H 'Accept: text/html' -H 'Sec-Fetch-Mode: navigate' "$SCHEME://$HOST/" || true)
if [ "$CODE" = "200" ]; then
  ok "已登录的浏览器导航返回页面（200）"
else
  bad "已登录的浏览器导航返回 ${CODE}，期望 200（工作区或等待页）"
fi
STATE=$(tr -d '\r' < /tmp/dsh-verify-h 2>/dev/null | sed -n 's/^[Xx]-[Dd][Ss][Hh]-[Ss]tate: //p')
[ -n "$STATE" ] && printf '  当前状态：%s\n' "$STATE"

say "9. 活动上报插件真的被加载（否则回收器会把活跃用户休眠）"
# 注意：dsh web 的日志在容器内 /tmp/dsh-web.log，stdout 只有一行 registered。
POD_NAME=$(kubectl -n "$NS" get pod -l "dsh.corp/user=$LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$POD_NAME" ]; then
  bad "找不到用户 pod，无法验证 activity 插件"
else
  # ① 激活失败会在启动日志里留痕（"did not activate" / ERR_MODULE_NOT_FOUND）
  HIT=$(kubectl -n "$NS" exec "$POD_NAME" -c dsh -- sh -c \
    'grep -E "did not activate|ERR_MODULE_NOT_FOUND" /tmp/dsh-web.log' 2>/dev/null || true)
  if [ -z "$HIT" ]; then
    ok "启动日志里没有插件激活失败"
  else
    bad "插件未激活：${HIT}"
  fi

  # ② 软链必须能解析到真实的 package.json（-d 对断链为假，-f 才跟随）
  if kubectl -n "$NS" exec "$POD_NAME" -c dsh -- sh -c \
       'test -f /data/dsh/profiles/web/node_modules/@corp/dsh-plugin-activity/package.json' 2>/dev/null; then
    ok "插件包可解析（$(kubectl -n "$NS" exec "$POD_NAME" -c dsh -- \
      readlink -f /data/dsh/profiles/web/node_modules/@corp/dsh-plugin-activity 2>/dev/null)）"
  else
    bad "插件包解析不到：/data/dsh/profiles/web/node_modules/@corp/dsh-plugin-activity 断了"
  fi

  # ③ 端到端：心跳每 15s 刷新 updated_at，超过 90s 说明上报没到 control
  AGE=$(kubectl -n dsh-system exec dsh-postgres-0 -- psql -U dsh -d dsh -tAc \
    "SELECT COALESCE(round(extract(epoch FROM now()-updated_at))::text,'') FROM dsh_users WHERE subject='$PRINCIPAL'" 2>/dev/null | tr -d ' ')
  case "$AGE" in
    '') bad "数据库里没有 $PRINCIPAL 的记录" ;;
    *[!0-9]*) bad "查询活动时间失败：${AGE}" ;;
    *) if [ "$AGE" -le 90 ]; then
         ok "control 侧心跳 ${AGE}s 内刷新过（插件在真上报）"
       else
         bad "control 侧 ${AGE}s 没有刷新，插件可能没在跑或连不上 control"
       fi ;;
  esac
fi

say "10. 工作区与家目录落在 PVC 上（否则 pod 重启即丢文件）"
POD_NAME=$(kubectl -n "$NS" get pod -l "dsh.corp/user=$LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$POD_NAME" ]; then
  bad "找不到用户 pod，无法验证工作区持久化"
else
  # ① 静态检查：dsh 进程的 HOME 与 cwd 都必须在 /data/dsh 下
  PROC_INFO=$(kubectl -n "$NS" exec "$POD_NAME" -c dsh -- sh -c '
    for d in /proc/[0-9]*; do
      cmd=$(tr "\0" " " < "$d/cmdline" 2>/dev/null)
      case "$cmd" in
        *"dsh web"*)
          home=$(tr "\0" "\n" < "$d/environ" 2>/dev/null | sed -n "s/^HOME=//p")
          echo "$home $(readlink "$d/cwd")"
          break;;
      esac
    done' 2>/dev/null)
  PROC_HOME=${PROC_INFO%% *}
  PROC_CWD=${PROC_INFO#* }
  case "$PROC_HOME" in
    /data/dsh/*) ok "dsh 进程 HOME=${PROC_HOME}（在卷上）" ;;
    "")          bad "读不到 dsh 进程的 HOME（进程没起或 /proc 不可读）" ;;
    *)           bad "dsh 进程 HOME=${PROC_HOME}，不在卷上，重启会丢文件" ;;
  esac
  case "$PROC_CWD" in
    /data/dsh/*) ok "默认工作区根 cwd=${PROC_CWD}（在卷上）" ;;
    *)           bad "dsh 进程 cwd=${PROC_CWD}，不在卷上，默认工作区重启即丢" ;;
  esac

  # ② 破坏性验证：写标记 → 删 pod → 等新 pod → 标记仍在（需 PERSIST_TEST=1）
  if [ "${PERSIST_TEST:-0}" != "1" ]; then
    printf '  提示：设 PERSIST_TEST=1 可做"重启后文件仍在"的实测（会重启该用户的 pod）\n'
  else
    MARK="verify-persist-$$.txt"
    MARK_PATH="/data/dsh/home/$MARK"
    if kubectl -n "$NS" exec "$POD_NAME" -c dsh -- sh -c "date > '$MARK_PATH'" 2>/dev/null; then
      BEFORE=$(kubectl -n "$NS" exec "$POD_NAME" -c dsh -- sh -c "cat '$MARK_PATH'" 2>/dev/null)
      kubectl -n "$NS" delete pod "$POD_NAME" >/dev/null 2>&1 || true
      NEW_POD=""
      for _ in $(seq 1 60); do
        NEW_POD=$(kubectl -n "$NS" get pod -l "dsh.corp/user=$LABEL" --no-headers 2>/dev/null \
          | awk '$2=="1/1"{print $1}' | head -1)
        [ -n "$NEW_POD" ] && [ "$NEW_POD" != "$POD_NAME" ] && break
        sleep 2
      done
      if [ -z "$NEW_POD" ]; then
        bad "重启后 120s 内没有就绪的新 pod"
      else
        AFTER=$(kubectl -n "$NS" exec "$NEW_POD" -c dsh -- sh -c "cat '$MARK_PATH'" 2>/dev/null)
        if [ -n "$AFTER" ] && [ "$AFTER" = "$BEFORE" ]; then
          ok "pod 重启后 $MARK_PATH 仍在（${AFTER}）"
        else
          bad "pod 重启后 $MARK_PATH 丢失（写入=${BEFORE:-无}，读回=${AFTER:-无}）"
        fi
        kubectl -n "$NS" exec "$NEW_POD" -c dsh -- sh -c "rm -f '$MARK_PATH'" 2>/dev/null || true
      fi
    else
      bad "无法在 $MARK_PATH 写文件（HOME 不可写？）"
    fi
  fi
fi

printf '\n===== 失败项：%d =====\n' "$FAIL"
exit $((FAIL > 0))
