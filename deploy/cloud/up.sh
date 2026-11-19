#!/usr/bin/env bash
# 在「已有 K8s 集群 + 已装 Kata」的云主机上部署 DSH，用于全链路验证。
#
# 不做的事（有意留白，见 deploy/cloud/README.md）：
#   - 不装 Istio / 不配真实域名证书：验证阶段用 ServiceLB + HTTP，TLS 层留到正式环境
#   - 不建 StorageClass：用现有的 local-path 或你给的 SC
#   - 不改域名文本：清单里的 dsh.corp 同时是标签键（dsh.corp/user），sed 会改坏标签，
#     所以域名只通过 DSH_BASE_DOMAIN 环境变量注入
#
# 用法（最少两个变量）：
#   DSH_REGISTRY=registry.cn-hangzhou.aliyuncs.com/myns \
#   DSH_DOMAIN=dsh.example.com \
#   ./deploy/cloud/up.sh
#
# 可选：
#   DSH_VERSION=0.2.1-alpha.4     镜像 tag
#   DSH_STORAGE_CLASS=local-path  存储类
#   DSH_RUNTIME_CLASS=kata-clh-runtime-rs
#   PULL_SECRET=acr-pull          私仓拉取密钥（需你事先在两个命名空间里创建）
#   DISABLE_KATA=1                强制 runc 模式（默认会自动识别：没有 RuntimeClass 就降级）
#   ADMIN_SUBJECT=alice@corp      管理员身份
#   SKIP_PROVISION=1              不自动开通示例用户
set -euo pipefail
cd "$(dirname "$0")/../.."

# 可选：把参数写在 deploy/cloud/env 里（已 gitignore），省得每次 export。
# 注意：**配置文件优先**，同名环境变量会被文件里的值覆盖。
# 想临时换一组参数，用 ENV_FILE 指向另一个文件（ENV_FILE=/dev/null 表示不读）。
ENV_FILE="${ENV_FILE:-deploy/cloud/env}"
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ENV_FILE"
  set +a
fi

DSH_REGISTRY="${DSH_REGISTRY:?需要 DSH_REGISTRY，例如 registry.cn-hangzhou.aliyuncs.com/myns}"
DSH_DOMAIN="${DSH_DOMAIN:?需要 DSH_DOMAIN，例如 dsh.example.com}"
DSH_VERSION="${DSH_VERSION:-0.2.1-alpha.4}"
# control/router/reaper 的 tag 与 Makefile 保持一致（默认 0.1.0）；云上若用一套版本可改成同一个
REPO_TAG="${REPO_TAG:-0.1.0}"
DSH_STORAGE_CLASS="${DSH_STORAGE_CLASS:-local-path}"
DSH_RUNTIME_CLASS="${DSH_RUNTIME_CLASS:-kata-clh-runtime-rs}"
DISABLE_KATA="${DISABLE_KATA:-0}"
PULL_SECRET="${PULL_SECRET:-}"
ADMIN_SUBJECT="${ADMIN_SUBJECT:-alice@corp}"
# 与 control/provision.go 的 slugFor 保持一致：取 @ 前、小写、非 [a-z0-9-] 变 -
slug_of() {
  printf '%s' "${1%%@*}" | tr '[:upper:]' '[:lower:]' | sed -e 's/[^a-z0-9-]/-/g' -e 's/^-*//' -e 's/-*$//'
}
ADMIN_SLUG="$(slug_of "$ADMIN_SUBJECT")"
PF_PORT="${PF_PORT:-0}"   # 0 = 让内核挑一个空闲端口
# 真实场景验证要带 Istio 入口（TLS 终止 + 通配证书 + *.domain 路由）
WITH_ISTIO="${WITH_ISTIO:-0}"
# 验证阶段没有 IdP 时保留 dev 登录；接真实 IdP 时设 0，并给 SSO_URL
KEEP_DEV_LOGIN="${KEEP_DEV_LOGIN:-1}"
# ALB 在本项目外部终止 TLS：集群内用 HTTP + NodePort，不需要 Istio
ALB_MODE="${ALB_MODE:-0}"
# ALB 后端端口：固定下来，避免集群重建后自动分配的端口漂移
NODE_PORT="${NODE_PORT:-31147}"
SSO_URL="${SSO_URL:-}"
# 边缘证书的 issuer：默认用仓库自带的内部 CA（浏览器不信任，只适合 curl -k）。
# 要浏览器绿锁，换成你的公网 issuer（如 Let's Encrypt + DNS-01 webhook）。
EDGE_ISSUER="${EDGE_ISSUER:-dsh-internal-ca}"
NS_SYS=dsh-system
NS_USERS=dsh-users
RENDER=deploy/cloud/render

