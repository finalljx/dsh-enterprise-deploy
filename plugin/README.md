# @corp/dsh-plugin-activity

把会话的 turn 状态上报给企业控制面，供空闲回收使用。

## 为什么不能靠 HTTP 连接或 CPU

| 信号 | 反例 |
|---|---|
| HTTP 连接数 | 浏览器关了，turn 还在跑 → 误杀；后台标签页挂一天 → 永不回收 |
| CPU 使用率 | agent 等模型返回时 CPU 为 0，但 turn 开着 → 误杀 |
| 日志 mtime | 一次 20 分钟的测试执行期间日志不增长 → 误判空闲 |

只有 `turn/start` / `turn/end` 能准确回答「有没有 open turn」。

## 安装

```sh
dsh plugin --profile web add @corp/dsh-plugin-activity@1.0.0
```

装进 profile 后，在 `$DSH_HOME/profiles/web/cordis.patch.yml` 加一行：

```yaml
- insert:
    - id: activity-reporter
      name: '@corp/dsh-plugin-activity'
```

本仓库的 `image/profile/cordis.patch.yml` 已包含这一行。

## 环境变量

| 变量 | 必需 | 说明 |
|---|---|---|
| `DSH_USER_ID` | 是 | 调用方标识，pod 里由 downward API 从标签注入 |
| `CONTROL_URL` | 是 | 控制面地址 |
| `CONTROL_TOKEN` | 是 | 上报令牌 |
| `ACTIVITY_INTERVAL_MS` | 否 | 心跳间隔，默认 15000 |

缺任一必需项时插件自动停用并记一条日志，不会让宿主启动失败。

## 行为

- 每次 `turn/start` / `turn/end` 立即上报一次
- 其余时间每 `ACTIVITY_INTERVAL_MS` 心跳一次，刷新 `lastActivityAt`
- 上报失败只记日志，下一轮重试；不影响会话
- 插件卸载时停止心跳

## 为什么零依赖

不 import 任何包，只读 `process.env` 和全局 `fetch`。这样它在任何 DSH 安装里都能加载，
不会因为 peer 依赖（`@deepseek-ai/cordis`、`@deepseek-ai/schemastery`）解析失败而启动不了。
配置全部走环境变量，代价是不能从 cordis.patch.yml 调参——对固定部署的企业环境是可接受的。
