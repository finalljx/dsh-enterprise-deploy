#!/usr/bin/env bash
# 在 macOS + k3d 上拉起一套可验证的环境。
#
# 与生产的三处差异（见 dev/README.md）：
#   1. 不用 Kata —— Docker Desktop 的 Linux 里没有 /dev/kvm
#   2. 不用 Ceph —— 用 k3s 自带的 local-path
#   3. 不用 Istio —— 直接把 router 暴露到本机 80 端口
#
# 镜像默认走 k3d image import，不需要任何仓库。要验「从私有 registry 拉镜像」
# 这条生产路径时，再打开 registry 模式（见 dev/README.md 第八节）：
#   export DSH_REGISTRY=localhost:5000         # host 侧 push 地址
#   export CLUSTER_REGISTRY=k3d-dsh-reg:5000   # 集群内 pull 地址（docker 网络里的名字）
#   export K3D_REGISTRY=dsh-reg                # 可选：不存在时按此名建，端口取自 DSH_REGISTRY
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${CLUSTER:-dsh}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.31.5-k3s1}"
# docker.io 加速器。k3s 节点默认直连 Docker Hub，国内基本拉不动，而 pause /
# busybox / klipper-lb / cert-manager 这些基础镜像又必须能拉。
# 设为空串可以关掉（内网换成自己的代理仓库地址即可）。
DOCKER_MIRROR="${DOCKER_MIRROR:-https://docker.m.daocloud.io}"
NS_SYS=dsh-system
NS_USERS=dsh-users
ADMIN_TOKEN="$(openssl rand -hex 16)"
ROUTER_TOKEN="$(openssl rand -hex 16)"
REGISTER_TOKEN="$(openssl rand -hex 16)"

DSH_REGISTRY="${DSH_REGISTRY:-}"
CLUSTER_REGISTRY="${CLUSTER_REGISTRY:-}"
K3D_REGISTRY="${K3D_REGISTRY:-}"
REGISTRY_MODE=0
if [ -n "$DSH_REGISTRY" ]; then
  REGISTRY_MODE=1
  if [ -z "$CLUSTER_REGISTRY" ]; then
    echo "设置了 DSH_REGISTRY=$DSH_REGISTRY 就要同时给 CLUSTER_REGISTRY（pod 侧地址，如 k3d-dsh-reg:5000）"
    echo "registry 只按 repo path 存内容，host 侧和集群侧可以不同名，但 repo 必须一致。"
    exit 1
  fi
  # 带时间戳的 tag：镜像换了 tag 才会被重新拉取，省得跟 IfNotPresent 缓存打架
  IMAGE_TAG="${IMAGE_TAG:-dev-$(date +%H%M%S)}"
else
  IMAGE_TAG=local
fi

# 镜像全名：registry 模式给集群侧地址，否则给本地 tag
img() {
  if [ "$REGISTRY_MODE" = 1 ]; then echo "$CLUSTER_REGISTRY/$1:$IMAGE_TAG"; else echo "dsh/$1:$IMAGE_TAG"; fi
}

step() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

step "1/9 检查依赖"
# go 不需要装在本机：control / router 都在 golang:1.22-alpine 里多阶段编译
for c in docker k3d kubectl openssl curl; do
  command -v "$c" >/dev/null || { echo "缺少 $c"; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "Docker 未运行"; exit 1; }
[ "$REGISTRY_MODE" = 1 ] && echo "registry 模式：push 到 ${DSH_REGISTRY}，pod 从 ${CLUSTER_REGISTRY} 拉，tag=$IMAGE_TAG"

step "2/9 创建 k3d 集群（k3s ${K3S_IMAGE}）"
CLUSTER_EXISTS=0
k3d cluster list "$CLUSTER" >/dev/null 2>&1 && CLUSTER_EXISTS=1
if [ "$REGISTRY_MODE" = 1 ] && [ -n "$K3D_REGISTRY" ]; then
  if k3d registry list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "k3d-$K3D_REGISTRY"; then
    echo "registry k3d-$K3D_REGISTRY 已存在，跳过"
  else
    k3d registry create "$K3D_REGISTRY" --port "${DSH_REGISTRY##*:}"
  fi
fi
mkdir -p dev/render
if [ -n "$DOCKER_MIRROR" ]; then
  cat > dev/render/registries.yaml <<YAML
mirrors:
  docker.io:
    endpoint:
      - ${DOCKER_MIRROR}
YAML
fi