# 是否真的用 Kata：在 step_check 里按集群状态判定（普通 ECS 没有 RuntimeClass 就自动降级）
KATA_MODE=0
POD_CIDR="${POD_CIDR:-10.42.0.0/16}"   # k3s 默认 pod 网段，其它发行版按实际改
SVC_CIDR="${SVC_CIDR:-10.43.0.0/16}"   # k3s 默认 service 网段

say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

if [ "${PRINT_CONFIG:-0}" = "1" ]; then
  cat <<EOF
DSH_REGISTRY      = $DSH_REGISTRY
DSH_VERSION       = $DSH_VERSION
REPO_TAG          = $REPO_TAG
DSH_DOMAIN        = $DSH_DOMAIN
DSH_STORAGE_CLASS = $DSH_STORAGE_CLASS
DSH_RUNTIME_CLASS = $DSH_RUNTIME_CLASS
PULL_SECRET       = ${PULL_SECRET:-<未设置>}
WITH_ISTIO        = $WITH_ISTIO
EDGE_ISSUER       = $EDGE_ISSUER
KEEP_DEV_LOGIN    = $KEEP_DEV_LOGIN
ADMIN_SUBJECT     = $ADMIN_SUBJECT
镜像将渲染为：
  $DSH_REGISTRY/dsh:$DSH_VERSION
  $DSH_REGISTRY/dsh-control:$REPO_TAG
  $DSH_REGISTRY/dsh-router:$REPO_TAG
  $DSH_REGISTRY/dsh-reaper:$REPO_TAG
EOF
  exit 0
fi

step_check() {
  say "0/9 前置检查"
  command -v kubectl >/dev/null || { echo "缺少 kubectl" >&2; exit 1; }
  kubectl cluster-info >/dev/null 2>&1 || { echo "kubectl 连不上集群" >&2; exit 1; }
  # /version 是格式化 JSON（字段分行、冒号后有空格），别用单行正则
  local raw major minor
  raw=$(kubectl get --raw /version 2>/dev/null || true)
  major=$(printf '%s' "$raw" | sed -n 's/.*"major": *"\([0-9]*\)".*/\1/p' | head -1)
  minor=$(printf '%s' "$raw" | sed -n 's/.*"minor": *"\([0-9]*\).*/\1/p' | head -1)
  echo "  K8s 版本: ${major:-?}.${minor:-?}"
  if [ "$ALB_MODE" = "1" ] && [ "$WITH_ISTIO" = "1" ]; then
    echo "  ALB_MODE 与 WITH_ISTIO 互斥：TLS 只能在一个地方终止" >&2
    exit 1
  fi
  if [ "${major:-0}" -lt 1 ] || { [ "${major:-0}" -eq 1 ] && [ "${minor:-0}" -lt 30 ]; }; then
    echo "  K8s < 1.30：ValidatingAdmissionPolicy 不是 GA，准入策略段会失败" >&2
    exit 1
  fi
  # 模式判定：集群里有 Kata RuntimeClass 才用 Kata，否则自动降级为 runc。
  # 普通 ECS 没有 /dev/kvm，装不了 Kata —— 这是正常路径，不是错误。
  local want_rc="$DSH_RUNTIME_CLASS"   # 覆盖前先记下用户期望的名字
  if [ "${DISABLE_KATA:-0}" = "1" ]; then
    KATA_MODE=0; DSH_RUNTIME_CLASS=none
    echo "  模式: runc（DISABLE_KATA=1 强制）"
  elif kubectl get runtimeclass "$want_rc" >/dev/null 2>&1; then
    KATA_MODE=1
    echo "  模式: kata（RuntimeClass ${want_rc}）"
  else
    KATA_MODE=0; DSH_RUNTIME_CLASS=none
    echo "  模式: runc（集群里没有 RuntimeClass ${want_rc}，自动降级）"
    echo "        微虚拟机隔离验不了，其余全部可用；要验 Kata 先跑 install-kata.sh"
  fi
}

