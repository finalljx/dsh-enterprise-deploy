## 0. 先设好上下文（这个项目的两个集群）
```sh
kubectl config get-contexts
kubectl config use-context k3d-dsh
# 或按命令指定
kubectl --context k3d-test-k8s ...
```
namespace 简写：`kubectl -n dsh-system ...`

## 1. 排障第一组：为什么起不来
```sh
kubectl -n dsh-users get pods -o wide
kubectl -n dsh-users describe pod dsh-alice-xxx
kubectl -n dsh-users logs deploy/dsh-alice -c dsh --tail=100
kubectl -n dsh-users logs deploy/dsh-alice -c dsh --previous   # 重启前的容器
kubectl -n dsh-system get events --sort-by=.lastTimestamp | tail -20
kubectl -n dsh-system get events --field-selector type=Warning
```
说明 describe 末尾的 Events 段落价值最高。

## 2. 看整个集群
```sh
kubectl get pods -A -o wide
kubectl get all -n dsh-system
kubectl get ns
kubectl top pod -n dsh-users   # 需要 metrics-server，k3s 自带
```

## 3. 本项目资源对照（表）

| 想看什么 | 命令 |
|---|---|
| 控制面 | `kubectl -n dsh-system get deploy,svc,pod -l app=dsh-control` |
| postgres | `kubectl -n dsh-system get sts,pod -l app=dsh-postgres`；进库 `psql` |
| 回收器 | `kubectl -n dsh-system logs deploy/dsh-reaper -f` |
| 用户资源 | `kubectl -n dsh-users get secret,pvc,certificate,deploy` |
| 某人的卷 | `kubectl -n dsh-users get pvc -l dsh.corp/user=alice_corp`（label 是 alice@corp 转成 alice_corp） |
| 证书状态 | `kubectl -n dsh-users get certificate dsh-alice-tls -o yaml`，看 `status.conditions` |
| 用户 pod 注解 | `kubectl -n dsh-users get pod -l dsh.corp/user=alice_corp -o jsonpath='{.items[0].metadata.annotations}'` |

## 4. 编辑/重启/扩缩
```sh
kubectl -n dsh-system rollout restart deploy/dsh-control
kubectl -n dsh-system rollout status deploy/dsh-control --timeout=120s
kubectl -n dsh-users scale deploy/dsh-alice --replicas=0   # 相当于休眠
kubectl -n dsh-system set image deploy/dsh-control control=...
kubectl -n dsh-system set env deploy/dsh-router LOG_LEVEL=debug
kubectl -n dsh-system edit deploy/dsh-control
kubectl apply -f 10-control.yaml --dry-run=server
kubectl diff -f 10-control.yaml
```

## 5. 进容器 / 拷文件 / 端口转发
```sh
kubectl -n dsh-system exec -it deploy/dsh-control -- sh   # distroless 没有 sh！
```
Important: control/router images are distroless static → no shell. Use ephemeral debug container:
```sh
kubectl -n dsh-system debug -it deploy/dsh-control --image=busybox:1.36 --target=control
```
postgres has psql:
```sh
kubectl -n dsh-system exec -it dsh-postgres-0 -- psql -U dsh -d dsh
kubectl -n dsh-system exec -i dsh-postgres-0 -- psql -U dsh -d dsh < control/schema.sql
```
port-forward:
```sh
kubectl -n dsh-system port-forward svc/dsh-control 8080:8080
kubectl -n dsh-system port-forward svc/dsh-postgres 5432:5432
kubectl -n dsh-users port-forward pod/dsh-alice-xxx 8443:8443
```
cp:
```sh
kubectl -n dsh-system cp /tmp/dsh-control deploy/dsh-control:/tmp/... 
```
Note: distroless nonroot can't write /tmp? /tmp exists? distroless static has no /tmp. So `kubectl cp` fails. Use `kubectl debug` or rebuild image. Mention.

## 6. 验证准入策略（本项目特有）
06-admission-policy 两个 VAP：dsh-pod-home-binding / dsh-pvc-owner-label
```sh
kubectl get validatingadmissionpolicy,binding
# 试一个坏 PVC：dry-run=server 会被拒
kubectl -n dsh-users apply --dry-run=server -f- <<'YAML'
...
YAML
```
Give a concrete bad PVC sample without the owner label → expect deny.
Also a good one to make sure not over-blocking.

