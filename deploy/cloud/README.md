# 云上全链路验证

在**一台** Linux 云主机上把 DSH 从入口跑到模型，重点验证三件本地验不了的事：
Kata 微虚拟机隔离、NetworkPolicy 真实生效、跨节点之外的全部链路。

> 这不是生产部署。单机没有 HA，存储用 local-path，TLS 层（Istio）与非 dev 登录都没上。
> 生产形态见 [dsh-enterprise-deploy-checklist.md](../../dsh-enterprise-deploy-checklist.md)。

## 需要几台机器

| 验证目标 | 台数 | 规格建议 |
|---|---|---|
| 只验功能链路（runc，无 Istio） | **1** | 8C32G，ESSD PL1 200G |
| **功能 + Istio 入口 + 真实 TLS** | **2 最低 / 3 推荐** | 8C32G ×3 |
| 再加 Kata 隔离 | +2 台**弹性裸金属** | 16C64G ×2（Kata 节点池） |

为什么推荐 3 台而不是 1 台：

- `dsh-control` 2 副本、`dsh-router` 3 副本，单节点时它们**全在一个故障域**，副本只是看起来有冗余
- Istio ingress gateway 会吃一份 CPU/内存，和用户 pod 抢资源
- 只有 ≥2 个工作节点才能做"杀一个节点看 pod 重建"的演练

为什么 Kata 要单独两台裸金属：`02-runtimeclass.yaml` 的 `nodeSelector` 要求节点带
`katacontainers.io/kata-runtime=true`，且节点被打了 `dsh.corp/dedicated=dsh:NoSchedule` 污点；
控制面、Istio 网关不需要 KVM，留在便宜的普通节点上即可。

## 0. 能验什么 / 不能验什么

| 能力 | 单机验证 | 说明 |
|---|---|---|
| 浏览器 → router → control → pod → 模型 | ✅ | 全链路 |
| 用户开通 / 休眠唤醒 / 活动回传 | ✅ | |
| Kata 微虚拟机隔离 | ✅ | 需要支持的实例规格，见下 |
| NetworkPolicy 强制 | ✅ | k3s 内置控制器，不是假通过 |
| 准入策略（VAP） | ✅ | 需要 K8s ≥1.30 |
| HA / 故障转移 / 跨节点卷重挂 | ❌ | 至少要 3 节点 |

## 1. 选实例（最关键的一步）

```sh
# 买之前先查规格是否支持嵌套虚拟化
aliyun ecs DescribeInstanceTypes --RegionId cn-hangzhou \
  --InstanceTypes '["ecs.g7.2xlarge","ecs.ebmgn7e.8xlarge"]' \
| jq '.InstanceTypes.InstanceType[] | {InstanceTypeId, NestedVirtualizationSupport}'
```

- **弹性裸金属**（`ecs.ebm*`）：官方明确"二次虚拟化能力默认开启"，一定有 `/dev/kvm`
- **普通实例**：多数没有 `/dev/kvm`，Kata 起不来；即使 `NestedVirtualizationSupport=Supported` 也要开机后实测
- 起步配置：8–16 vCPU / 32 GiB / 数据盘 ESSD PL1 ≥200 GiB（用户卷 30 GiB/人）
- 出网：EIP 或 NAT 网关（拉镜像、调 `api.deepseek.com` 都要）

### 普通 ECS（没有 KVM）也能部署

完全可以，只是验不了微虚拟机隔离这一层：

```sh
ALLOW_NO_KVM=1 sudo ./deploy/cloud/preflight.sh    # KVM 两项降级为 WARN
# 装 k3s 后直接部署，跳过 Kata：
DISABLE_KATA=1 DSH_REGISTRY=... DSH_DOMAIN=... ./deploy/cloud/up.sh
```

此时用户 pod 跑 runc，但 `PodSecurity restricted`、`NetworkPolicy`、一人一卷、非 root、
资源 limit 全都照常生效——隔离是**进程级 + 命名空间级**，而不是独立内核。
功能链路（路由、认证、开通、休眠唤醒、模型调用）与 Kata 模式下完全一致。

