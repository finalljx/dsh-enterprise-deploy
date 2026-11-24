#!/usr/bin/env bash
# 把已有用户的镜像原地切到新版本，不需要删除用户。
#
#   ./deploy/cloud/rollout-user-image.sh              # 只看会改什么（默认 dry-run）
#   ./deploy/cloud/rollout-user-image.sh --apply      # 真正执行
#   ./deploy/cloud/rollout-user-image.sh --apply --wait
#   IMAGE_TAG=0.1.1 ./deploy/cloud/rollout-user-image.sh --apply
#
# 关键：control 的 DSH_IMAGE 必须一起改。否则以后重新开通某个用户时，
# provision 会用旧的 DSH_IMAGE 把镜像 PATCH 回去，等于升级被回滚。
set -euo pipefail
cd "$(dirname "$0")/../.."

# 与 up.sh 一致：配置文件优先；ENV_FILE=/dev/null 可不读
ENV_FILE="${ENV_FILE:-deploy/cloud/env}"
[ -f "$ENV_FILE" ] && { set -a; . "$ENV_FILE"; set +a; }

NS_SYS="${NS_SYS:-dsh-system}"
NS_USERS="${NS_USERS:-dsh-users}"
DSH_REGISTRY="${DSH_REGISTRY:?需要 DSH_REGISTRY，可写在 deploy/cloud/env}"
DSH_VERSION="${DSH_VERSION:-0.2.1-alpha.4}"
IMAGE_TAG="${IMAGE_TAG:-$DSH_VERSION}"
REF="${DSH_REGISTRY}/dsh:${IMAGE_TAG}"

APPLY=0 WAIT=0 SET_CONTROL=1
for arg in "$@"; do
  case "$arg" in
    --apply)       APPLY=1 ;;
    --wait)        WAIT=1 ;;
    --no-control)  SET_CONTROL=0 ;;
    -h|--help)     sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "目标镜像"
echo "  $REF"
if [ "$APPLY" = 0 ]; then
  echo "  （dry-run，加 --apply 才会执行）"
fi

say "control 的 DSH_IMAGE（决定新用户用什么镜像）"
CUR_CTRL=$(kubectl -n "$NS_SYS" get deploy dsh-control \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | sed -n 's/^DSH_IMAGE=//p')
echo "  当前: ${CUR_CTRL:-<未设置>}"
echo "  目标: $REF"
if [ "$CUR_CTRL" != "$REF" ]; then
  if [ "$APPLY" = 1 ] && [ "$SET_CONTROL" = 1 ]; then
    kubectl -n "$NS_SYS" set env deploy/dsh-control DSH_IMAGE="$REF" >/dev/null
    kubectl -n "$NS_SYS" rollout status deploy/dsh-control --timeout=180s | tail -1
  else
    echo "  → 会用新的 DSH_IMAGE 更新 control（不改的话，重新开通用户会把镜像改回旧的）"
  fi
else
  echo "  → 已一致，无需改"
fi

say "用户 Deployment"
kubectl -n "$NS_USERS" get deploy -l app=dsh-user \
  -o custom-columns='NAME:.metadata.name,DESIRED:.spec.replicas,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[0].image' \
  --no-headers | awk -v ref="$REF" '{
    image = $4
    mark  = (image == ref) ? "  = 已是目标" : "  → 将切换"
    printf "  %-16s replicas=%-2s ready=%-4s %s%s\n", $1, $2, ($3=="<none>"?"0":$3), image, mark
  }'

if [ "$APPLY" = 0 ]; then
  say "未执行"
  echo "  确认无误后加 --apply"
  exit 0
fi

say "切换"
kubectl -n "$NS_USERS" set image deploy -l app=dsh-user dsh="$REF"

if [ "$WAIT" = 1 ]; then
  say "等待滚动完成"
  # 休眠用户（replicas=0）没有 Pod 可等，会自动跳过
  for d in $(kubectl -n "$NS_USERS" get deploy -l app=dsh-user -o name); do
    desired=$(kubectl -n "$NS_USERS" get "$d" -o jsonpath='{.spec.replicas}')
    if [ "${desired:-0}" = "0" ]; then
      printf '  %s: 休眠中（replicas=0），下次唤醒时使用新镜像\n' "${d#deployment.apps/}"
      continue
    fi
    kubectl -n "$NS_USERS" rollout status "$d" --timeout=300s | tail -1
  done
fi

say "结果"
kubectl -n "$NS_USERS" get deploy -l app=dsh-user \
  -o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[0].image' \
  --no-headers
