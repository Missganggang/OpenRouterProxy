/**
 * 轮询 Hook（规格书 9.3：流量曲线默认 30 秒轮询，可切换 10 秒）。
 *
 * 关键设计：
 *  - 页面不可见时暂停轮询，避免后台标签页持续打接口；
 *  - 组件卸载或依赖变化时清理定时器，防止重复轮询；
 *  - 上一次请求未结束时跳过本轮，避免请求堆积。
 */
import { useEffect, useRef, useState, useCallback } from 'react'

interface Options {
  /** 轮询间隔（毫秒）。传 0 表示不轮询。 */
  interval?: number
  /** 是否在挂载后立即执行一次。 */
  immediate?: boolean
  /** 页面隐藏时是否暂停。 */
  pauseWhenHidden?: boolean
}

/**
 * 周期性执行一个异步函数。
 *
 * 返回手动触发函数，便于「立即刷新」按钮复用同一套 loading 状态。
 */
export function usePolling<T>(
  fn: () => Promise<T>,
  options: Options = {},
): {
  data: T | undefined
  loading: boolean
  error: unknown
  refresh: () => Promise<void>
} {
  const { interval = 30000, immediate = true, pauseWhenHidden = true } = options

  const [data, setData] = useState<T>()
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<unknown>()

  // 用 ref 保存最新的 fn，避免把它放进 useEffect 依赖导致重复建定时器。
  const fnRef = useRef(fn)
  fnRef.current = fn

  // 标记上一次请求是否仍在进行。
  const inFlight = useRef(false)
  // 组件是否已卸载，防止卸载后 setState 警告。
  const mounted = useRef(true)

  const run = useCallback(async () => {
    if (inFlight.current) return
    inFlight.current = true
    if (mounted.current) setLoading(true)
    try {
      const result = await fnRef.current()
      if (mounted.current) {
        setData(result)
        setError(undefined)
      }
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      inFlight.current = false
      if (mounted.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    mounted.current = true

    if (immediate) {
      void run()
    }

    if (interval <= 0) {
      return () => {
        mounted.current = false
      }
    }

    const timer = window.setInterval(() => {
      // 页面隐藏时跳过本轮，减少无意义的网络开销。
      if (pauseWhenHidden && document.hidden) return
      void run()
    }, interval)

    // 页面重新可见时立即补一次，避免用户看到过期数据。
    const onVisible = () => {
      if (!document.hidden) void run()
    }
    if (pauseWhenHidden) {
      document.addEventListener('visibilitychange', onVisible)
    }

    return () => {
      mounted.current = false
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [interval, immediate, pauseWhenHidden, run])

  return { data, loading, error, refresh: run }
}