适合：功能验证、内部演示、开发联调。
不适合：把不可信代码真跑在上面（内核逃逸会波及整机与其他用户）。

没有 Kata 时用 `verify-isolation.sh` 验收（11 项断言，见第 4 节）。

买完先跑体检：

```sh
sudo ./deploy/cloud/preflight.sh
```

它有 FAIL 就别往下走——那些 FAIL 在云上基本等价于"实例规格选错了"。

## 2. 系统准备

```sh
sudo swapoff -a && sudo sed -i '/ swap /s/^/#/' /etc/fstab
sudo modprobe br_netfilter && echo br_netfilter | sudo tee /etc/modules-load.d/br_netfilter.conf
sudo apt install -y chrony && sudo systemctl enable --now chrony   # 时间窗：ticket 60s、cookie 12h
```

安全组/防火墙放行：22（限源 IP）、80、443、6443（限源 IP）。

## 3. 装 k3s

```sh
curl -sfL https://get.k3s.io | sudo INSTALL_K3S_VERSION="v1.31.5+k3s1" sh -s - --write-kubeconfig-mode 644
sudo kubectl wait --for=condition=Ready node --all --timeout=180s
```

两点注意：

- **不要加 `--disable-network-policy`**，否则本仓库的 NetworkPolicy 全部失效
- k3s ≥1.30 是硬要求（`ValidatingAdmissionPolicy` GA）

## 4. Kata（可选）与隔离验收

**先判断要不要装**：只有实例有 `/dev/kvm` 才装。

### 4.1 有 KVM：装 Kata

```sh
./deploy/cloud/install-kata.sh     # kata-deploy + RuntimeClass 对齐 + 节点标签/污点
./deploy/cloud/verify-kata.sh      # 内核对比 + 反向对照，证明真的在微虚拟机里
```

`verify-kata.sh` 的判据是：guest 内核 ≠ 宿主内核、guest 内无 `/dev/kvm`、同节点的 runc pod 内核 == 宿主内核。
三条同时成立才算通过——只设 `runtimeClassName` 而 shim 没装好，pod 会卡在 `ContainerCreating`。

如果 RuntimeClass 名字不是 `kata-clh-runtime-rs`，用 `RC=<实际名字>` 跑验证，并相应调整部署时的 `DSH_RUNTIME_CLASS`。

### 4.2 没有 KVM（普通 ECS）：跳过 Kata，验容器级隔离

`up.sh` 会**自动识别**：集群里没有 Kata RuntimeClass 就降级为 runc，并打印
`模式: runc（…自动降级）`。不需要手动设 `DISABLE_KATA=1`（那是给"有 Kata 但想跑 runc"用的）。

部署完成后跑：

```sh
./deploy/cloud/verify-isolation.sh
```

11 项断言，覆盖没有 Kata 时仍然必须成立的部分：

| 断言 | 为什么重要 |
|---|---|
| uid ≠ 0 | 容器内不能是 root |
| `CapEff` 全 0 | 不能 mount、不能 ptrace、不能改网络 |
| `Seccomp=2` | syscall 过滤生效 |
| 无 hostPath / docker.sock | 不能碰宿主文件系统或容器运行时 |
| 根目录不可写 | 持久化只能落到自己的 PVC |
| 公网 443 可达 | 模型网关走通 |
| K8s API server 被阻断 | 用户容器拿不到集群管理面 |
| postgres 端口被阻断 | 网络策略不是摆设 |
| SA 无实权 | pod 里的 token 不等于权限 |
| 资源 limits 存在 | 一个用户吃不掉整节点 |

## 5. 推镜像

### 5.1 先写配置

```sh
cp deploy/cloud/env.example deploy/cloud/env    # env 已被 gitignore
$EDITOR deploy/cloud/env                        # 填 DSH_REGISTRY / DSH_DOMAIN / PULL_SECRET
PRINT_CONFIG=1 ./deploy/cloud/up.sh             # 核对解析结果，不部署
```

