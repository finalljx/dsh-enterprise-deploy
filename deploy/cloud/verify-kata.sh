#!/usr/bin/env bash
# 证明「pod 真的跑在微虚拟机里」，而不是 RuntimeClass 字段写对了、实际退回 runc。
#
# 判据（任一不满足就失败）：
#   1. RuntimeClass 存在且 pod 用上了它
#   2. guest 内核与宿主内核不同        ← 最强证据，也是 verify.sh 用的判据
#   3. guest 内看不到 /dev/kvm         ← 说明 pod 不在宿主内核里
#   4. 反向对照：同节点的 runc pod 内核 == 宿主内核
#   5. 节点侧 crictl 里 runtimeType 是 kata（能取到才检查）
#
# 用法：
#   ./deploy/cloud/verify-kata.sh
#   RC=kata-clh ./deploy/cloud/verify-kata.sh        # 换成实际存在的 RuntimeClass
#   IMAGE=registry.cn-hangzhou.aliyuncs.com/x/busybox:1.36 ./deploy/cloud/verify-kata.sh
set -uo pipefail

RC="${RC:-kata-clh-runtime-rs}"
IMAGE="${IMAGE:-busybox:1.36}"
NS="${NS:-default}"
KATA_POD=dsh-kata-probe
RUNC_POD=dsh-runc-probe
TIMEOUT="${TIMEOUT:-180}"

FAIL=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

cleanup() {
  kubectl -n "$NS" delete pod "$KATA_POD" "$RUNC_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. 环境"
command -v kubectl >/dev/null || { echo "缺少 kubectl" >&2; exit 1; }
kubectl cluster-info >/dev/null 2>&1 || { echo "kubectl 连不上集群" >&2; exit 1; }
echo "  RuntimeClass=$RC  probe image=$IMAGE"

say "1. RuntimeClass 是否存在"
if kubectl get runtimeclass "$RC" >/dev/null 2>&1; then
  ok "runtimeclass/$RC 存在"
  HANDLER=$(kubectl get runtimeclass "$RC" -o jsonpath='{.handler}')
  echo "  handler=$HANDLER"
else
  bad "runtimeclass/$RC 不存在。先跑 install-kata.sh；现有：$(kubectl get runtimeclass -o name | tr '\n' ' ')"
  printf '\n\033[31m无法继续\033[0m\n'; exit 1
fi

say "2. 拉起 kata 探针"
cleanup
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $KATA_POD
  namespace: $NS
spec:
  runtimeClassName: $RC
  restartPolicy: Never
  tolerations:
    - { key: dsh.corp/dedicated, operator: Equal, value: dsh, effect: NoSchedule }
  containers:
    - name: c
      image: $IMAGE
      command: ["sleep", "3600"]
YAML

if kubectl -n "$NS" wait --for=condition=Ready "pod/$KATA_POD" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
  ok "kata 探针 Ready"
else
  bad "kata 探针没有就绪（${TIMEOUT}s）——shim 没装好或镜像拉不到，看下面的事件"
  kubectl -n "$NS" describe pod "$KATA_POD" 2>/dev/null | sed -n '/Events:/,$p' | tail -12
  printf '\n\033[31m无法继续\033[0m\n'; exit 1
fi

say "3. guest 内核 vs 宿主内核"
NODE=$(kubectl -n "$NS" get pod "$KATA_POD" -o jsonpath='{.spec.nodeName}')
HOST_KERNEL=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.kernelVersion}')
GUEST_KERNEL=$(kubectl -n "$NS" exec "$KATA_POD" -- uname -r 2>/dev/null || true)
echo "  节点 $NODE"
echo "  宿主 $HOST_KERNEL"
echo "  guest $GUEST_KERNEL"
if [ -n "$GUEST_KERNEL" ] && [ "$GUEST_KERNEL" != "$HOST_KERNEL" ]; then
  ok "内核不同，pod 跑在自己的内核里"
else
  bad "guest 内核与宿主相同或取不到：RuntimeClass 可能回退成了 runc"
fi

say "4. guest 内不应有 /dev/kvm"
if kubectl -n "$NS" exec "$KATA_POD" -- test -e /dev/kvm 2>/dev/null; then
  bad "guest 里能看见 /dev/kvm，说明不是微虚拟机"
else
  ok "guest 内无 /dev/kvm"
fi

say "5. 反向对照（同节点的 runc pod）"
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $RUNC_POD
  namespace: $NS
spec:
  nodeName: $NODE
  restartPolicy: Never
  tolerations:
    - { key: dsh.corp/dedicated, operator: Equal, value: dsh, effect: NoSchedule }
  containers:
    - name: c
      image: $IMAGE
      command: ["sleep", "3600"]
YAML
if kubectl -n "$NS" wait --for=condition=Ready "pod/$RUNC_POD" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
  RUNC_KERNEL=$(kubectl -n "$NS" exec "$RUNC_POD" -- uname -r 2>/dev/null || true)
  echo "  runc 内核 $RUNC_KERNEL"
  if [ "$RUNC_KERNEL" = "$HOST_KERNEL" ]; then
    ok "runc pod 与宿主同内核，说明第 3 步的差异确实来自 RuntimeClass"
  else
    bad "runc pod 内核也跟宿主不同，判据不成立（节点本身有问题？）"
  fi
else
  bad "runc 对照 pod 没起来，无法验证判据有效性"
fi

say "6. 节点侧 runtimeType（能取到才检查）"
CONTAINER_ID=$(kubectl -n "$NS" get pod "$KATA_POD" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null | sed 's#^[a-z]*://##')
CRICTL=""
for cand in "crictl" "crictl -r unix:///run/k3s/containerd/containerd.sock" "sudo crictl"; do
  if $cand version >/dev/null 2>&1; then CRICTL="$cand"; break; fi
done
if [ -n "$CONTAINER_ID" ] && [ -n "$CRICTL" ]; then
  RT=$($CRICTL inspect "$CONTAINER_ID" 2>/dev/null | grep -oE '"runtimeType":[^,]*' | head -1 || true)
  if [ -n "$RT" ]; then
    echo "  $RT"
    case "$RT" in
      *kata*) ok "containerd 记录的是 kata runtime" ;;
      *) bad "containerd 记录的 runtimeType 不是 kata：$RT" ;;
    esac
  else
    echo "  （crictl 里没解析出 runtimeType，跳过）"
  fi
else
  echo "  （本机没有 crictl 或拿不到容器 ID，跳过；不影响前面的结论）"
fi

say "结论"
if [ "$FAIL" -eq 0 ]; then
  printf '  \033[32mKata 生效\033[0m：guest 内核 %s ≠ 宿主 %s，且 runc 对照符合预期\n' "$GUEST_KERNEL" "$HOST_KERNEL"
  exit 0
fi
printf '  \033[31m%d 项不通过\033[0m：这就是「清单写对了但实际没隔离」的典型表现\n' "$FAIL"
exit 1
