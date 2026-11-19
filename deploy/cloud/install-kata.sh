#!/usr/bin/env bash
# 在已有 K8s 节点上安装 Kata（kata-deploy），并把 RuntimeClass 名字对齐到本仓库的约定。
#
# 前置：preflight.sh 全绿；节点是裸金属或已开嵌套虚拟化。
#
# 用法：
#   ./deploy/cloud/install-kata.sh
#   KATA_REF=3.22.0 ./deploy/cloud/install-kata.sh     # 钉一个 kata-containers 版本
set -euo pipefail

KATA_REF="${KATA_REF:-main}"
EXPECTED_RC="${EXPECTED_RC:-kata-clh-runtime-rs}"
NS=kube-system

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "0. 前置检查"
command -v kubectl >/dev/null || { echo "缺少 kubectl" >&2; exit 1; }
kubectl cluster-info >/dev/null 2>&1 || { echo "kubectl 连不上集群" >&2; exit 1; }
if [ ! -e /dev/kvm ]; then
  echo "本机没有 /dev/kvm，装了也跑不起来。先跑 preflight.sh 确认实例规格。" >&2
  exit 1
fi

say "1. 安装 kata-deploy（ref=${KATA_REF}）"
# 注意：main 会随时间变化。生产请用 KATA_REF 钉到具体 release tag。
kubectl apply -f "https://raw.githubusercontent.com/kata-containers/kata-containers/${KATA_REF}/tools/packaging/kata-deploy/kata-deploy/base/kata-deploy.yaml"
kubectl -n "$NS" wait --for=condition=Ready -l name=kata-deploy pod --timeout=15m

say "2. 等 shim 注册进节点"
# kata-deploy 在节点上装 shim 并重启 containerd；RuntimeClass 由它自己创建。
for _ in $(seq 1 60); do
  if kubectl get runtimeclass "$EXPECTED_RC" >/dev/null 2>&1; then break; fi
  sleep 5
done

say "3. 核对 RuntimeClass 名字"
AVAILABLE=$(kubectl get runtimeclass -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep '^kata' || true)
if [ -z "$AVAILABLE" ]; then
  echo "没有任何 kata RuntimeClass：看 kata-deploy 的 DaemonSet 日志与节点 /opt/kata 是否存在" >&2
  kubectl -n "$NS" logs -l name=kata-deploy --tail=30 2>/dev/null || true
  exit 1
fi
printf '  已安装的 kata RuntimeClass:\n'
printf '    %s\n' $AVAILABLE

# 本仓库清单写死 kata-clh-runtime-rs（Kata 4.x Rust runtime）。
# 只有 Go 版 kata-clh 时不要用它——上游已弃用，行为与本仓库的 overhead 配置不一致。
if ! echo "$AVAILABLE" | grep -qx "$EXPECTED_RC"; then
  cat >&2 <<EOF
清单期望的 RuntimeClass "$EXPECTED_RC" 不存在。
  1) 确认 kata-deploy 的 shims 配置包含 clh-runtime-rs（Kata 4.x 默认装）；
  2) 或者退而求其次，把 control 的 DSH_RUNTIME_CLASS 改成实际存在的名字：
       kubectl -n dsh-system set env deploy/dsh-control DSH_RUNTIME_CLASS=<上表里的名字>
  3) 不要用不带 runtime-rs 后缀的 kata-clh（Go 版已弃用）。
EOF
  exit 1
fi
echo "  ✅ $EXPECTED_RC 存在，与本仓库清单一致"

say "4. 给节点打标签与污点"
# 02-runtimeclass.yaml 的 nodeSelector 就是靠这个标签匹配节点的。
for node in $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  lbl=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.katacontainers\.io/kata-runtime}' 2>/dev/null || true)
  if [ "$lbl" = "true" ]; then
    echo "  $node: 已有 kata-runtime 标签"
  else
    kubectl label node "$node" katacontainers.io/kata-runtime=true --overwrite >/dev/null
    echo "  $node: 补上 kata-runtime 标签（kata-deploy 没打成功，检查节点日志）"
  fi
done

# 与 checklist 一致：Kata 节点池专用，不让普通负载落进来。
# 用户 pod 的 Deployment 已经带了对应 toleration（见 control/provision.go）。
for node in $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  kubectl taint node "$node" dsh.corp/dedicated=dsh:NoSchedule --overwrite >/dev/null 2>&1 || true
done
echo "  已加污点 dsh.corp/dedicated=dsh:NoSchedule（只允许带 toleration 的 DSH 负载）"

say "5. 下一步"
cat <<EOF
  ./deploy/cloud/verify-kata.sh        # 用内核对比证明真的在微虚拟机里
然后回到 up.sh 部署 DSH，DSH_RUNTIME_CLASS 保持 $EXPECTED_RC 即可。
EOF