### 5.2 构建并推送

ACR 个人版有两个域名，**本地 push 和集群 pull 用的不是同一个**：

| 场景 | 域名 |
|---|---|
| 本地 `docker push` | `crpi-xxx.cn-beijing.personal.cr.aliyuncs.com/dsh_ljx` |
| 集群 `pull` | `crpi-xxx-vpc.cn-beijing.personal.cr.aliyuncs.com/dsh_ljx` |

两个域名指向同一实例，registry 只按 repo 路径存，所以「公网 push + VPC pull」是同一份镜像。脚本会自动把 VPC 域名去掉 `-vpc` 得到公网域名：

```sh
./deploy/cloud/push-images.sh                 # 构建 + 推送（默认 linux/amd64）
SKIP_PUSH=1 ./deploy/cloud/push-images.sh     # 只构建打 tag，先看看
IMAGES="dsh-router" ./deploy/cloud/push-images.sh   # 只重建改动过的镜像
PLATFORM=linux/arm64 ./deploy/cloud/push-images.sh  # 目标不是 amd64 时改这个
```

Apple Silicon 上构建必须带 `--platform linux/amd64`，否则推上去是 arm64 镜像，ECS 上会
`exec format error`。脚本默认就是 amd64，并在推送前校验每个镜像的架构，不匹配直接中止。

### 5.3 给集群配拉取凭据

两个命名空间各一份（用户 pod 用的是 `dsh-workload` 这个 SA）：

```sh
for ns in dsh-system dsh-users; do
  kubectl -n $ns create secret docker-registry acr-pull \
    --docker-server=crpi-xxx-vpc.cn-beijing.personal.cr.aliyuncs.com \
    --docker-username=<阿里云账号> --docker-password=<ACR 独立密码>
done
```

## 6. 部署

```sh
DEEPSEEK_API_KEY=sk-... ./deploy/cloud/up.sh     # 其余参数从 deploy/cloud/env 读
```

没有 KVM 的机器可以 `DISABLE_KATA=1` 降级为 runc 验证（这一步就不再验微虚拟机隔离了）。

脚本做的事：cert-manager + 内部 CA → 命名空间/RBAC/网络/准入/Issuer → 令牌与模型凭据 →
渲染清单（换 registry 与 StorageClass）→ Postgres 与 schema → control/router/reaper →
开通示例用户。全程幂等，可重复跑。

**域名不会被 sed 改**：清单里的 `dsh.corp` 同时是标签键（`dsh.corp/user`），做文本替换会改坏标签。
域名只通过 `DSH_BASE_DOMAIN` 环境变量注入。

## 7. 入口：HTTP 或 Istio + TLS

### 7.1 快速模式（默认，无 TLS）

`up.sh` 把 router 的 Service 改成 LoadBalancer：

```sh
kubectl -n dsh-system get svc dsh-router   # 等 EXTERNAL-IP
```

把 `*.<域名>` 解析到那个 IP，本机也可以 `curl --resolve alice.dsh.example.com:80:<IP> ...`。

### 7.2 真实场景：Istio 终止 TLS

**先装 Istio**（`numTrustedProxies` 必须设，否则 Host 被改写、router 的 Host 校验会 403）：

```sh
istioctl install --set meshConfig.defaultConfig.gatewayTopology.numTrustedProxies=1
kubectl -n istio-system get svc istio-ingressgateway   # 等 EXTERNAL-IP
```

**再让 up.sh 带上入口层**：

```sh
WITH_ISTIO=1 \
EDGE_ISSUER=<你的公网 issuer> \
DSH_REGISTRY=... DSH_DOMAIN=dsh.example.com DEEPSEEK_API_KEY=sk-... \
./deploy/cloud/up.sh
```

它会：把 `*.dsh.corp` 通配换成你的域名（只替换通配值，不动 `dsh.corp/user` 标签键）→
等通配证书 Ready → 应用 Gateway/VirtualService → 把 router 的
`PUBLIC_SCHEME=https`、`COOKIE_SECURE=true`、Service 降回 ClusterIP。

