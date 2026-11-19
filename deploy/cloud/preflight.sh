#!/usr/bin/env bash
# 云主机（或任意 Linux 裸金属）部署前的宿主体检。
#
# 目标：在花钱装 k3s/Kata 之前，把「这台机器根本跑不了」的情况挡掉。
# 每条检查都打印原因；有 FAIL 就以非零退出，不要带着隐患往下走。
#
# 用法：
#   sudo ./deploy/cloud/preflight.sh                # 完整检查
#   SKIP_EGRESS=1 ./deploy/cloud/preflight.sh       # 内网机器跳过出网检查
set -uo pipefail

# 只支持 Linux：依赖 /proc、/sys 与 cgroup。在 macOS 上跑只会得到一堆误导性的 FAIL。
if [ ! -d /proc/1 ]; then
  echo "此脚本只支持 Linux（需要 /proc、/sys 与 cgroup），当前系统不是。" >&2
  exit 2
fi

FAIL=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# ALLOW_NO_KVM=1：接受"只跑 runc、验不了微虚拟机隔离"的降级部署。
# 普通 ECS 就是这种情况——功能全都能验，只少内核级隔离。
ALLOW_NO_KVM="${ALLOW_NO_KVM:-0}"

say "1. 硬件虚拟化（Kata 的硬前提）"
if [ -e /dev/kvm ]; then
  if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    ok "/dev/kvm 存在且可读写"
  else
    bad "/dev/kvm 存在但不可读写（把运行用户加进 kvm 组，或检查设备权限）"
  fi
else
  if [ "$ALLOW_NO_KVM" = "1" ]; then
    warn "/dev/kvm 不存在：按降级模式继续，用户 pod 将跑 runc（无微虚拟机隔离）"
  else
    bad "/dev/kvm 不存在：普通 ECS 实例没有嵌套虚拟化，Kata 起不来。换弹性裸金属（ecs.ebm*），或 ALLOW_NO_KVM=1 降级"
  fi
fi

FLAGS=$(grep -c -E '(^| )(vmx|svm)( |$)' /proc/cpuinfo 2>/dev/null || true)
if [ "${FLAGS:-0}" -gt 0 ]; then
  ok "CPU 暴露了虚拟化扩展（vmx/svm 出现 ${FLAGS} 次）"
else
  if [ "$ALLOW_NO_KVM" = "1" ]; then
    warn "CPU 未暴露 vmx/svm（降级模式，忽略）"
  else
    bad "CPU 未暴露 vmx/svm：确认实例支持嵌套虚拟化，或在控制台开启该特性"
  fi
fi

if grep -qi hypervisor /proc/cpuinfo 2>/dev/null; then
  warn "检测到 hypervisor 标志：这台是虚拟机，需要宿主开启嵌套虚拟化"
fi

say "2. 内核与 cgroup"
KREL=$(uname -r)
KMAJ=$(echo "$KREL" | cut -d. -f1)
KMIN=$(echo "$KREL" | cut -d. -f2)
if [ "$KMAJ" -gt 5 ] || { [ "$KMAJ" -eq 5 ] && [ "$KMIN" -ge 4 ]; }; then
  ok "内核 ${KREL}（≥5.4，virtiofs 与 Kata 都支持）"
else
  bad "内核 $KREL 偏旧：Kata + virtiofs 建议 ≥5.4"
fi

if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  ok "cgroup v2"
else
  warn "cgroup v1（k3s 可用，但部分发行版会告警）"
fi

say "3. 系统设置"
SWAP=$(swapon --show=NAME --noheadings 2>/dev/null | wc -l | tr -d ' ')
if [ "$SWAP" = "0" ]; then
  ok "swap 已关闭"
else
  bad "swap 开着：kubelet 默认要求关闭，执行 swapoff -a 并注释 /etc/fstab 里的 swap 行"
fi

if lsmod 2>/dev/null | grep -q '^br_netfilter'; then
  ok "br_netfilter 已加载"
else
  warn "br_netfilter 未加载：modprobe br_netfilter（重启后需持久化）"
fi

if command -v chronyc >/dev/null 2>&1 && chronyc tracking >/dev/null 2>&1; then
  ok "chrony 时间同步正常"
elif systemctl is-active --quiet systemd-timesyncd 2>/dev/null; then
  ok "systemd-timesyncd 在跑"
else
  warn "没检测到时间同步：cookie/ticket 都有时间窗，务必装 chrony"
fi

say "4. 资源"
CORES=$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null || echo 0)
MEM_GB=$(awk '/MemTotal/ {printf "%d", $2/1024/1024}' /proc/meminfo 2>/dev/null || echo 0)
if [ "$CORES" -ge 8 ]; then
  ok "CPU ${CORES} 核"
else
  warn "CPU ${CORES} 核：控制面 + 1~2 个用户 pod 会紧（用户 pod limit 是 4C/8Gi）"
fi
if [ "$MEM_GB" -ge 16 ]; then
  ok "内存 ${MEM_GB} GiB"
else
  bad "内存 ${MEM_GB} GiB：单用户 pod limit 就到 8Gi，至少 16 GiB"
fi

DATA_FREE_GB=$(df -BG --output=avail /var 2>/dev/null | tail -1 | tr -dc '0-9' || true)
DATA_FREE_GB=${DATA_FREE_GB:-0}
if [ "${DATA_FREE_GB:-0}" -ge 100 ]; then
  ok "/var 可用 ${DATA_FREE_GB} GiB"
else
  bad "/var 可用 ${DATA_FREE_GB:-0} GiB：镜像 + 用户卷需要 ≥100 GiB，且卷是 30 GiB/人"
fi

say "5. 端口与依赖命令"
for p in 22 80 443 6443; do
  if ss -lntH 2>/dev/null | awk '{print $4}' | grep -q ":${p}$"; then
    bad "端口 ${p} 已被占用"
  else
    ok "端口 ${p} 空闲"
  fi
done

for c in curl tar; do
  command -v "$c" >/dev/null 2>&1 && ok "已安装 $c" || bad "缺少 $c"
done

say "6. 出网（拉镜像、调模型、装 k3s 都要）"
if [ "${SKIP_EGRESS:-0}" = "1" ]; then
  warn "SKIP_EGRESS=1，跳过"
else
  check_url() {
    local url="$1" label="$2"
    if curl -fsS -o /dev/null -m 8 "$url"; then
      ok "$label"
    else
      bad "$label 不通：没有 NAT 网关/EIP 时，镜像和模型都会"看起来像密钥失效""
    fi
  }
  check_url https://get.k3s.io "get.k3s.io"
  check_url https://registry-1.docker.io/v2/ "Docker Hub（或换 ACR）"
  check_url https://api.deepseek.com/ "api.deepseek.com（模型网关）"
fi

say "结论"
if [ "$FAIL" -eq 0 ]; then
  printf '  \033[32m全部通过\033[0m，可以继续 install-k3s.sh / install-kata.sh\n'
  exit 0
fi
printf '  \033[31m%d 项不通过\033[0m，先解决再往下走；带 FAIL 的项在云上通常意味着实例规格选错了\n' "$FAIL"
exit 1
