# 用户 pod 镜像

## 构建

```sh
docker build -t registry.corp/dsh:0.2.1-alpha.4 ./image
docker push registry.corp/dsh:0.2.1-alpha.4
```

## 启动时发生什么

1. `init-home.sh`（initContainer）：空卷写 `.owner.json`；有数据无标记则拒绝启动
2. `entrypoint.sh`（主容器）：
   - 交叉校验 `DSH_USER_ID`（来自 Pod 标签）与卷上 `.owner.json`
   - 覆盖 `$DSH_HOME/profiles/web/cordis.patch.yml`
   - `dsh web --host $POD_IP --port 8443 --tls-cert ... --no-open`
   - 从 stdout 解析 launch token，`POST /internal/register` 给控制面
3. router 首次代理时用该 token 换 DSH cookie，之后自己持有

## 为什么 launch token 要上报

DSH 的浏览器认证是「进程令牌换 cookie」，没有关闭开关。token 随进程变化，
所以每次启动都要让 router 知道新的。见 checklist §5.3。

## 已知限制

- 采用「router 持有 DSH cookie」的方式（方案 B）。方案 C（受信前置认证 +
  `source.actor`）需要一次 DSH 核心改动，落地后可去掉 token 上报与 cookie 代理。
- `plugin-manager` 被禁用后，用户无法自助安装插件，符合平台统一发布策略。