**证书是这里最容易翻车的点**：

| 来源 | 浏览器信任 | 做法 |
|---|---|---|
| 仓库自带的内部 CA（默认） | ❌ | 只能 `curl -k`；脚本会打 WARN |
| 公网 CA | ✅ | `EDGE_ISSUER` 指向 cert-manager 的 DNS-01 issuer（阿里云 DNS 需要 webhook） |
| 企业 CA | ✅（域内机器） | 手工把证书导入 `istio-system/dsh-wildcard-tls`，跳过 08 的 Certificate |

**登录方式**：`KEEP_DEV_LOGIN=1`（默认）保留 `/_dev/sso` 假 IdP，用来先验 TLS 链路；
接真实 IdP 时设 `KEEP_DEV_LOGIN=0 SSO_URL=https://sso.example.com/login`。

### 7.3 云上已有 ALB：TLS 在 ALB 终止

不要在集群内再启一层 TLS。设 `ALB_MODE=1`：

```sh
ALB_MODE=1 DEEPSEEK_API_KEY=sk-... ./deploy/cloud/up.sh
```

它会先按 `NODE_PORT`（默认 31147）把 Service 的 nodePort 固定下来，然后做四件事：

1. Service 改成 `NodePort` + `externalTrafficPolicy: Local`
   （kube-proxy 只转本节点 pod，ALB 探测语义才准确；节点没 pod 该后端自然不通）
2. router 设 `PUBLIC_SCHEME=auto`，并删掉 `COOKIE_SECURE` 让它跟随协议
   - ALB 带了 `X-Forwarded-Proto: https` → 回调用 https、cookie 带 `Secure`
   - ALB 只开 80（没有该头）→ 回调保持 http、cookie 不带 `Secure`
   - **绝不把 http 请求升级成 https**，所以 ALB 只配 80 时不会被跳到没人监听的 443
3. 等 rollout 完成
4. 打印 ALB 后端配置清单

ALB 侧照这个配：

| 项 | 值 |
|---|---|
| 后端协议 / 端口 | HTTP / `NODE_PORT`（默认 31147，可改；集群重建后不会漂） |
| 健康检查 | `GET /healthz`，间隔 10s，超时 5s，不健康 3 次 |
| Host | **原样透传**，改写会导致 router 的 Host 校验 403 |

协议也可以交给 router 自动判断：`PUBLIC_SCHEME=auto` 会读 `X-Forwarded-Proto`（ALB 七层
默认会带），其次退回连接本身的 TLS 状态；`COOKIE_SECURE` 未显式设置时跟随协议，
不会出现「回调是 https、cookie 没 Secure」这种组合坑。

```
PUBLIC_SCHEME=auto
```
| 会话保持 | 不需要（会话在 cookie 里） |
| 安全组 | 只放行 ALB 后端地址段到该 NodePort |

`ALB_MODE=1` 与 `WITH_ISTIO=1` 互斥——TLS 只能在一个地方终止，同时开会直接报错退出。

**健康检查的分工**（谁汇报什么）：

| 层 | 谁探测 | 管什么 |
|---|---|---|
| Pod | kubelet `readinessProbe`（5s，仓库已配） | 不 Ready 就从 Endpoints 摘掉 |
| Pod 存活 | kubelet `livenessProbe`（10s，router 已配） | 进程 hang 住时重启 |
| 节点 | ALB 主动 `GET /healthz` | 这个后端能不能用 |

svclb / klipper **不做健康检查**，它只是端口转发——所以 `ALB → svclb → router` 不会比 `ALB → NodePort` 更"及时"，只会多一个依赖。

## 7.4 开通与删除用户

开通（管理页 `/_dsh/admin`，或命令行）：

