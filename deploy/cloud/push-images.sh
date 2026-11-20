#!/usr/bin/env bash
# 本地构建并推送镜像到 ACR。
#
# 为什么单独一个脚本：
#   - 集群在 VPC 内，清单里必须写【VPC 域名】；
#   - 你的 Mac 解析不了 VPC 域名，push 必须走【公网域名】。
#   两者指向同一实例，registry 只按 repo 路径存，所以「公网 push + VPC pull」是同一份镜像。
#
# 用法：
#   ./deploy/cloud/push-images.sh                 # 构建 + 推送
#   SKIP_PUSH=1 ./deploy/cloud/push-images.sh     # 只构建和打 tag，不推
#   ACR_PUBLIC=<公网端点> ./deploy/cloud/push-images.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

[ -f deploy/cloud/env ] && { set -a; . ./deploy/cloud/env; set +a; }

VPC_PREFIX="${DSH_REGISTRY:?需要 DSH_REGISTRY（VPC 端点），可写在 deploy/cloud/env}"
DSH_VERSION="${DSH_VERSION:-0.2.1-alpha.4}"   # 镜像里装的 DSH 版本
IMAGE_TAG="${IMAGE_TAG:-$DSH_VERSION}"          # 镜像 tag，可与版本不同
REPO_TAG="${REPO_TAG:-0.1.0}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
MODEL_BASE_URL="${MODEL_BASE_URL:-https://api.deepseek.com/anthropic}"
NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmmirror.com}"
# ECS 基本都是 amd64；在 Apple Silicon 上构建必须显式指定，否则推上去是 arm64 镜像
PLATFORM="${PLATFORM:-linux/amd64}"

# VPC 端点去掉 -vpc 就是公网端点；拿不到就用 ACR_PUBLIC 显式指定
PUB_PREFIX="${ACR_PUBLIC:-${VPC_PREFIX/-vpc./.}}"
PUB_HOST="${PUB_PREFIX%%/*}"

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "0. 端点"
echo "  push（公网）: $PUB_PREFIX"
echo "  pull（VPC） : $VPC_PREFIX"
if [ "$PUB_PREFIX" = "$VPC_PREFIX" ]; then
  echo "  WARN: 两个端点相同。如果这是 VPC 域名，本地 push 会解析失败——用 ACR_PUBLIC 指定公网端点。" >&2
fi

say "1. 登录 $PUB_HOST"
if [ -z "${SKIP_PUSH:-}" ]; then
  docker login "$PUB_HOST"
fi

# 只构建改动过的：IMAGES="dsh-router" 或 IMAGES="dsh-router dsh"，默认全部
IMAGES="${IMAGES:-dsh-control dsh-router dsh-reaper dsh}"
want() { case " $IMAGES " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# 同名 tag 覆盖推送时节点不会重新拉取（IfNotPresent），「升级」会静默失效。
# 放在构建之前：省得跑完几分钟的多阶段构建才失败。
say "2. 检查镜像 tag 是否已被占用（${IMAGE_TAG}）"
if [ -n "${SKIP_PUSH:-}" ]; then
  echo "  跳过（SKIP_PUSH=1）"
elif want dsh; then
  hack/check-tag-unused.sh "$PUB_PREFIX/dsh:$IMAGE_TAG"
else
  echo "  跳过（本次不构建 dsh）"
fi

say "3. 构建（${IMAGES}）"
want dsh-control && docker build --platform "$PLATFORM" -f control/Dockerfile --build-arg GOPROXY="$GOPROXY" \
  -t "$PUB_PREFIX/dsh-control:$REPO_TAG" ./control
want dsh-router  && docker build --platform "$PLATFORM" -f router/Dockerfile \
  -t "$PUB_PREFIX/dsh-router:$REPO_TAG" ./router
want dsh-reaper  && docker build --platform "$PLATFORM" -f reaper/Dockerfile \
  -t "$PUB_PREFIX/dsh-reaper:$REPO_TAG" ./reaper
# 用户镜像必须用 Dockerfile.dev：image/Dockerfile 依赖尚未发布的活动插件
want dsh && docker build --platform "$PLATFORM" -f Dockerfile.dev --build-arg MODEL_BASE_URL="$MODEL_BASE_URL" \
  --build-arg NPM_REGISTRY="$NPM_REGISTRY" \
  --build-arg DSH_VERSION="$DSH_VERSION" \
  -t "$PUB_PREFIX/dsh:$IMAGE_TAG" .

say "4. 校验目标架构"
ARCH_FAIL=0
for ref in "dsh-control:$REPO_TAG" "dsh-router:$REPO_TAG" "dsh-reaper:$REPO_TAG" "dsh:$IMAGE_TAG"; do
  case " $IMAGES " in
    *" ${ref%%:*} "*) ;;
    *) continue ;;
  esac
  got=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$PUB_PREFIX/$ref" 2>/dev/null || echo "?")
  if [ "$got" = "$PLATFORM" ]; then
    echo "  OK   $ref → $got"
  else
    echo "  FAIL $ref → ${got}（期望 ${PLATFORM}；集群是 amd64 时会 ImagePullBackOff/exec format error）"
    ARCH_FAIL=1
  fi
done
[ "$ARCH_FAIL" = 1 ] && { echo "架构不匹配，先别推" >&2; exit 1; }

say "5. 推送"
if [ -n "${SKIP_PUSH:-}" ]; then
  echo "  SKIP_PUSH=1，不推送"
else
  want dsh-control && docker push "$PUB_PREFIX/dsh-control:$REPO_TAG"
  want dsh-router  && docker push "$PUB_PREFIX/dsh-router:$REPO_TAG"
  want dsh-reaper  && docker push "$PUB_PREFIX/dsh-reaper:$REPO_TAG"
  want dsh         && docker push "$PUB_PREFIX/dsh:$IMAGE_TAG"
fi

say "6. 集群侧会引用（VPC 端点）"
cat <<EOF
  $VPC_PREFIX/dsh:$IMAGE_TAG
  $VPC_PREFIX/dsh-control:$REPO_TAG
  $VPC_PREFIX/dsh-router:$REPO_TAG
  $VPC_PREFIX/dsh-reaper:$REPO_TAG

  拉取凭据（在两个命名空间各建一份，--docker-server 只写域名）：
    for ns in dsh-system dsh-users; do
      kubectl -n \$ns create secret docker-registry ${PULL_SECRET:-acr-pull} \\
        --docker-server=${VPC_PREFIX%%/*} \\
        --docker-username=<阿里云账号> --docker-password=<ACR 独立密码>
    done
EOF