step_certs() {
  say "1/9 cert-manager 与内部 CA"
  kubectl apply -f cert-manager.yaml
  kubectl -n cert-manager wait --for=condition=Available deploy --all --timeout=300s

  # 复用已有 CA：每次重跑换 CA 会让已签发的用户证书全部失效
  if kubectl -n cert-manager get secret dsh-internal-ca-keypair >/dev/null 2>&1; then
    kubectl -n cert-manager get secret dsh-internal-ca-keypair -o jsonpath='{.data.tls\.crt}' | openssl base64 -d -A > /tmp/dsh-ca.crt
    kubectl -n cert-manager get secret dsh-internal-ca-keypair -o jsonpath='{.data.tls\.key}' | openssl base64 -d -A > /tmp/dsh-ca.key
    echo "  复用已有 CA"
  else
    openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
      -keyout /tmp/dsh-ca.key -out /tmp/dsh-ca.crt -subj "/CN=DSH Cloud CA" 2>/dev/null
    kubectl -n cert-manager create secret tls dsh-internal-ca-keypair \
      --cert=/tmp/dsh-ca.crt --key=/tmp/dsh-ca.key --dry-run=client -o yaml | kubectl apply -f -
    echo "  已生成新的内部 CA"
  fi
}

step_base() {
  say "2/9 命名空间、RBAC、网络、准入、Issuer"
  kubectl apply -f 01-namespaces.yaml
  # router 在 dsh-system 里也要挂这份 CA（Secret 不能跨命名空间引用）
  kubectl -n "$NS_SYS" create secret tls dsh-internal-ca-keypair \
    --cert=/tmp/dsh-ca.crt --key=/tmp/dsh-ca.key --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f 04-rbac.yaml
  kubectl apply -f 05-networkpolicy.yaml
  kubectl apply -f 06-admission-policy.yaml
  kubectl apply -f 07-issuer.yaml

  if [ "$KATA_MODE" = "1" ]; then
    # 清单里的 RuntimeClass 带 overhead 与 nodeSelector，覆盖 kata-deploy 自建的那份
    handler=$(kubectl get runtimeclass "$DSH_RUNTIME_CLASS" -o jsonpath='{.handler}')
    if [ "$handler" = "kata-clh-runtime-rs" ]; then
      kubectl apply -f 02-runtimeclass.yaml
      echo "  已应用 02-runtimeclass.yaml（handler 匹配）"
    else
      echo "  跳过 02-runtimeclass.yaml：实际 handler=${handler}，与清单写的 kata-clh-runtime-rs 不一致"
    fi
  fi

  # 出网：验证阶段要连公网模型网关。只放行到公网 443，
  # except 掉 pod/service 网段，否则用户容器能直接摸到 K8s API server。
  kubectl apply -f - <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: dsh-dev-public-egress, namespace: $NS_USERS }
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - $POD_CIDR
              - $SVC_CIDR
      ports:
        - { protocol: TCP, port: 443 }
YAML
}

