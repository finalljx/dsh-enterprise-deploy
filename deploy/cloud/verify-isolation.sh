#!/usr/bin/env bash
# 没有 Kata 时的隔离验收：证明用户 pod 是「受限容器」，而不是随便一个 root 容器。
#
# 有 Kata 时应该跑 verify-kata.sh（内核隔离）；这份验的是两层都该成立的部分：
#   非 root、capabilities 为空、seccomp 过滤、无 hostPath/无 docker.sock、
#   根目录不可写、出网受 NetworkPolicy 约束、ServiceAccount 无实权、资源有上限。
#
# 用法：
#   ./deploy/cloud/verify-isolation.sh
#   NS=dsh-users USER_LABEL=alice_corp ./deploy/cloud/verify-isolation.sh
set -uo pipefail

NS="${NS:-dsh-users}"
SUBJECT="${SUBJECT:-alice@corp}"
USER_LABEL="${USER_LABEL:-$(echo "$SUBJECT" | tr '@.' '__')}"
CTRL_NS="${CTRL_NS:-dsh-system}"

FAIL=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
warn(){ printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

# 在用户 pod 里跑一段 bash；失败时返回空串
inpod() {
  kubectl -n "$NS" exec "$POD" -c dsh -- bash -c "$1" 2>/dev/null || true
}

say "0. 找到用户 pod"
command -v kubectl >/dev/null || { echo "缺少 kubectl" >&2; exit 1; }
kubectl cluster-info >/dev/null 2>&1 || { echo "kubectl 连不上集群" >&2; exit 1; }
POD=$(kubectl -n "$NS" get pod -l "dsh.corp/user=$USER_LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -z "$POD" ]; then
  echo "找不到 $SUBJECT 的 pod（label dsh.corp/user=${USER_LABEL}）" >&2
  echo "先开通该用户，或确认 pod 已就绪" >&2
  exit 1
fi
READY=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].ready}')
[ "$READY" = "true" ] || { echo "pod $POD 未就绪，先排查它" >&2; exit 1; }
echo "  pod=$POD  label=$USER_LABEL"

say "1. 不是 root"
UID_IN_POD=$(inpod 'id -u')
if [ "${UID_IN_POD:-0}" != "0" ] && [ -n "$UID_IN_POD" ]; then
  ok "uid=$UID_IN_POD"
else
  bad "uid=${UID_IN_POD:-<空>}：不能以 root 运行"
fi

say "2. capabilities 全 drop"
CAPEFF=$(inpod 'grep CapEff /proc/self/status' | awk '{print $2}')
if [ -n "$CAPEFF" ] && [ "$(echo "$CAPEFF" | tr -d '0')" = "" ]; then
  ok "CapEff=${CAPEFF}（全 0）"
else
  bad "CapEff=${CAPEFF:-<空>}：仍有 capability 残留"
fi

say "3. seccomp 过滤生效"
# /proc/self/status 里有两行 Seccomp*（还有 Seccomp_filters），只取主行
SECCOMP=$(inpod 'grep "^Seccomp:" /proc/self/status' | awk '{print $2}' | head -1)
if [ "${SECCOMP:-0}" = "2" ]; then
  ok "Seccomp=2（filter 模式）"
else
  bad "Seccomp=${SECCOMP:-<空>}：期望 2"
fi

say "4. 没有 hostPath / docker.sock"
VOLS=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{range .spec.volumes[*]}{.name}{":"}{.hostPath.path}{" "}{end}')
BAD_VOLS=$(printf '%s' "$VOLS" | tr ' ' '\n' | grep -E 'hostPath|docker\.sock|:/var/run' || true)
if [ -z "$BAD_VOLS" ]; then
  ok "卷都是 pvc/secret/projected，没有宿主路径"
else
  bad "发现宿主路径挂载：$BAD_VOLS"
fi