# 已有集群没法安全补加速器：k3s 只在启动时读 registries.yaml，而重启 k3d 节点会
# 换掉容器 IP，k3s 缓存的 kubelet 服务端证书仍指向旧 IP —— 之后 exec/logs/port-forward
# 全部报 x509 不匹配。所以这里只提示，不自动改。
warn_missing_mirror() {
  [ -n "$DOCKER_MIRROR" ] || return 0
  if docker exec "k3d-${CLUSTER}-server-0" sh -c "grep -q '${DOCKER_MIRROR}' /etc/rancher/k3s/registries.yaml" 2>/dev/null; then
    return 0
  fi
  cat <<MSG
WARN: 集群 $CLUSTER 没有 docker.io 加速器，pause / busybox / klipper-lb 这类基础镜像可能拉不动。
      老集群补不进去（重启节点会让 kubelet 证书对不上），只能重建：
        k3d cluster delete $CLUSTER && ./dev/up.sh
MSG
}

if [ "$CLUSTER_EXISTS" = 1 ]; then
  echo "集群 $CLUSTER 已存在，跳过创建"
  warn_missing_mirror
  if [ "$REGISTRY_MODE" = 1 ]; then
    echo "WARN: 已有集群不会自动挂上 registry。若它不是用 --registry-use 建的，pod 会拉不到镜像，"
    echo "      先 k3d cluster delete $CLUSTER 再重跑。"
  fi
else
  k3d_cluster_args=(
    --image "$K3S_IMAGE"
    --agents 1
    --port "80:8080@loadbalancer"
    --k3s-arg "--disable=traefik@server:0"
  )
  [ -n "$DOCKER_MIRROR" ] && k3d_cluster_args+=(--registry-config dev/render/registries.yaml)
  [ "$REGISTRY_MODE" = 1 ] && k3d_cluster_args+=(--registry-use "$CLUSTER_REGISTRY")
  k3d cluster create "$CLUSTER" "${k3d_cluster_args[@]}"
fi
# 本机 context 可能对着别的集群（例如 k3d-test-k8s）。kubectl 不会因为「集群名不对」
# 而报错，只会把清单 apply 到当前 context —— 于是 pod 落到错误的集群上拉不到本地镜像。
kubectl config use-context "k3d-$CLUSTER" >/dev/null 2>&1 || true
ctx="$(kubectl config current-context 2>/dev/null || true)"
if [ "$ctx" != "k3d-$CLUSTER" ]; then
  echo "当前 kubectl context 是 ${ctx:-<none>}，不是 k3d-${CLUSTER}，已中止：不往别的集群 apply。"
  echo "修复：k3d kubeconfig merge -a -d   或   KUBECONFIG=\$PWD/dev/render/kubeconfig.yaml ./dev/up.sh"
  exit 1
fi
echo "kubectl context: $ctx"
kubectl wait --for=condition=Ready node --all --timeout=180s

step "3/9 安装 cert-manager"
kubectl apply -f cert-manager.yaml
kubectl -n cert-manager wait --for=condition=Available deploy --all --timeout=300s

step "4/9 生成内部 CA 与 base 清单"
# 复用已有 CA：重跑一次就换一张 CA，会让所有已签发的用户证书失效。
if kubectl -n cert-manager get secret dsh-internal-ca-keypair >/dev/null 2>&1; then
  kubectl -n cert-manager get secret dsh-internal-ca-keypair \
    -o jsonpath='{.data.tls\.crt}' | openssl base64 -d -A > /tmp/dsh-ca.crt
  kubectl -n cert-manager get secret dsh-internal-ca-keypair \
    -o jsonpath='{.data.tls\.key}' | openssl base64 -d -A > /tmp/dsh-ca.key
  echo "复用集群里已有的 CA"
else
  openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
    -keyout /tmp/dsh-ca.key -out /tmp/dsh-ca.crt -subj "/CN=DSH Dev CA" 2>/dev/null
  kubectl -n cert-manager create secret tls dsh-internal-ca-keypair \
    --cert=/tmp/dsh-ca.crt --key=/tmp/dsh-ca.key --dry-run=client -o yaml | kubectl apply -f -
fi

kubectl apply -f 01-namespaces.yaml
# Secret 不能跨命名空间引用：ClusterIssuer 要 cert-manager 里那份，
# router 在 dsh-system 里要挂同一份 CA 去校验 pod 证书，所以放两份。
kubectl -n "$NS_SYS" create secret tls dsh-internal-ca-keypair \
  --cert=/tmp/dsh-ca.crt --key=/tmp/dsh-ca.key --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f 04-rbac.yaml