step_pull_secret() {
  say "3/9 私仓拉取凭据"
  if [ -z "$PULL_SECRET" ]; then
    echo "  未设置 PULL_SECRET：镜像必须公开可拉，否则用户 pod 会 ImagePullBackOff"
    return
  fi
  for ns in "$NS_SYS" "$NS_USERS"; do
    if ! kubectl -n "$ns" get secret "$PULL_SECRET" >/dev/null 2>&1; then
      echo "  $ns 里没有 ${PULL_SECRET}，先创建：" >&2
      echo "    kubectl -n $ns create secret docker-registry $PULL_SECRET \\" >&2
      echo "      --docker-server=${DSH_REGISTRY%%/*} --docker-username=... --docker-password=..." >&2
      exit 1
    fi
  done
  # 用户 pod 用的是 dsh-workload 这个 SA（见 control/provision.go）
  for sa in dsh-control dsh-router dsh-reaper; do
    kubectl -n "$NS_SYS" patch serviceaccount "$sa" -p "{\"imagePullSecrets\":[{\"name\":\"$PULL_SECRET\"}]}" >/dev/null
  done
  kubectl -n "$NS_USERS" patch serviceaccount dsh-workload -p "{\"imagePullSecrets\":[{\"name\":\"$PULL_SECRET\"}]}" >/dev/null
  echo "  已把 $PULL_SECRET 挂到 4 个 ServiceAccount"
}

step_secrets() {
  say "4/9 令牌与模型凭据"
  if kubectl -n "$NS_SYS" get secret dsh-control-auth >/dev/null 2>&1; then
    old_admin=$(kubectl -n "$NS_SYS" get secret dsh-control-auth -o jsonpath='{.data.admin-token}' | openssl base64 -d -A)
    old_router=$(kubectl -n "$NS_SYS" get secret dsh-control-auth -o jsonpath='{.data.router-token}' | openssl base64 -d -A)
    old_reg=$(kubectl -n "$NS_SYS" get secret dsh-control-auth -o jsonpath='{.data.register-token}' | openssl base64 -d -A)
    if [ -n "$old_admin" ] && [ -n "$old_router" ] && [ -n "$old_reg" ]; then
      ADMIN_TOKEN="$old_admin"; ROUTER_TOKEN="$old_router"; REGISTER_TOKEN="$old_reg"
      echo "  复用已有令牌"
    fi
  fi
  ADMIN_TOKEN="${ADMIN_TOKEN:-$(openssl rand -hex 16)}"
  ROUTER_TOKEN="${ROUTER_TOKEN:-$(openssl rand -hex 16)}"
  REGISTER_TOKEN="${REGISTER_TOKEN:-$(openssl rand -hex 16)}"
  COOKIE_KEY=$(kubectl -n "$NS_SYS" get secret dsh-router-auth -o jsonpath='{.data.cookie-key}' 2>/dev/null | openssl base64 -d -A || true)
  [ -n "$COOKIE_KEY" ] || COOKIE_KEY="$(openssl rand -hex 32)"

  kubectl -n "$NS_SYS" create secret generic dsh-control-auth \
    --from-literal=admin-token="$ADMIN_TOKEN" --from-literal=router-token="$ROUTER_TOKEN" \
    --from-literal=register-token="$REGISTER_TOKEN" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NS_USERS" create secret generic dsh-control-auth \
    --from-literal=register-token="$REGISTER_TOKEN" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NS_SYS" create secret generic dsh-router-auth \
    --from-literal=cookie-key="$COOKIE_KEY" --dry-run=client -o yaml | kubectl apply -f -

  mkdir -p "$RENDER"
  printf 'export ADMIN_TOKEN=%s\nexport ROUTER_TOKEN=%s\nexport REGISTER_TOKEN=%s\n' \
    "$ADMIN_TOKEN" "$ROUTER_TOKEN" "$REGISTER_TOKEN" > "$RENDER/tokens.env"

  MODEL_KEY="${DEEPSEEK_API_KEY:-dummy}"
  [ "$MODEL_KEY" = "dummy" ] && echo "  WARN: 没有 DEEPSEEK_API_KEY，端到端对话会失败（结构验证不受影响）"
  # 名字必须跟开通时的 slug 一致（provision.go 用的是 dsh-model-token-<slug>），
  # 否则 control 会以为「缺模型密钥」而拒绝开通。
  if [ "${MODEL_KEY}" = "dummy" ] && kubectl -n "${NS_USERS}" get secret "dsh-model-token-${ADMIN_SLUG}" >/dev/null 2>&1; then
    echo "  已是 dummy 且密钥已存在，保留原值（不覆盖真实 token）"
  else
    kubectl -n "${NS_USERS}" create secret generic "dsh-model-token-${ADMIN_SLUG}" \
      --from-literal=token="${MODEL_KEY}" --dry-run=client -o yaml | kubectl apply -f -
  fi
}