## 7. 网络排查
```sh
kubectl -n dsh-system run netshoot --rm -it --image=nicolaka/netshoot --restart=Never -- bash
# 里面：curl -s http://dsh-control.dsh-system.svc.cluster.local:8080/healthz
kubectl get networkpolicy -n dsh-users -o yaml
kubectl get svc,endpoints -n dsh-system
```
Note flannel doesn't enforce.

## 8. 本地 k3d
```sh
k3d cluster list
k3d kubeconfig get dsh > dev/render/kubeconfig.yaml
k3d image import -c dsh x:1
docker exec -it k3d-dsh-server-0 sh
k3d cluster delete dsh
```

## 9. JSON/YAML 提取（写脚本用）
```sh
kubectl get pod -o jsonpath=...
kubectl get ... -o custom-columns=...
kubectl get ... -o wide
```
Maybe merge into other sections to keep length manageable.

I'll keep it structured but trimmed. Also mention `-w` watch and `--sort-by`.

Length: this will be fairly long but it's a cheatsheet — acceptable. Aim for scannable.

Should I write it to docs/? Could offer. I'll offer at the end: "要的话我存成 docs/kubectl-cheatsheet.md".

Let me double-check a couple of claims:
1. distroless → no shell: yes, `gcr.io/distroless/static` has no shell, no /tmp, no package manager.
2. reaper deployment logs: `kubectl -n dsh-system logs deploy/dsh-reaper`.
3. postgres service name dsh-postgres at 5432 — yes (09-postgres svc).
4. `kubectl top` — k3s includes metrics-server by default. Yes.
5. VAP is v1 (k8s 1.30+). `kubectl get validatingadmissionpolicy` works on 1.30+; their cluster 1.31.5. Good.
6. `kubectl debug --target` works with distroless (uses ephemeral containers). Yes.
7. For `kubectl -n dsh-users debug` the VAP may reject pods? Ephemeral containers attach to existing pod; VAP validates pod create, not ephemeral update. Fine, but on dsh-user pods the pod has the home-claim annotation so it passes anyway.

Also the alias suggestion: `alias k=kubectl` and `kubectl config set-context --current --namespace=dsh-system`.

Let me write.
按这个项目的实际对象整理，命令都对着 `dsh-system` / `dsh-users` 里的真实名字。

## 0. 上下文

```sh
kubectl config get-contexts                 # 你本机有 k3d-dsh 和 k3d-test-k8s 两个
kubectl config use-context k3d-dsh
kubectl config set-context --current --namespace=dsh-system   # 省掉 -n
alias k=kubectl
```

## 1. 排障三板斧（90% 的时间用这三条）

```sh
kubectl -n dsh-users get pods -o wide            # 先看状态和 IP
kubectl -n dsh-users describe pod dsh-alice-xxx  # 重点看最下面的 Events
kubectl -n dsh-users logs deploy/dsh-alice -c dsh --tail=100
kubectl -n dsh-users logs deploy/dsh-alice -c dsh --previous   # 崩溃重启前的日志
```

`describe` 末尾的 Events 比 `get` 有用得多——`ImagePullBackOff`、`FailedScheduling`、PVC 挂不上、准入策略拒绝，原因都写在那里。

集群级事件：

```sh
kubectl get events -A --sort-by=.lastTimestamp | tail -30
kubectl get events -n dsh-system --field-selector type=Warning
```

## 2. 本项目对象对照

| 想看什么 | 命令 |
|---|---|
| 控制面 | `kubectl -n dsh-system get deploy,svc,pod -l app=dsh-control` |
| Postgres | `kubectl -n dsh-system get sts,pod -l app=dsh-postgres` |
| 回收器 | `kubectl -n dsh-system logs deploy/dsh-reaper -f` |
| 所有用户资源 | `kubectl -n dsh-users get secret,pvc,certificate,deploy` |
| 某个用户的资源 | `kubectl -n dsh-users get all,pvc,secret,certificate -l dsh.corp/user=alice_corp` |
| 证书签发状态 | `kubectl -n dsh-users get certificate dsh-alice-tls` |
| 用户 pod 的归属标记 | `kubectl -n dsh-users get pod -l dsh.corp/user=alice_corp -o jsonpath='{.items[0].metadata.annotations}'` |

注意 label 是 `alice_corp`（`@` 和 `.` 被替换过，见 `labelFor`），不是邮箱原文。

## 3. 改配置、重启、扩缩

