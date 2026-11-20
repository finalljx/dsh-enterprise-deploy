/**
 * 活动上报插件：把会话的 turn 状态报给企业控制面，供空闲回收使用。
 *
 * 为什么需要它：回收的唯一硬性守卫是「没有 open turn」。从 pod 外部看不到这个
 * 状态；看 HTTP 连接会把「浏览器关了但 turn 还在跑」误判为空闲。
 *
 * 零依赖：不 import 任何包，只读环境变量。这样它在任何 DSH 安装里都能加载，
 * 也不会因为 peer 依赖解析失败而启动不了。
 *
 * 环境变量：
 *   DSH_USER_ID     调用方标识（pod 里由 downward API 注入）
 *   CONTROL_URL     控制面地址
 *   CONTROL_TOKEN   上报用令牌
 *   ACTIVITY_INTERVAL_MS  心跳间隔，默认 15000
 *
 * @module @corp/dsh-plugin-activity
 */

/** Cordis 插件名。 */
export const name = 'activity-reporter'

const DEFAULT_INTERVAL_MS = 15000

/**
 * @param {object} ctx - Cordis 上下文。
 * @param {object} config - 插件配置（本插件不使用，全部走环境变量）。
 * @returns {void}
 */
export function apply(ctx, config) {
  void config

  const subject = process.env.DSH_USER_ID
  const controlUrl = process.env.CONTROL_URL
  const token = process.env.CONTROL_TOKEN

  if (!subject || !controlUrl || !token) {
    ctx.logger?.('activity-reporter: DSH_USER_ID / CONTROL_URL / CONTROL_TOKEN 缺失，插件停用')
    return
  }

  const parsed = Number(process.env.ACTIVITY_INTERVAL_MS ?? DEFAULT_INTERVAL_MS)
  const intervalMs = Number.isFinite(parsed) && parsed >= 1000 ? parsed : DEFAULT_INTERVAL_MS

  // control 侧用秒（time.Now().Unix()）比较空闲时间；这里必须同单位，
  // 传毫秒会让 last_activity_at 变成 1.7e12，空闲判断永远为假（用户永不休眠）。
  const nowSeconds = () => Math.floor(Date.now() / 1000)

  let openTurn = false
  let lastActivityAt = nowSeconds()
  let disposed = false

  async function report() {
    if (disposed) return
    try {
      await fetch(`${controlUrl}/internal/activity`, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ subject, openTurn, lastActivityAt }),
        signal: AbortSignal.timeout(3000),
      })
    } catch (error) {
      // 上报失败不影响主流程：下一轮心跳会补上
      ctx.logger?.(`activity-reporter: report failed: ${String(error)}`)
    }
  }

  ctx.on('session/event', (_session, event) => {
    lastActivityAt = nowSeconds()
    if (event.type === 'turn/start') {
      openTurn = true
      void report()
    } else if (event.type === 'turn/end') {
      openTurn = false
      void report()
    }
  })

  const timer = setInterval(() => { void report() }, intervalMs)

  ctx.effect(() => () => {
    disposed = true
    clearInterval(timer)
  }, 'activity-reporter: heartbeat')

  void report()
}