step_render() {
  say "5/9 渲染清单（registry / storageClass / 镜像 tag）"
  mkdir -p "$RENDER"
  # 只替换镜像前缀与存储类；域名走环境变量，避免动到 dsh.corp/user 这类标签键。
  # 左边是清单里的固定写法（源 tag 写死 0.1.0 / 0.2.1-alpha.4），右边才是目标值。
  local subs=(
    "s#registry.corp/dsh-control:0.1.0#$DSH_REGISTRY/dsh-control:$REPO_TAG#g"
    "s#registry.corp/dsh-router:0.1.0#$DSH_REGISTRY/dsh-router:$REPO_TAG#g"
    "s#registry.corp/dsh-reaper:0.1.0#$DSH_REGISTRY/dsh-reaper:$REPO_TAG#g"
    "s#registry.corp/dsh:0.2.1-alpha.4#$DSH_REGISTRY/dsh:$DSH_VERSION#g"
    "s#storageClassName: dsh-rbd#storageClassName: $DSH_STORAGE_CLASS#g"
    "s#nodePort: 31147#nodePort: $NODE_PORT#g"
  )
  local args=() e f
  for e in "${subs[@]}"; do args+=(-e "$e"); done
  for f in 09-postgres 10-control 11-router 13-reaper; do
    sed "${args[@]}" "$f.yaml" > "$RENDER/$f.yaml"
  done
  echo "  渲染到 $RENDER/"
}

step_database() {
  say "6/9 Postgres 与 schema"
  kubectl apply -f "$RENDER/09-postgres.yaml"
  kubectl -n "$NS_SYS" rollout status statefulset/dsh-postgres --timeout=300s
  DSN="postgres://dsh:CHANGE_ME_STRONG_PASSWORD@dsh-postgres.$NS_SYS.svc.cluster.local:5432/dsh?sslmode=disable"
  kubectl -n "$NS_SYS" create secret generic dsh-postgres-dsn \
    --from-literal=dsn="$DSN" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NS_SYS" exec -i dsh-postgres-0 -- psql -U dsh -d dsh < control/schema.sql
}

step_apps() {
  say "7/9 control / router / reaper"
  kubectl apply -f "$RENDER/10-control.yaml"
  kubectl -n "$NS_SYS" set env deploy/dsh-control \
    DSH_IMAGE="$DSH_REGISTRY/dsh:$DSH_VERSION" \
    DSH_STORAGE_CLASS="$DSH_STORAGE_CLASS" \
    DSH_RUNTIME_CLASS="$DSH_RUNTIME_CLASS" \
    DSH_BASE_DOMAIN="$DSH_DOMAIN" \
    DSH_MODEL_ENV_NAME=DEEPSEEK_API_KEY >/dev/null
  kubectl -n "$NS_SYS" rollout status deploy/dsh-control --timeout=300s

  kubectl apply -f "$RENDER/11-router.yaml"
  # 验证阶段没有 TLS 终止层：router 用 HTTP，管理员用 DEV_SUBJECT 直接登录
  kubectl -n "$NS_SYS" set env deploy/dsh-router \
    COOKIE_SECURE=false PUBLIC_SCHEME=http DEV_SUBJECT="$ADMIN_SUBJECT" SSO_URL=/_dev/sso \
    ADMIN_SUBJECTS="$ADMIN_SUBJECT" DSH_BASE_DOMAIN="$DSH_DOMAIN" >/dev/null
  # 入口类型由模式决定：ALB_MODE → NodePort，WITH_ISTIO → ClusterIP，否则本机 LoadBalancer
  if [ "$ALB_MODE" != "1" ] && [ "$WITH_ISTIO" != "1" ]; then
    kubectl -n "$NS_SYS" patch svc dsh-router -p '{"spec":{"type":"LoadBalancer"}}' >/dev/null
  fi
  kubectl -n "$NS_SYS" rollout status deploy/dsh-router --timeout=300s

  kubectl apply -f "$RENDER/13-reaper.yaml"
  kubectl -n "$NS_SYS" rollout status deploy/dsh-reaper --timeout=180s
}

