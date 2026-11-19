#!/usr/bin/env bash
# 在 ECS 本机验证「ALB 终止 TLS」方案。
#
# ALB 做的事只有两件：把 HTTP 请求转到节点端口，并原样带上 Host。
# 所以这里就用 curl 模拟这两件事，不依赖 ALB 是否已经配好。
#
# 用法：
#   ./deploy/cloud/verify-alb.sh                 # 自动探测各节点的 NodePort
#   HOST=alice.dsh.corp ./deploy/cloud/verify-alb.sh
#   TARGET=http://127.0.0.1:18080 ./deploy/cloud/verify-alb.sh   # 用 port-forward 验逻辑
#
# 它验不了的（必须真 ALB）：证书链、DNS、Host 改写、ALB 健康检查判定。
set -uo pipefail

NS="${NS:-dsh-system}"
SUBJECT="${SUBJECT:-alice@corp}"
SLUG="${SUBJECT%%@*}"
HOST="${HOST:-$SLUG.dsh.corp}"
TARGET="${TARGET:-}"        # 覆盖目标地址；不填则逐个节点探测

FAIL=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
say()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "0. 集群侧配置"
command -v kubectl >/dev/null || { echo "缺少 kubectl" >&2; exit 1; }
kubectl cluster-info >/dev/null 2>&1 || { echo "kubectl 连不上集群" >&2; exit 1; }

TYPE=$(kubectl -n "$NS" get svc dsh-router -o jsonpath='{.spec.type}' 2>/dev/null)
ETP=$(kubectl -n "$NS" get svc dsh-router -o jsonpath='{.spec.externalTrafficPolicy}' 2>/dev/null)
NP=$(kubectl -n "$NS" get svc dsh-router -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null)
echo "  Service: type=$TYPE externalTrafficPolicy=$ETP nodePort=$NP"
[ "$TYPE" = "NodePort" ] || bad "Service 不是 NodePort（ALB_MODE=1 会自动改）"
[ "$ETP" = "Local" ] || warn "externalTrafficPolicy=${ETP}：Local 才能保证探测语义，建议设成 Local"

PS=$(kubectl -n "$NS" get deploy dsh-router -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="PUBLIC_SCHEME")].value}')
CS=$(kubectl -n "$NS" get deploy dsh-router -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="COOKIE_SECURE")].value}')
echo "  router: PUBLIC_SCHEME=$PS COOKIE_SECURE=$CS"
[ "$PS" = "https" ] || bad "PUBLIC_SCHEME=${PS}（ALB 终止 TLS 时必须是 https，否则登录会跳到 http）"
[ "$CS" = "true" ]  || bad "COOKIE_SECURE=${CS}（浏览器在 https 下，必须是 true）"

say "1. 节点可达性（ALB 后端候选）"
if [ -n "$TARGET" ]; then
  BASES=("$TARGET")
  echo "  使用 TARGET=$TARGET"
else
  BASES=()
  while read -r name ip; do
    [ -n "$ip" ] || continue
    BASES+=("http://$ip:$NP")
    echo "  节点 $name → http://$ip:$NP"
  done < <(kubectl get nodes -o wide --no-headers | awk '{print $1, $6}')
fi

for base in "${BASES[@]}"; do
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' -H "Host: $HOST" "$base/healthz" 2>/dev/null || echo 000)
  if [ "$code" = "200" ]; then
    ok "$base/healthz → 200（ALB 可以把这个后端加进来）"
  else
    warn "$base/healthz → ${code}（该节点没有 router pod 时，Local 策略下就会这样）"
  fi
done

say "2. Host 透传与 https 重定向"
if [ -z "${BASES[0]:-}" ]; then
  echo "  没有可用目标" >&2
  exit 1
fi
BASE="${BASES[0]}"

loc=$(curl -s -o /dev/null -D - -m 5 -H "Host: $HOST" "$BASE/" 2>/dev/null | tr -d '\r' | awk 'tolower($1)=="location:"{print $2}' | head -1)
code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' -H "Host: $HOST" "$BASE/" 2>/dev/null)
echo "  GET / → $code, Location: ${loc:-<无>}"
if [ "$code" = "302" ]; then
  ok "router 认得这个 Host（302 到登录流程）"
else
  bad "GET / 返回 ${code}：Host 没透传，或 router 侧没这个用户的记录"
fi
# Location 通常形如 /_dev/sso?return=https%3A%2F%2Falice...%2F_auth%2Fcallback
# 所以要看 return= 参数的协议，而不是 Location 本身
case "$loc" in
  "") warn "没有 Location 头，跳过" ;;
  *"return=https%3A%2F%2F"*|*"return=https://"*)
      ok "回调地址用 https（PUBLIC_SCHEME 生效）" ;;
  *"return=http%3A%2F%2F"*|*"return=http://"*)
      bad "回调地址用 http：ALB 终止 TLS 后浏览器会被降级，检查 PUBLIC_SCHEME" ;;
  *)
      if printf '%s' "$loc" | grep -q '^https://'; then
        ok "重定向到 https"
      else
        warn "无法判断协议，Location=$loc"
      fi ;;
esac

say "3. 会话 cookie 的 Secure 属性"
sso=$(curl -s -o /dev/null -D - -m 5 -H "Host: $HOST" \
  "$BASE/_dev/sso?return=$(printf 'http://%s/_auth/callback' "$HOST" | sed 's#/#%2F#g')" 2>/dev/null | tr -d '\r')
ticket=$(printf '%s' "$sso" | awk 'tolower($1)=="location:"{print $2}' | sed -n 's/.*ticket=\([^&]*\).*/\1/p' | head -1)
if [ -z "$ticket" ]; then
  warn "拿不到 dev 登录 ticket（DEV_SUBJECT 可能没开，KEEP_DEV_LOGIN=0）"
else
  hdr=$(curl -s -o /dev/null -D - -m 5 -H "Host: $HOST" "$BASE/_auth/callback?ticket=$ticket" 2>/dev/null | tr -d '\r')
  if printf '%s' "$hdr" | grep -qi 'set-cookie:.*dsh_sess.*secure'; then
    ok "会话 cookie 带 Secure"
  else
    bad "会话 cookie 没有 Secure：浏览器在 https 下会拒绝回传，表现为登录死循环"
  fi
fi

say "4. 结论"
if [ "$FAIL" -eq 0 ]; then
  printf '  \033[32m本机这一跳没问题\033[0m：NodePort 可达、Host 透传、https 重定向、cookie Secure\n'
  cat <<'EOF'

  剩下必须在真 ALB 上确认（本机验不了）：
    - ALB 证书与域名匹配，浏览器无告警
    - ALB 转发时保留了原始 Host（没配"后端主机名"改写）
    - 健康检查 GET /healthz 判定为健康，且未健康的后端被正确摘除
    - 只把有 router pod 的节点加进后端组（Local 策略）
EOF
  exit 0
fi
printf '  \033[31m%d 项不通过\033[0m\n' "$FAIL"
exit 1
