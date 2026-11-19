# 本地验证（macOS + k3d）

目标：在本机把**路由、认证、隔离、休眠/唤醒**跑通，不等同于生产验收。

## 一、四条必须换掉的差异

| 生产 | 本地 | 为什么 |
|---|---|---|
| Kata 微虚拟机（`kata-clh-runtime-rs`） | runc，去掉 `runtimeClassName` | Docker Desktop 的 Linux 里没有 `/dev/kvm`，Kata 起不来 |
| Ceph RBD / CephFS | k3s 自带 `local-path` | 没有 Ceph；local-path 是本地文件系统，`flock` 正常 |
| Istio 通配 TLS + 子域 | 直接把 router 暴露到本机 80 | 少一层，先验身份与路由逻辑 |
| 内部 CA 签的证书 | 同左，但主机不信任 | 用 `curl -k`，或把 `ca.crt` 加进钥匙串 |

**所以本地验不了的：** 微虚拟机的内核隔离、跨节点卷重挂、故障转移 SLO。

### NetworkPolicy 是生效的

k3s 自带 network policy 控制器（kube-router），即使 CNI 是 flannel 也会**真正执行** NetworkPolicy，
不需要换 Calico。实证：`dsh-users` 的策略只放行内网 `10.20.0.0/16`，用户 pod 访问
`api.deepseek.com:443` 被挡；用 `up.sh` 补一条公网 443 的放行策略后立刻恢复 200。

所以 `verify.sh` 第 3 条（pod 之间不可互访）是真检查，不是假通过。
唯一的例外是显式加了 `--disable-network-policy` 建集群——`up.sh` 没有加。

## 二、准备工作

```sh
brew install k3d kubectl
# Docker Desktop 或 OrbStack，内存至少给 8 GB
go version    # ≥ 1.22，用来构建 control / router
```

k3s 版本必须 ≥ 1.30（`ValidatingAdmissionPolicy` GA）。脚本默认用 `rancher/k3s:v1.31.5-k3s1`。

### 镜像加速器

k3s 节点默认直连 Docker Hub。国内基本拉不动，而 `pause` / `busybox`（PVC 助手）/
`klipper-lb`（LoadBalancer）这些都必须在集群里能拉到，否则 pod 会卡在 `ImagePullBackOff`。

`up.sh` 会在**建集群时**把加速器写进 `dev/render/registries.yaml` 并通过
`--registry-config` 传给 k3d（与 `--registry-use` 的私有 registry 条目自动合并）：

```sh
export DOCKER_MIRROR=https://docker.m.daocloud.io   # 默认值
export DOCKER_MIRROR=                               # 置空 = 不配加速器
```

**已经建好的集群补不了这个配置**：k3s 只在启动时读 `registries.yaml`，而重启 k3d 节点
会换掉容器 IP，k3s 缓存的 kubelet 服务端证书仍指向旧 IP，之后 `exec` / `logs` /
`port-forward` 会全部报 `x509: certificate is valid for ...`。所以只能重建：

```sh
k3d cluster delete dsh && ./dev/up.sh
```

同理，遇到 `ImagePullBackOff` 时不要用 `docker restart k3d-dsh-agent-0` 来"修"，重启后
执行通道就断了。要临时补镜像，用宿主机拉 + `k3d image import`。


## 三、一条命令

```sh
./dev/up.sh
```

它做九件事：建集群 → 装 cert-manager → 生成自签 CA → 建 Secret → 构建并导入四个镜像 →
起 Postgres 并灌 schema → 起 control/router/reaper → 开通 alice → 等 pod 就绪。

成功后会打印：

```
把子域指向本机（需要 sudo）：
  echo '127.0.0.1 alice.dsh.corp bob.dsh.corp' | sudo tee -a /etc/hosts
```

## 四、验收

```sh
SCHEME=http SKIP_KATA=1 ./verify.sh
```