kubectl apply -f 05-networkpolicy.yaml
# 本地没有内网模型网关，用户 pod 必须能连公网 443 才能真的对话。
# NetworkPolicy 是并集语义，补一条更宽的出网规则即可，不用改生产清单。
# 只放行 443；pod 之间仍然只有 router 能进（上面两条策略的 ingress 约束不变）。
if [ "${DSH_ALLOW_PUBLIC_EGRESS:-1}" = "1" ]; then
  kubectl apply -f - <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: dsh-dev-public-egress, namespace: $NS_USERS }
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - ports:
        - { protocol: TCP, port: 443 }
YAML
fi
kubectl apply -f 06-admission-policy.yaml
kubectl apply -f 07-issuer.yaml
# 02 Kata / 03 Ceph 跳过；08 边缘证书、12 Istio 跳过

step "5/9 准备 Secret"
# 复用集群里已有的令牌：Secret 换了值、Deployment 模板却没变时，kubectl apply 不会重启
# pod，跑着的进程仍拿旧令牌 —— 表现为「重启后一切正常、否则一直是 403」。
# 所有值都齐才复用，缺一个就整套重生成。
read_secret_key() { kubectl -n "$1" get secret "$2" -o jsonpath="{.data.$3}" 2>/dev/null | openssl base64 -d -A 2>/dev/null || true; }
if kubectl -n "$NS_SYS" get secret dsh-control-auth >/dev/null 2>&1; then
  old_admin="$(read_secret_key "$NS_SYS" dsh-control-auth admin-token)"
  old_router="$(read_secret_key "$NS_SYS" dsh-control-auth router-token)"
  old_register="$(read_secret_key "$NS_SYS" dsh-control-auth register-token)"
  if [ -n "$old_admin" ] && [ -n "$old_router" ] && [ -n "$old_register" ]; then
    ADMIN_TOKEN="$old_admin"; ROUTER_TOKEN="$old_router"; REGISTER_TOKEN="$old_register"
    echo "复用已有的控制面令牌"
  fi
fi
COOKIE_KEY="$(read_secret_key "$NS_SYS" dsh-router-auth cookie-key)"
[ -n "$COOKIE_KEY" ] || COOKIE_KEY="$(openssl rand -hex 32)"

kubectl -n "$NS_SYS" create secret generic dsh-control-auth \
  --from-literal=admin-token="$ADMIN_TOKEN" \
  --from-literal=router-token="$ROUTER_TOKEN" \
  --from-literal=register-token="$REGISTER_TOKEN" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS_USERS" create secret generic dsh-control-auth \
  --from-literal=register-token="$REGISTER_TOKEN" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS_SYS" create secret generic dsh-router-auth \
  --from-literal=cookie-key="$COOKIE_KEY" --dry-run=client -o yaml | kubectl apply -f -
# 真实模型凭据：没有就走 dummy，端到端聊天会失败但结构仍可验
MODEL_KEY="${DEEPSEEK_API_KEY:-}"
if [ -z "$MODEL_KEY" ]; then
  if [ "${REQUIRE_MODEL_KEY:-0}" = "1" ]; then
    echo "缺少 DEEPSEEK_API_KEY（REQUIRE_MODEL_KEY=1）"; exit 1
  fi
  echo "WARN: 未设置 DEEPSEEK_API_KEY，端到端聊天会失败（结构验证不受影响）"
  MODEL_KEY=dummy
fi
mkdir -p dev/render
cat > dev/render/tokens.env <<ENV
export ADMIN_TOKEN=$ADMIN_TOKEN
export ROUTER_TOKEN=$ROUTER_TOKEN
export REGISTER_TOKEN=$REGISTER_TOKEN
ENV

step "6/9 构建镜像"
CONTROL_IMG="$(img dsh-control)"
ROUTER_IMG="$(img dsh-router)"
REAPER_IMG="$(img dsh-reaper)"
DSH_IMG="$(img dsh)"
# 默认走 goproxy.cn：proxy.golang.org 在多数国内网络里不可达
docker build -f control/Dockerfile \
  --build-arg GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
  -t "$CONTROL_IMG" ./control
docker build -f router/Dockerfile  -t "$ROUTER_IMG"  ./router
docker build -f reaper/Dockerfile  -t "$REAPER_IMG"  ./reaper
docker build -f image/Dockerfile \
  --build-arg MODEL_BASE_URL="${MODEL_BASE_URL:-https://api.deepseek.com/anthropic}" \
  -t "$DSH_IMG" .
