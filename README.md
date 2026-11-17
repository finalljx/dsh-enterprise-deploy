# DSH 企业内网部署 · 可执行清单

按编号顺序执行。每一步都有验证命令，失败就停下，不要跳过。

## 前置

- K8s ≥ 1.30（`ValidatingAdmissionPolicy` GA）
- 节点池启用 KVM，已安装 Kata Containers ≥ 4.0（`kata-clh-runtime-rs` handler）
- KVM 节点池带 `katacontainers.io/kata-runtime=true` 标签（kata-deploy 通常会给安装节点打；未打则手动补，否则 `RuntimeClass` 的 `nodeSelector` 匹配不到节点）
- cert-manager ≥ 1.14
- Istio ≥ 1.20（仅用 Ingress Gateway）
- 一个支持 `flock` 的 StorageClass（Ceph RBD 或 CephFS；**不要 NFSv3**）

## 变量

```sh
export DSH_DOMAIN=dsh.corp              # 子域后缀
export DSH_REGISTRY=registry.corp       # 私有镜像/包仓库
export DSH_VERSION=0.2.1-alpha.4
export IDP_ISSUER=https://sso.corp      # 企业 IdP
```

## 执行顺序

| 步 | 文件 | 作用 | 验证 |
|---|---|---|---|
| 1 | `01-namespaces.yaml` | 命名空间 | `kubectl get ns` |
| 2 | `02-runtimeclass.yaml` | Kata RuntimeClass | `kubectl get runtimeclass kata-clh-runtime-rs` |
| 3 | `03-storageclass.yaml` | 存储类 | `kubectl get sc` |
| 4 | `04-rbac.yaml` | 服务账号与权限 | `kubectl auth can-i` |
| 5 | `05-networkpolicy.yaml` | 默认拒绝 + 白名单 | 见步骤内 |
| 6 | `06-admission-policy.yaml` | PVC 与用户标签一致性 | 见步骤内 |
| 7 | `07-issuer.yaml` | 内部 CA | `kubectl get clusterissuer` |
| 8 | `08-edge-cert.yaml` | 通配边缘证书 | `kubectl get certificate -n dsh-system` |
| 9 | `09-postgres.yaml` | 控制面数据库 | `kubectl -n dsh-system exec dsh-postgres-0 -- pg_isready -U dsh` |
| 10 | `10-control.yaml` | 控制面（注册表/活动/令牌） | `curl /healthz` |
| 11 | `11-router.yaml` | 路由服务 | `curl /healthz` |
| 12 | `12-istio.yaml` | 入口网关与分流 | `kubectl get gw,vs -n dsh-system` |
| 13 | `image/` | 构建用户镜像并推到私有 registry | 见 `image/README.md` |
| 14 | `14-example-user.yaml` | 一个用户的完整资源（参考/对照用，实际由脚本渲染） | `kubectl apply --dry-run=server -f` |
| 15 | `plugin/` | 活动上报插件（装进 DSH，见 `plugin/README.md`） | DSH 日志出现 `activity-reporter` |
| 16 | `13-reaper.yaml` | 空闲回收器（含 RBAC） | `kubectl -n dsh-system logs deploy/dsh-reaper` |
| 17 | `/_dsh/admin`（或 `provision.py`） | 开通一个用户 | `kubectl get pod -n dsh-users` |
| 18 | `verify.sh` | 隔离验收 | 全绿 |

## 平台页面（用户与管理员）

| 页面 | 由谁提供 | 出现时机 |
|---|---|---|
| 登录页 `/_dsh/login` | router | 没有会话的浏览器导航；`LOGIN_MODE=direct` 可跳过，直接跳 SSO |
| 启动等待页 | router | 工作区休眠 / 注册中 / 尚未开通；每 2s 查 `/_dsh/status`，就绪后自动跳转 |
| 管理台 `/_dsh/admin` | router（调用 control 开通 API） | 管理员新增用户、查看状态、休眠/唤醒 |
| DSH 工作区 | 用户 pod | `podIP + launchToken` 就绪、mTLS 完成 token 交换之后 |

等待页返回 200 + `X-DSH-State`（不是 503，避免被企业代理换成错误页）；curl / XHR 等非浏览器请求仍然是
`503 + Retry-After`。完整链路与流程图见 [docs/login-to-service-flow.md](docs/login-to-service-flow.md)，
开通一个新用户的操作步骤见 [docs/add-user.md](docs/add-user.md)。

## 目录

```
01..11 *.yaml      集群侧清单
image/             用户 pod 镜像（Dockerfile + 脚本 + 企业 profile）
plugin/            活动上报插件（零依赖，装进 DSH）
router/            路由服务源码（Go）
control/           控制面源码（Go）
reaper/            空闲回收器（bash + kubectl）
provision.py       开通 CLI（调 control 的 /internal/provision）
verify.sh          验收脚本
```

## 不做的事

- 不改 DSH 源码。`source.actor` 就地归因需要一次核心改动，见 checklist §6.1。
- 不做跨集群调度。用户固定归属一个集群，见 checklist §0。

## 本地验证

macOS + k3d 的准备工作与可验证范围见 [dev/README.md](dev/README.md)，一条命令启动：`./dev/up.sh`。