| 检查 | 本地能不能验 |
|---|---|
| pod 就绪 / owner 标记 | 能 |
| runtimeClassName + guest 内核 | **不能**（`SKIP_KATA=1` 跳过） |
| 准入策略拒绝错误 PVC 绑定 | 能（k3s ≥ 1.30） |
| pod 之间不可互访 | 能（k3s 内置 netpol 控制器会执行，实测有效） |
| 未认证访问跳 SSO | 能 |
| 跨身份访问 403 | 能 |
| 重定向不含 token | 能 |

## 五、单独验几件事

**休眠与唤醒**（这是本轮新增的回路，最重要）

```sh
# 手动把 openTurn 置 false 并把活动时间调老
kubectl -n dsh-system port-forward svc/dsh-control 8080:8080 &
curl -s -X POST localhost:8080/internal/activity \
  -H "Authorization: Bearer $REGISTER_TOKEN" -H 'content-type: application/json' \
  -d '{"subject":"alice@corp","openTurn":false,"lastActivityAt":0}'

# 观察回收器缩容
kubectl -n dsh-users get deploy dsh-alice -w

# 再访问一次，观察唤醒
curl -sk -o /dev/null -w '%{http_code}\n' http://alice.dsh.corp/
```

**open turn 时不许回收**

```sh
curl -s -X POST localhost:8080/internal/activity \
  -H "Authorization: Bearer $REGISTER_TOKEN" -H 'content-type: application/json' \
  -d '{"subject":"alice@corp","openTurn":true,"lastActivityAt":0}'
# 等 2 分钟，deploy 副本应保持 1
```

**活动插件是否真的在上报**

```sh
kubectl -n dsh-users logs deploy/dsh-alice -c dsh | grep -i activity
kubectl -n dsh-system logs deploy/dsh-control | grep activity
```

若插件没装成功（镜像构建时提示 WARN），用上面的手工 POST 也能验回收逻辑。

## 六、清理

```sh
k3d cluster delete dsh
sudo sed -i '' '/dsh.corp/d' /etc/hosts
```

---

# 七、端到端跑一个真实用户

上面那套验的是结构和策略。想在浏览器里真的跟 alice 聊一轮，还差**模型后端**和**登录链路**两块。

## 7.0 先用本机 DSH 确认 key 和模型可用（不碰 k8s）

这一步能在 2 分钟内排除"到底是模型配置问题还是平台问题"：

```sh
export DEEPSEEK_API_KEY=sk-...
npm i -g @deepseek-ai/dsh@0.2.1-alpha.4

DSH_HOME=/tmp/dsh-selftest dsh --profile headless "用一句话介绍你自己"
```

- 有回答 → key 和模型链路没问题，继续往下
- 报 `MISSING_CREDENTIAL` / `INVALID_CREDENTIAL` → key 没生效
- 报 404 / 405 → 端点不对，检查 `baseURL`

## 7.1 模型

`image/profile/cordis.patch.yml` 里写的是内网网关 `https://llm-gw.corp/anthropic`，本机解析不到。
`Dockerfile.dev` 用构建参数替换它：

DeepSeek 官方服务就是 Messages 兼容端点，`MODEL_BASE_URL` 默认已经是它：

```sh
export DEEPSEEK_API_KEY=sk-...                            # 真实 key
export MODEL_BASE_URL=https://api.deepseek.com/anthropic  # 默认值，可换内网网关
REQUIRE_MODEL_KEY=1 ./dev/up.sh                           # 没 key 直接失败
```

`baseURL` 会被适配器补成 `https://api.deepseek.com/anthropic/v1/messages`；
换成内网网关时只改 `MODEL_BASE_URL`，其余不用动。

`up.sh` 会把 key 写进 `dsh-model-token-alice` 这个 Secret，并以 `DEEPSEEK_API_KEY`
注入容器（`Dockerfile.dev` 同时把 profile 的 `apiKeyEnv` 改成这个名字）。

没有 key 也能启动：会打一条 WARN，身份/路由/回收都正常，只有真正发消息时会失败。

## 7.2 登录

生产走企业 IdP，本地没有。`router` 加了一个**只在 `DEV_SUBJECT` 非空时挂载**的
`/_dev/sso`：