```sh
kubectl -n dsh-system rollout restart deploy/dsh-control
kubectl -n dsh-system rollout status  deploy/dsh-control --timeout=120s
kubectl -n dsh-system rollout undo    deploy/dsh-control

kubectl -n dsh-system set env deploy/dsh-router LOG_LEVEL=debug
kubectl -n dsh-system set image deploy/dsh-control control=registry.corp/dsh-control:0.2.0

kubectl -n dsh-users scale deploy/dsh-alice --replicas=0   # 手动休眠
kubectl -n dsh-users scale deploy/dsh-alice --replicas=1   # 手动唤醒

kubectl diff -f 10-control.yaml              # 先看会改什么
kubectl apply -f 10-control.yaml --dry-run=server
```

## 4. 进容器 / 看库 / 端口转发

**注意**：`control` 和 `router` 是 distroless 镜像，没有 shell、没有 `sh`、也没有 `/tmp`，直接 `exec` 和 `cp` 都会失败。用临时调试容器：

```sh
kubectl -n dsh-system debug -it deploy/dsh-control --image=busybox:1.36 --target=control
kubectl -n dsh-system debug -it deploy/dsh-router  --image=nicolaka/netshoot --target=router
```

Postgres 有 psql，可以直接进：

```sh
kubectl -n dsh-system exec -it dsh-postgres-0 -- psql -U dsh -d dsh
kubectl -n dsh-system exec -i dsh-postgres-0 -- psql -U dsh -d dsh < control/schema.sql
kubectl -n dsh-system exec -i dsh-postgres-0 -- psql -U dsh -d dsh -c 'select subject,handle,desired_state from dsh_users;'
```

端口转发（本地调试必备）：

```sh
kubectl -n dsh-system port-forward svc/dsh-control 8080:8080
kubectl -n dsh-system port-forward svc/dsh-postgres 5432:5432
kubectl -n dsh-users  port-forward pod/dsh-alice-xxx 8443:8443
```

## 5. 验证准入策略（本项目特有）

两个 `ValidatingAdmissionPolicy`：`dsh-pod-home-binding`（pod 必须带 home-claim 注解）和 `dsh-pvc-owner-label`（PVC 必须带 owner 标签）。

```sh
kubectl get validatingadmissionpolicy,validatingadmissionpolicybinding
```

造一个缺 owner 标签的 PVC，`--dry-run=server` 会真的过一遍准入，应当被拒：

```sh
kubectl -n dsh-users apply --dry-run=server -f- <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: probe-bad-pvc
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 1Gi } }
YAML
# 期望：Error from server: ... dsh-pvc-owner-label
```

## 6. 网络排查

```sh
kubectl get svc,endpoints -n dsh-system
kubectl get networkpolicy -n dsh-users -o yaml

# 临时 netns 里测连通性
kubectl -n dsh-system run netshoot --rm -it --restart=Never \
  --image=nicolaka/netshoot -- \
  curl -s -m 3 http://dsh-control.dsh-system.svc.cluster.local:8080/healthz
```

提醒：k3d 默认 flannel **不强制** NetworkPolicy，本地看到"通"不代表策略生效，要换 Calico。

## 7. 提取字段（写脚本时）

```sh
kubectl -n dsh-users get pod -o jsonpath='{.items[*].status.podIP}'
kubectl -n dsh-users get deploy -o custom-columns=\
NAME:.metadata.name,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[0].image
kubectl -n dsh-users get pods -w                     # 持续观察
kubectl -n dsh-users get pvc --sort-by=.metadata.creationTimestamp
```

## 8. k3d 相关（本地才有）

```sh
k3d cluster list
k3d kubeconfig get dsh > dev/render/kubeconfig.yaml   # 本机 kubeconfig 写不进 ~/.kube 时用这个
k3d image import -c dsh dsh/dsh:local                 # 绕过 registry 灌镜像
docker exec -it k3d-dsh-server-0 sh                   # 进 k3s 节点（这里才有 shell）
docker exec k3d-dsh-agent-0 crictl -r unix:///run/k3s/containerd/containerd.sock images
k3d cluster delete dsh
```

---

这份是按项目现状整理的。要不要我存成 `docs/kubectl-cheatsheet.md`？如果要，我顺便把 `provision` 开通后的验收命令（`get secret,pvc,certificate,deploy -l dsh.corp/user=`）和日志查询（`jq 'select(.req==...)'`）也并进去，形成一个从"开通"到"排障"的闭环。