step_istio() {
  say "7.5/9 Istio 入口（TLS + 通配证书）"
  if [ "$WITH_ISTIO" != "1" ]; then
    echo "  跳过（WITH_ISTIO!=1；验证阶段用 ServiceLB + HTTP）"
    return 0
  fi
  if ! kubectl get crd virtualservices.networking.istio.io >/dev/null 2>&1; then
    echo "  集群里没有 Istio CRD，先安装：" >&2
    echo "    istioctl install --set meshConfig.defaultConfig.gatewayTopology.numTrustedProxies=1" >&2
    echo "  注意 numTrustedProxies 必须设，否则 Istio 改写 Host，router 的 Host 校验会 403" >&2
    exit 1
  fi

  kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # 只替换 "*.dsh.corp" 这个通配值；dsh.corp/user 这类标签键不能被动
  sed -e "s#\*\.dsh\.corp#*.$DSH_DOMAIN#g" -e "s#name: dsh-internal-ca#name: $EDGE_ISSUER#g" \
    08-edge-cert.yaml > "$RENDER/08-edge-cert.yaml"
  sed "s#\*\.dsh\.corp#*.$DSH_DOMAIN#g" 12-istio.yaml     > "$RENDER/12-istio.yaml"

  kubectl apply -f "$RENDER/08-edge-cert.yaml"
  if ! kubectl -n istio-system wait --for=condition=Ready certificate/dsh-wildcard --timeout=300s >/dev/null 2>&1; then
    echo "  通配证书没签发出来，看：" >&2
    echo "    kubectl -n istio-system describe certificate dsh-wildcard" >&2
    echo "  公网域名要能通过 issuer 校验：内部 CA 签的证书浏览器不会信任" >&2
    exit 1
  fi
  echo "  通配证书 $DSH_DOMAIN 已签发（issuer: ${EDGE_ISSUER}）"
  if [ "$EDGE_ISSUER" = "dsh-internal-ca" ]; then
    echo "  WARN: 内部 CA 签的证书浏览器不信任；要绿锁请用 EDGE_ISSUER=<公网 issuer> 重跑，或手工导入证书"
  fi

  kubectl apply -f "$RENDER/12-istio.yaml"

  # 入口改成 https：cookie 必须 Secure；router 降回 ClusterIP 由网关转发
  local set_args=(COOKIE_SECURE=true PUBLIC_SCHEME=https DSH_BASE_DOMAIN="$DSH_DOMAIN")
  if [ "$KEEP_DEV_LOGIN" = "1" ]; then
    set_args+=(DEV_SUBJECT="$ADMIN_SUBJECT" SSO_URL=/_dev/sso)
    echo "  保留 dev 登录（KEEP_DEV_LOGIN=1）：仍在用假 IdP，不属于生产配置"
  else
    [ -n "$SSO_URL" ] || { echo "KEEP_DEV_LOGIN=0 时必须给 SSO_URL" >&2; exit 1; }
    set_args+=(SSO_URL="$SSO_URL")
    kubectl -n "$NS_SYS" set env deploy/dsh-router DEV_SUBJECT- >/dev/null 2>&1 || true
  fi
  kubectl -n "$NS_SYS" set env deploy/dsh-router "${set_args[@]}" >/dev/null
  kubectl -n "$NS_SYS" patch svc dsh-router -p '{"spec":{"type":"ClusterIP"}}' >/dev/null
  kubectl -n "$NS_SYS" rollout status deploy/dsh-router --timeout=180s
}