```
浏览器 → http://alice.dsh.corp/
router  → 302 /_dsh/login?next=%2F
登录页  → 点击「使用企业账号登录」
router  → 302 /_dev/sso?return=http%3A%2F%2Falice.dsh.corp%2F_auth%2Fcallback
/_dev/sso → 302 .../_auth/callback?ticket=alice@corp|<exp>|<hmac>
router  → 校验 ticket（和正式流程同一段代码）→ 种会话 cookie → 302 /
```

不想多点这一次：

```sh
kubectl -n dsh-system set env deploy/dsh-router LOGIN_MODE=direct
```

关键点：**ticket 的签名与校验走的是正式代码路径**，只有"谁来签发"被替换了。
所以这个流程验证的是真实的认证逻辑，不是旁路。

### 本地验多个用户

假 IdP 默认只签发 `DEV_SUBJECT`。而会话 cookie 是 host-only 的，如果只按默认身份签，
你访问 `usera.dsh.corp` 会拿到 alice 的 cookie，随后被 Host 与主体一致性检查拦成 **403**
（这是隔离在起作用，不是 bug）。

所以 `/_dev/sso` 支持按子域决定身份：

```
访问 http://usera.dsh.corp/
router → 302 /_dev/sso?...&subject=usera%40corp
```

规则：slug 取主机名第一段，域后缀沿用 `DEV_SUBJECT` 里 `@` 之后的部分
（`usera.dsh.corp` + `DEV_SUBJECT=alice@corp` → `usera@corp`）。
也可以手动指定：`/_dev/sso?subject=bob@corp&return=...`。

生产（`DEV_SUBJECT` 为空）不会从 Host 推导身份，这段逻辑不生效。
换了代码后如果浏览器还留着旧的 403 会话，用 DevTools 清掉 `usera.dsh.corp` 的
`dsh_sess`，或在页面里点「退出登录」后重新进。

`up.sh` 已经设好：`DEV_SUBJECT=alice@corp PUBLIC_SCHEME=http SSO_URL=/_dev/sso COOKIE_SECURE=false`。

## 7.3 跑起来

```sh
echo '127.0.0.1 alice.dsh.corp bob.dsh.corp' | sudo tee -a /etc/hosts
open http://alice.dsh.corp/
```

应该看到 DSH 的 Web UI，直接可用（session 归属 alice 的 pod）。

点"新建会话"发一句话，验证四件事：

1. 模型返回正常 → 说明 profile 的 `baseURL` / `apiKeyEnv` 生效
2. `kubectl -n dsh-users logs deploy/dsh-alice -c dsh | grep register` → pod 上报成功
3. `kubectl -n dsh-system logs deploy/dsh-control | grep activity` → 插件在心跳
4. 发消息期间 `openTurn` 为 `true`：

```sh
source dev/render/tokens.env
kubectl -n dsh-system port-forward svc/dsh-control 8080:8080 &
curl -s -H "Authorization: Bearer $ROUTER_TOKEN" \
  'localhost:8080/internal/lookup?subject=alice@corp' | jq .openTurn
```

## 7.4 用 bob 验隔离

```sh
kubectl -n dsh-system port-forward svc/dsh-control 18080:8080 &
ADMIN_TOKEN=... python3 provision.py bob@corp \
  --model-token "$DEEPSEEK_API_KEY" \
  --control-url http://127.0.0.1:18080
```

然后：

- `http://bob.dsh.corp/` 用 bob 的会话能正常用
- 用 alice 的浏览器访问 `bob.dsh.corp` → **403**（Host 与主体不一致）
- 两个控件的会话列表互不可见

## 7.5 端到端验不了什么

- IdP 的真实登录（OIDC 跳转、ticket 一次性、重放拒绝）
- 通配证书与浏览器信任链
- 微虚拟机隔离

前两条要在接上真实 IdP 的预发环境验，第三条只能在支持 KVM 的 Linux 节点上验。

---

# 八、DSH_REGISTRY：本地要不要搭仓库