if [ "$REGISTRY_MODE" = 1 ]; then
  for i in "$CONTROL_IMG" "$ROUTER_IMG" "$REAPER_IMG" "$DSH_IMG"; do
    docker push "$i"
  done
else
  k3d image import -c "$CLUSTER" "$CONTROL_IMG" "$ROUTER_IMG" "$REAPER_IMG" "$DSH_IMG"
fi

step "7/9 渲染并应用运行时清单"
mkdir -p dev/render
sed -e 's#storageClassName: dsh-rbd#storageClassName: local-path#' 09-postgres.yaml > dev/render/09-postgres.yaml
sed -e "s#registry.corp/dsh-control:0.1.0#$CONTROL_IMG#" 10-control.yaml > dev/render/10-control.yaml
sed -e "s#registry.corp/dsh-router:0.1.0#$ROUTER_IMG#" 11-router.yaml > dev/render/11-router.yaml
sed -e "s#registry.corp/dsh-reaper:0.1.0#$REAPER_IMG#" 13-reaper.yaml > dev/render/13-reaper.yaml

kubectl apply -f dev/render/09-postgres.yaml
kubectl -n "$NS_SYS" rollout status statefulset/dsh-postgres --timeout=180s

DSN="postgres://dsh:CHANGE_ME_STRONG_PASSWORD@dsh-postgres.$NS_SYS.svc.cluster.local:5432/dsh?sslmode=disable"
kubectl -n "$NS_SYS" create secret generic dsh-postgres-dsn \
  --from-literal=dsn="$DSN" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS_SYS" exec -i dsh-postgres-0 -- psql -U dsh -d dsh < control/schema.sql

kubectl apply -f dev/render/10-control.yaml
# 开通用户时控制面用这些值创建资源（生产里写死在清单里）
kubectl -n "$NS_SYS" set env deploy/dsh-control \
  DSH_IMAGE="$DSH_IMG" \
  DSH_STORAGE_CLASS=local-path \
  DSH_RUNTIME_CLASS=none \
  DSH_MODEL_ENV_NAME=DEEPSEEK_API_KEY
kubectl -n "$NS_SYS" rollout status deploy/dsh-control --timeout=180s

kubectl apply -f dev/render/11-router.yaml
# dev：router 关闭 cookie Secure（本地没有 TLS），并把 Service 暴露成 LB
kubectl -n "$NS_SYS" set env deploy/dsh-router \
  COOKIE_SECURE=false PUBLIC_SCHEME=http DEV_SUBJECT=alice@corp SSO_URL=/_dev/sso \
  ADMIN_SUBJECTS=alice@corp DSH_BASE_DOMAIN=dsh.corp
kubectl -n "$NS_SYS" patch svc dsh-router -p '{"spec":{"type":"LoadBalancer"}}'
kubectl -n "$NS_SYS" rollout status deploy/dsh-router --timeout=180s

kubectl apply -f dev/render/13-reaper.yaml

step "8/9 开通并唤醒 alice"
kubectl -n "$NS_SYS" port-forward svc/dsh-control 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
for _ in $(seq 1 20); do
  curl -sf http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break
  sleep 0.5
done
ADMIN_TOKEN="$ADMIN_TOKEN" python3 provision.py alice@corp \
  --tier T1 \
  --model-token "$MODEL_KEY" \
  --control-url http://127.0.0.1:18080
kill "$PF_PID" 2>/dev/null || true

echo "等待回收器拉起 pod（最长 60s）..."
for _ in $(seq 1 60); do
  ready=$(kubectl -n "$NS_USERS" get deploy dsh-alice -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
  [ "$ready" = "1" ] && break
  sleep 1
done
kubectl -n "$NS_USERS" get pods

step "9/9 完成"
cat <<TIP

把子域指向本机（需要 sudo）：
  echo '127.0.0.1 alice.dsh.corp bob.dsh.corp' | sudo tee -a /etc/hosts

验收：
  SCHEME=http ./verify.sh

注意：k3d 默认 flannel 不强制 NetworkPolicy，verify.sh 第 3 条会显示通过但并未真实验证。
令牌（用于手工调接口）：
  source dev/render/tokens.env
  kubectl -n dsh-system port-forward svc/dsh-control 8080:8080 &
  curl -s -H "Authorization: Bearer \$ROUTER_TOKEN" \
    'localhost:8080/internal/lookup?subject=alice@corp' | jq
清理：k3d cluster delete $CLUSTER
TIP