step_alb() {
  say "7.6/9 ALB 接入（TLS 在 ALB 终止）"
  if [ "$ALB_MODE" != "1" ]; then
    echo "  跳过（ALB_MODE!=1）"
    return 0
  fi

  # NodePort + Local：kube-proxy 只转本节点的 router pod，ALB 探测语义才准确，
  # 且节点上没有 router pod 时该后端直接不通，由 ALB 摘除。
  kubectl -n "$NS_SYS" patch svc dsh-router --type=merge \
    -p '{"spec":{"type":"NodePort","externalTrafficPolicy":"Local"}}' >/dev/null

  # ALB 终止 TLS，浏览器看到的是 https；这一跳是 http 不影响这两个值
  local set_args=(COOKIE_SECURE=true PUBLIC_SCHEME=https DSH_BASE_DOMAIN="$DSH_DOMAIN")
  if [ "$KEEP_DEV_LOGIN" = "1" ]; then
    set_args+=(DEV_SUBJECT="$ADMIN_SUBJECT" SSO_URL=/_dev/sso)
    echo "  保留 dev 登录（KEEP_DEV_LOGIN=1）：仍在用假 IdP"
  else
    [ -n "$SSO_URL" ] || { echo "KEEP_DEV_LOGIN=0 时必须给 SSO_URL" >&2; exit 1; }
    set_args+=(SSO_URL="$SSO_URL")
    kubectl -n "$NS_SYS" set env deploy/dsh-router DEV_SUBJECT- >/dev/null 2>&1 || true
  fi
  kubectl -n "$NS_SYS" set env deploy/dsh-router "${set_args[@]}" >/dev/null
  kubectl -n "$NS_SYS" rollout status deploy/dsh-router --timeout=180s

  local np
  np=$(kubectl -n "$NS_SYS" get svc dsh-router -o jsonpath='{.spec.ports[0].nodePort}')
  cat <<EOF

  ALB 七层后端配置：
    协议 HTTP   端口 $np   后端：每台 ECS 内网 IP
    健康检查 GET /healthz（期望 200）  间隔 10s  超时 5s  不健康阈值 3  健康阈值 2
    Host      必须原样透传，不要改写（否则 router 的 Host 校验 403）
    安全组    只放行 ALB 后端地址段到 ${np}，不要对 0.0.0.0/0 开
    会话保持  不需要（会话在 cookie 里）
EOF
}

step_provision() {
  say "8/9 开通 ${ADMIN_SUBJECT}"
  if [ "${SKIP_PROVISION:-0}" = "1" ]; then
    echo "  SKIP_PROVISION=1，跳过"
    return 0
  fi

  # 端口别写死：18080 被占时 port-forward 会静默退出，最后只看到 connection refused，很难查。
  if [ "${PF_PORT}" = "0" ]; then
    PF_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
  fi

  local pflog pf ok_url rc
  pflog=$(mktemp -t dshpf)
  kubectl -n "${NS_SYS}" port-forward svc/dsh-control "${PF_PORT}:8080" >"${pflog}" 2>&1 &
  pf=$!
  ok_url=0
  for _ in $(seq 1 40); do
    kill -0 "${pf}" 2>/dev/null || break
    if curl -sf -m 1 "http://127.0.0.1:${PF_PORT}/healthz" >/dev/null 2>&1; then ok_url=1; break; fi
    sleep 0.5
  done
  if [ "${ok_url}" != "1" ]; then
    echo "  port-forward 没起来（端口 ${PF_PORT}）：" >&2
    tail -5 "${pflog}" >&2 || true
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
    rm -f "${pflog}"
    exit 1
  fi

  # 只有拿到真实 key 才传 --model-token；否则不传，让 control 复用已有 Secret，
  # 避免把线上真实的 token 覆盖成 dummy。
  local token_args=()
  case "${MODEL_KEY:-}" in
    ""|dummy) : ;;
    *) token_args=(--model-token "${MODEL_KEY}") ;;
  esac

  rc=0
  ADMIN_TOKEN="${ADMIN_TOKEN}" python3 provision.py "${ADMIN_SUBJECT}" \
    --tier T1 ${token_args[@]+"${token_args[@]}"} \
    --control-url "http://127.0.0.1:${PF_PORT}" || rc=$?

  # 已存在的用户可能处于 sleeping：UpsertUser 刻意不覆盖 desired_state，
  # 所以重新开通不会自动醒。开通语义是「可用」，这里显式唤醒。
  if [ "${rc}" -eq 0 ]; then
    cur=$(curl -sf -m 3 -H "Authorization: Bearer ${ROUTER_TOKEN}" \
      "http://127.0.0.1:${PF_PORT}/internal/lookup?subject=${ADMIN_SUBJECT}" 2>/dev/null \
      | sed -n 's/.*"desiredState":"\([^"]*\)".*/\1/p')
    if [ -n "${cur}" ] && [ "${cur}" != "running" ]; then
      if curl -sf -m 3 -X POST -H "Authorization: Bearer ${ADMIN_TOKEN}" \
           -H 'content-type: application/json' \
           -d "{\"subject\":\"${ADMIN_SUBJECT}\",\"desiredState\":\"running\"}" \
           "http://127.0.0.1:${PF_PORT}/internal/desired" >/dev/null 2>&1; then
        echo "  期望状态 ${cur} → running（已唤醒）"
      else
        echo "  WARN: 唤醒失败，该用户可能仍处于 ${cur}"
      fi
    fi
  fi

  kill "${pf}" 2>/dev/null || true
  wait "${pf}" 2>/dev/null || true
  rm -f "${pflog}"
  if [ "${rc}" -ne 0 ]; then
    echo "  开通失败（provision.py 退出码 ${rc}）" >&2
    exit "${rc}"
  fi

  # 打印该用户实际的访问地址：协议取决于入口模式
  local scheme=http host
  if [ "${ALB_MODE}" = "1" ] || [ "${WITH_ISTIO}" = "1" ]; then scheme=https; fi
  host="${ADMIN_SLUG}.${DSH_DOMAIN}"
  echo "  访问地址: ${scheme}://${host}"
  echo "  等待 pod 就绪（最长 120s）..."
  local i ready
  for i in $(seq 1 120); do
    ready=$(kubectl -n "${NS_USERS}" get deploy "dsh-${ADMIN_SLUG}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    if [ "${ready}" = "1" ]; then
      echo "  ✅ dsh-${ADMIN_SLUG} 已就绪："
      kubectl -n "${NS_USERS}" get pod -o wide --no-headers 2>/dev/null | grep "dsh-${ADMIN_SLUG}-" | head -3
      return 0
    fi
    sleep 1
  done
  echo "  WARN: 120s 内未就绪，看 kubectl -n ${NS_USERS} get pod / describe"
  return 0
}