先分清它是什么。`DSH_REGISTRY` 不是 DSH 的环境变量，是这个仓库自己的约定，
只出现在两处：`Makefile` 的四个 `*-image` target（决定 `docker build/push` 的 tag 前缀），
以及 README 的变量表。清单里的 `registry.corp/...` 是硬编码常量，不读这个变量。

**所以默认的 `./dev/up.sh` 完全不需要仓库**：它把镜像 build 成 `dsh/*:local`，
`k3d image import` 直接灌进 containerd，再 sed 掉清单里的 `registry.corp`。
走这条路，`DSH_REGISTRY` 一个字都不用设。

需要搭的场景只有一个：想验「pod 从私有 registry 拉镜像」这条生产路径。

## 8.1 打开 registry 模式

```sh
k3d registry create dsh-reg --port 5000     # 也可以跳过，up.sh 会按 K3D_REGISTRY 自动建

export DSH_REGISTRY=localhost:5000          # host 侧 push 地址
export CLUSTER_REGISTRY=k3d-dsh-reg:5000    # 集群内 pull 地址
./dev/up.sh
```

三个变量的分工：

| 变量 | 谁用 | 取值 |
|---|---|---|
| `DSH_REGISTRY` | host 上的 `docker push` | 只能写 `localhost:5000`，`k3d-xxx` 在 host 上解析不到 |
| `CLUSTER_REGISTRY` | 清单里的 image、pod 的 pull | `k3d-<registry名>:<端口>`，只存在于 docker 网络内 |
| `K3D_REGISTRY` | 可选，不存在时自动 `k3d registry create` | 端口从 `DSH_REGISTRY` 里取 |

两个名字不同不会出错：registry 只按 **repo path** 存内容，不记 host。
`localhost:5000/dsh-control:dev-101500` 和 `k3d-dsh-reg:5000/dsh-control:dev-101500`
落在同一个 repo `dsh-control` 上。可以自己验：

```sh
curl -s http://localhost:5000/v2/_catalog | jq
```

## 8.2 三个坑

1. **registry 必须和集群一起建**。`k3d cluster edit` 只能改端口，没有 `--registry-use`。
   已经建好的集群只能 `k3d cluster delete dsh` 重来，up.sh 会打 WARN 提醒。
2. **tag 带时间戳**（`dev-HHMMSS`）。清单没写 `imagePullPolicy`，默认 `IfNotPresent`，
   固定 tag 重建后 pod 不会重新拉，会一直跑旧镜像。
3. **企业真实 registry 还要两件事**：TLS 证书被节点信任，以及 `imagePullSecrets`。
   k3d 内置 registry 是明文 HTTP，这些它替你藏起来了，别以为生产也能这么干。

## 8.3 用企业 registry 时

`make image/router-image/control-image/reaper-image` 只认 `DSH_REGISTRY`，
不碰清单。所以推上去之后，还要把清单里的 `registry.corp` 换掉
（生产用 kustomize 或 helm，本地用 up.sh 的 sed）：

```sh
export DSH_REGISTRY=harbor.corp/dsh-platform   # 含项目路径
export DSH_VERSION=0.2.1-alpha.4
make image control-image router-image reaper-image
```

---

# 九、在集群外调试 control（开通流程）

control 的开通能力在集群里走 ServiceAccount，在集群外走 kubeconfig；两条路是**同一份
`Provision` 代码**，所以本地能复现线上的行为，只差 RBAC 授权（本地是集群管理员）。

## 9.1 让 control 连上集群

```sh
export DSH_K8S_KUBECONFIG=dev/render/kubeconfig.yaml   # k3d kubeconfig get dsh > 该文件
```

代码见 `control/k8s_local.go`：借 `kubectl config view --raw --minify -o json` 把
kubeconfig 归一化成 JSON，支持 token 和客户端证书两种凭据（k3d 默认是后者），
并把 k3d 写的 `0.0.0.0:PORT` 自动换成 `127.0.0.1:PORT`。

启动时会打一行 `provisioning enabled source=kubeconfig=...`；打不出来说明凭据没读到，
日志里会跟一条 `provisioning disabled reason=...`。