say "5. 根目录不可写、/tmp 可写"
if inpod 'touch /dsh-root-probe' >/dev/null 2>&1 && [ -n "$(inpod 'ls /dsh-root-probe')" ]; then
  bad "uid=${UID_IN_POD} 能写根目录（rootfs 未受保护）"
else
  ok "根目录对运行用户只读"
fi
if [ -n "$(inpod 'touch /tmp/dsh-probe && echo ok')" ]; then
  ok "/tmp 可写（临时空间，pod 重启即丢）"
  inpod 'rm -f /tmp/dsh-probe' >/dev/null
else
  warn "/tmp 不可写：某些工具（编译、包管理器）可能失败"
fi

say "6. 出网：公网可用，集群内部被 NetworkPolicy 收口"
PUB=$(inpod 'curl -sS -m 8 -o /dev/null -w "%{http_code}" https://api.deepseek.com/' | tail -c 3)
if [ "$PUB" = "401" ] || [ "$PUB" = "200" ] || [ "$PUB" = "403" ]; then
  ok "公网 443 可达（HTTP ${PUB}），模型网关可用"
else
  bad "公网 443 不可达（HTTP ${PUB:-超时}）：检查出网策略与 NAT"
fi

# API server 的 service IP：从 kubernetes 这个 svc 拿
API_IP=$(kubectl -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
if [ -n "$API_IP" ]; then
  API_CODE=$(inpod "curl -sS -m 5 -o /dev/null -w '%{http_code}' https://$API_IP:443" | tail -c 3)
  if [ "$API_CODE" = "000" ] || [ -z "$API_CODE" ]; then
    ok "访问 $API_IP:443 被阻断（容器拿不到 K8s API）"
  else
    bad "能连 K8s API server（HTTP ${API_CODE}）：出网策略放得太宽"
  fi
else
  warn "拿不到 API server IP，跳过"
fi

PG_IP=$(kubectl -n "$CTRL_NS" get svc dsh-postgres -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
if [ -n "$PG_IP" ]; then
  PG_CODE=$(inpod "curl -sS -m 4 -o /dev/null -w '%{http_code}' telnet://$PG_IP:5432" | tail -c 3)
  if [ "$PG_CODE" = "000" ] || [ -z "$PG_CODE" ]; then
    ok "访问 postgres:5432 被阻断（只允许 control:8080）"
  else
    bad "能连 postgres（$PG_IP:5432）：用户 pod 不该有数据库网络权限"
  fi
fi

say "7. ServiceAccount 没有实权"
SA="system:serviceaccount:$NS:dsh-workload"
# 只关心「资源权限」：Non-Resource URL（/healthz、/api 发现）是 system:discovery 的默认能力，
# 第一列为空，不算实权。
# 发现类权限是 Non-Resource URL，输出行以空格开头（第一列为空）；按行首是否有空白区分。
POWER=$(kubectl auth can-i --list --as="$SA" 2>/dev/null \
  | awk 'NR>1 && $0 !~ /^[[:space:]]/ && $1 !~ /^selfsubject/' || true)
if [ -z "$POWER" ]; then
  ok "dsh-workload 只有 selfsubject* 与 API 发现权限"
else
  bad "dsh-workload 有额外权限：$(echo "$POWER" | head -3 | tr '\n' ' ')"
fi

say "8. 资源上限存在"
LIM=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits}')
if echo "$LIM" | grep -q '"cpu"'; then
  ok "limits=$LIM"
else
  bad "没有设置 limits：一个用户可以把节点吃满"
fi

say "结论"
if [ "$FAIL" -eq 0 ]; then
  printf '  \033[32m容器级隔离成立\033[0m：非 root、cap 为空、seccomp 过滤、无宿主挂载、网络收口、资源有界\n'
  printf '  未覆盖：\033[33m内核隔离\033[0m（需要 Kata，见 verify-kata.sh）\n'
  exit 0
fi
printf '  \033[31m%d 项不通过\033[0m\n' "$FAIL"
exit 1