```sh
ADMIN_TOKEN=$(kubectl -n dsh-system get secret dsh-control-auth -o jsonpath='{.data.admin-token}' | base64 -d)
kubectl -n dsh-system port-forward svc/dsh-control 18080:8080 &
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" -H 'content-type: application/json' \
  -d '{"subject":"bob@corp","tier":"T1","modelToken":"sk-..."}' \
  http://127.0.0.1:18080/internal/provision
```

删除（软删除，默认保留数据卷）：

```sh
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  'http://127.0.0.1:18080/internal/users?subject=bob%40corp'
```

| 参数 | 默认 | 作用 |
|---|---|---|
| `keepData` | `1` | 保留 PVC（用户数据）；`keepData=0` 才真删 |
| `purge` | `0` | `purge=1` 连数据库行一起物理删除（不可恢复） |

删除做四件事，按顺序：

1. 数据库软删除（`deleted_at` 置位）+ 清空 pod 信息 → router 立刻查不到，用户马上 503
2. 删 Deployment → pod 消失
3. 删 Certificate 与它签出的 TLS Secret
4. 按 `keepData` 决定是否删 PVC

管理页每行也有「删除」按钮，等价于 `keepData=1`（数据保留）。

**PVC 命名**：`dsh-home-<slug>`（如 `dsh-home-bob`）。早期版本用
`dsh-home-<sha256 前 12 位>`，provision 会检测旧名并继续沿用，不会给老用户分一个空卷。

**定时清理**：`Store.DeletedBefore(duration)` 已就绪，后续在 control 里挂一个
定时任务调用 `Purge` 即可（软删除超过 N 天的行才真正消失）。

## 8. 验收

```sh
SCHEME=http ./verify.sh                  # Kata 模式：不设 SKIP_KATA
SCHEME=http SKIP_KATA=1 ./verify.sh      # runc 模式：跳过两条内核断言
./deploy/cloud/verify-isolation.sh       # 两种模式都该跑
[有 KVM] ./deploy/cloud/verify-kata.sh   # 只在 Kata 模式
```

再加上清单要求的三个必跑项：

1. 杀掉正在跑工具的 pod，确认会话标为 interrupted 且没有重复执行副作用
2. 用 alice 的会话访问 bob 的子域，必须 403；把 alice 的卷挂到 bob 的 pod，必须启动失败
3. 导出 alice 的会话，确认每条 user message 带 actor

## 9. 排查入口

```sh
kubectl -n dsh-system get pods
kubectl -n dsh-users  get pods
kubectl -n dsh-system logs deploy/dsh-router  | jq -c 'select(.level!="INFO")'
kubectl -n dsh-system logs deploy/dsh-control | jq -c 'select(.event=="provision")'
kubectl -n dsh-system logs deploy/dsh-reaper  --tail=20
```

用户 pod 起不来时按这个顺序看：`ImagePullBackOff`（registry/PULL_SECRET）→
`ContainerCreating`（Kata shim / PVC 绑定）→ CrashLoop（读 `/data/dsh/logs/startup-*.log`）。

## 10. 销毁（按量付费别忘）

```sh
# 先确认没在用的数据
kubectl -n dsh-users  get pvc
kubectl -n dsh-system get pvc

# k3s
/usr/local/bin/k3s-uninstall.sh

# 实例：控制台释放（按量付费实例开机即计费）
```

## 脚本清单

| 文件 | 作用 |
|---|---|
| [preflight.sh](preflight.sh) | 宿主体检：KVM、内核、swap、资源、端口、出网 |
| [install-kata.sh](install-kata.sh) | kata-deploy + RuntimeClass 对齐 + 节点标签/污点 |
| [verify-kata.sh](verify-kata.sh) | 证明 pod 真的跑在微虚拟机里（含反向对照） |
| [up.sh](up.sh) | 部署 DSH 全链路（自动识别 Kata / runc） |
| [verify-isolation.sh](verify-isolation.sh) | runc 模式的容器级隔离验收（11 项） |
| [push-images.sh](push-images.sh) | 本地构建并推送到 ACR（自动处理公网/VPC 域名） |
| [env.example](env.example) | 参数模板；复制成 `env` 后 `up.sh` 自动读取 |