## 9.2 存储用本地 Postgres

```sh
docker run -d --name dsh-pg -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=dsh \
  -e POSTGRES_DB=dsh -p 55433:5432 postgres:16-alpine
docker exec -i dsh-pg psql -U dsh -d dsh < control/schema.sql
```

或者复用集群里的库：`kubectl -n dsh-system port-forward svc/dsh-postgres 5432:5432`。

## 9.3 跑起来

```sh
cd control && go build -o /tmp/dsh-control .

ADMIN_TOKEN=admin REGISTER_TOKEN=reg ROUTER_TOKEN=router \
DATABASE_URL='postgres://dsh:pw@127.0.0.1:55433/dsh?sslmode=disable' \
DSH_K8S_KUBECONFIG=$PWD/../dev/render/kubeconfig.yaml \
DSH_NAMESPACE=dsh-users DSH_BASE_DOMAIN=dsh.corp DSH_IMAGE=dsh/dsh:local \
DSH_STORAGE_CLASS=local-path DSH_RUNTIME_CLASS=none DSH_ISSUER=dsh-internal-ca \
LOG_LEVEL=debug PORT=18080 /tmp/dsh-control
```

`DSH_RUNTIME_CLASS=none` 是本地专用的：k3d 没有 Kata，留着会让 Deployment 引到不存在的 RuntimeClass。

## 9.4 复现一次开通

```sh
curl -s -X POST localhost:18080/internal/provision \
  -H "Authorization: Bearer admin" \
  -H "X-DSH-Actor: local-debug" \
  -H "X-Request-ID: debug-001" \
  -d '{"subject":"bob@corp","tier":"T1","modelToken":"sk-test"}' | jq
```

第二次去掉 `modelToken` 再发一次，就是幂等路径：Secret 复用、其它对象 PATCH。

## 9.5 日志怎么读

`LOG_LEVEL=info`（默认）只有阶段与结果；`debug` 才加每条 k8s 请求。

```json
{"level":"INFO","msg":"provision step","req":"debug-001","event":"provision",
 "subject":"bob@corp","actor":"local-debug","step":"deployment","result":"created","ms":8}
```

| 字段 | 用途 |
|---|---|
| `req` | 贯穿一次调用的 request id；同一次点击的所有日志都带它，`grep debug-001` 即可 |
| `step` | secret / pvc / certificate / deployment / store，失败时看它是哪一步 |
| `result` | created / patched / unchanged / reused / upserted，幂等语义的可观测形式 |
| `ms` | 该阶段耗时 |
| `status` + `reason` | 仅失败时出现，来自 k8s `Status`；403 就是 RBAC，409 是冲突 |

线上直接查：

```sh
kubectl -n dsh-system logs deploy/dsh-control | jq -c 'select(.event=="provision")' | tail
# 只看失败阶段
kubectl -n dsh-system logs deploy/dsh-control | jq -c 'select(.msg=="provision step failed")'
```

密钥不会出现在日志里：k8s 请求日志只记 method/path/status，不记 body。

## 9.6 跨服务追一次请求

router 在入口读取（没有就生成）`X-Request-ID`，回写到响应头，并把它带到每个出站请求
（`/internal/lookup`、`/internal/desired`、管理台 API）；control 用它打日志，再带到 k8s 请求。
所以「浏览器 → router → control → API server」是同一条链：

```sh
# 1) 浏览器或 curl 指定 id；响应头里会原样带回来
curl -s -D- -o /dev/null -H 'X-Request-ID: debug-001' http://alice.dsh.corp/_dsh/status | grep -i x-request-id

# 2) 两个服务都用同一个 id 过滤
kubectl -n dsh-system logs deploy/dsh-router  | jq -c 'select(.req=="debug-001")'
kubectl -n dsh-system logs deploy/dsh-control | jq -c 'select(.req=="debug-001")'
```

原来 `ALERT ...` 的告警日志现在是 `"alert":true` 字段，检索方式不变：
`jq -c 'select(.alert==true)'`。


