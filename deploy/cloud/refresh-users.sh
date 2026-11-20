#!/usr/bin/env bash
# 把存量用户 Deployment 的镜像对齐到当前 IMAGE_TAG。
#
# 为什么需要：
#   control 的 DSH_IMAGE 只在 control 启动时读一次，用户 Deployment 的 image
#   只在「开通 / 重新开通」时写入。所以换了 IMAGE_TAG 重跑 up.sh 之后，
#   只有 control 拿到新值，存量用户的 template 还是旧 tag。reaper 的
#   scale 0→1 只是改副本数、不会重写 template，于是新开通的用户用新镜像、
#   老用户继续跑旧镜像，两套版本混跑且没有任何报错。
#
# 用法：
#   ./deploy/cloud/refresh-users.sh                       # 只对齐已休眠的用户
#   ./deploy/cloud/refresh-users.sh --dry-run             # 只打印，不 patch
#   ./deploy/cloud/refresh-users.sh --include-running     # 运行中的也换（会重启）
#
#   目标镜像来源：DSH_IMAGE，或 DSH_REGISTRY + IMAGE_TAG（读 deploy/cloud/env）。
#
# 默认跳过运行中的用户：改 template 会触发 Recreate 重启（RWO 卷必须 Recreate），
# 正在进行的 turn 会被打断。跳过的用户会在下次重新开通时自动跟进。
set -euo pipefail
cd "$(dirname "$0")/../.."

ENV_FILE="${ENV_FILE:-deploy/cloud/env}"
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ENV_FILE"
  set +a
fi

NS_SYS="${NS_SYS:-dsh-system}"
NS_USERS="${NS_USERS:-dsh-users}"
DSH_REGISTRY="${DSH_REGISTRY:-}"
DSH_VERSION="${DSH_VERSION:-0.2.1-alpha.4}"
IMAGE_TAG="${IMAGE_TAG:-$DSH_VERSION}"

DRY_RUN=0
INCLUDE_RUNNING=0

usage() {
  sed -n '2,17p' "$0" | sed -e 's/^# \{0,1\}//'
}

for a in "$@"; do
  case "$a" in
    --dry-run)         DRY_RUN=1 ;;
    --include-running) INCLUDE_RUNNING=1 ;;
    -h|--help)         usage; exit 0 ;;
    *) echo "未知参数: ${a} （--help 看用法）" >&2; exit 2 ;;
  esac
done

IMAGE="${DSH_IMAGE:-}"
if [ -z "$IMAGE" ]; then
  if [ -z "$DSH_REGISTRY" ]; then
    echo "需要 DSH_IMAGE，或 DSH_REGISTRY + IMAGE_TAG（可写在 deploy/cloud/env）" >&2
    exit 2
  fi
  IMAGE="$DSH_REGISTRY/dsh:$IMAGE_TAG"
fi

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "目标镜像: $IMAGE"

# control 的值才是「新用户会用什么」。不一致说明 up.sh 的 set env 没跑到，
# 或者两个脚本读了不同的 env 文件 —— 这正是 tag 漂移的源头，要显式报出来。
ctrl=$(kubectl -n "$NS_SYS" get deploy dsh-control \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' 2>/dev/null \
  | sed -n 's/^DSH_IMAGE=//p' || true)
if [ -n "$ctrl" ] && [ "$ctrl" != "$IMAGE" ]; then
  echo "  WARN: control 的 DSH_IMAGE=${ctrl} 与目标不一致" >&2
  echo "        新开通的用户会用 control 的值，先重跑 ./deploy/cloud/up.sh 对齐" >&2
fi

deploys=$(kubectl -n "$NS_USERS" get deploy -l app=dsh-user \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
if [ -z "$deploys" ]; then
  echo "  ${NS_USERS} 里没有用户 Deployment，跳过"
  exit 0
fi

updated=0
skipped=0
kept=0
failed=0

for d in $deploys; do
  # 容器名从对象里读，不在脚本里写死 dsh / init-home，加 sidecar 也不会漏
  names=$(kubectl -n "$NS_USERS" get deploy "$d" \
    -o jsonpath='{.spec.template.spec.containers[*].name}{" "}{.spec.template.spec.initContainers[*].name}')
  cur=$(kubectl -n "$NS_USERS" get deploy "$d" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')
  replicas=$(kubectl -n "$NS_USERS" get deploy "$d" -o jsonpath='{.spec.replicas}')

  if [ "$cur" = "$IMAGE" ]; then
    echo "  ok      ${d}  (${cur})"
    kept=$((kept + 1))
    continue
  fi

  if [ "${replicas:-0}" != "0" ] && [ "$INCLUDE_RUNNING" != "1" ]; then
    echo "  skip    ${d}  运行中，${cur} -> ${IMAGE}（加 --include-running 强制）"
    skipped=$((skipped + 1))
    continue
  fi

  if [ "$DRY_RUN" = "1" ]; then
    echo "  would   ${d}  ${cur} -> ${IMAGE}"
    updated=$((updated + 1))
    continue
  fi

  args=()
  for n in $names; do
    args+=("$n=$IMAGE")
  done
  if kubectl -n "$NS_USERS" set image "deploy/$d" "${args[@]}" >/dev/null; then
    echo "  update  ${d}  ${cur} -> ${IMAGE}"
    updated=$((updated + 1))
  else
    echo "  FAIL    ${d}  set image 失败" >&2
    failed=$((failed + 1))
  fi
done

say "结果"
echo "  更新 ${updated}    已最新 ${kept}    跳过 ${skipped}    失败 ${failed}"
if [ "$skipped" -gt 0 ]; then
  echo "  跳过的用户下次重新开通时会自动跟进；要立刻全量对齐用 --include-running。"
fi
if [ "$DRY_RUN" = "1" ]; then
  echo "  --dry-run：没有真的 patch。"
fi
[ "$failed" -eq 0 ] || exit 1