step_done() {
  say "9/9 完成"
  local verify_cmd="./deploy/cloud/verify-isolation.sh"
  [ "$KATA_MODE" = "1" ] && verify_cmd="./deploy/cloud/verify-kata.sh"
  local ip src
  if [ "$ALB_MODE" = "1" ]; then
    ip="<ALB 的公网/内网地址>"
    src="ALB（TLS 在 ALB 终止，后端 NodePort）"
  elif [ "$WITH_ISTIO" = "1" ]; then
    ip=$(kubectl -n istio-system get svc istio-ingressgateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    src="istio-ingressgateway（TLS 由网关终止）"
  else
    ip=$(kubectl -n "$NS_SYS" get svc dsh-router -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    src="dsh-router ServiceLB（HTTP，无 TLS）"
  fi
  cat <<EOF

  入口地址：${ip:-<pending，等 EIP/SLB 分配>}   ← $src
  子域解析：把 *.$DSH_DOMAIN 指向 ${ip:-<上面的 IP>}
  入口协议：$([ "$WITH_ISTIO" = "1" ] && echo "https://<slug>.$DSH_DOMAIN" || echo "http://<slug>.${DSH_DOMAIN}（无 TLS，验证用）")

  验证（当前模式: $([ "$KATA_MODE" = "1" ] && echo kata || echo runc)）：
    SCHEME=http ./verify.sh
    $verify_cmd

  排查：
    kubectl -n $NS_SYS get pods
    kubectl -n $NS_USERS get pods
    kubectl -n $NS_SYS logs deploy/dsh-router  | jq -c 'select(.level!="INFO")'
    kubectl -n $NS_SYS logs deploy/dsh-control | jq -c 'select(.event=="provision")'

EOF
}

step_check
step_certs
step_base
step_pull_secret
step_secrets
step_render
step_database
step_apps
step_istio
step_alb
step_provision
step_done
