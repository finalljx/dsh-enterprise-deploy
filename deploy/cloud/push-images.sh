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
DSH_VERSION="${DSH_VERSION:-0.2.1-alpha.4}"
REPO_TAG="${REPO_TAG:-0.1.0}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
MODEL_BASE_URL="${MODEL_BASE_URL:-https://api.deepseek.com/anthropic}"
NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmmirror.com}"

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

say "2. 构建"
docker build -f control/Dockerfile --build-arg GOPROXY="$GOPROXY" -t "$PUB_PREFIX/dsh-control:$REPO_TAG" ./control
docker build -f router/Dockerfile  -t "$PUB_PREFIX/dsh-router:$REPO_TAG"  ./router
docker build -f reaper/Dockerfile  -t "$PUB_PREFIX/dsh-reaper:$REPO_TAG"  ./reaper
# 用户镜像必须用 Dockerfile.dev：image/Dockerfile 依赖尚未发布的活动插件
docker build -f Dockerfile.dev --build-arg MODEL_BASE_URL="$MODEL_BASE_URL" \
  --build-arg NPM_REGISTRY="$NPM_REGISTRY" \
  -t "$PUB_PREFIX/dsh:$DSH_VERSION" .

say "3. 推送"
if [ -n "${SKIP_PUSH:-}" ]; then
  echo "  SKIP_PUSH=1，不推送"
else
  for ref in "dsh-control:$REPO_TAG" "dsh-router:$REPO_TAG" "dsh-reaper:$REPO_TAG" "dsh:$DSH_VERSION"; do
    docker push "$PUB_PREFIX/$ref"
  done
fi

say "4. 集群侧会引用（VPC 端点）"
cat <<EOF
  $VPC_PREFIX/dsh:$DSH_VERSION
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
