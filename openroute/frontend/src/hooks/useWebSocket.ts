/**
 * 实时推送 Hook（规格书 9.3：节点状态用 WebSocket）。
 *
 * 用原生 WebSocket 而非第三方库：本项目只需要「接收事件后触发回调」，
 * 不支持自动重连的库反而要写更多代码。
 *
 * 工程要点：
 *  - 指数退避重连（1s → 2s → 4s … 上限 30s），避免面板重启时把服务打满；
 *  - 卸载时主动关闭，且不再重连；
 *  - 支持 HMR/依赖变化时安全地重连。
 */
import { useEffect, useRef, useState, useCallback } from 'react'

import { getAccessToken } from '../api/client'

/** 后端推送的事件结构。 */
export interface WsEvent {
  type:
    | 'config_changed'
    | 'node_online'
    | 'node_offline'
    | 'node_metrics'
    | 'rule_sync'
    | 'alert_fired'
    | 'alert_resolved'
  data: unknown
}

interface Options {
  /** 事件回调；用 ref 保存，变化不会触发重连。 */
  onEvent?: (ev: WsEvent) => void
  /** 是否启用。为 false 时不建立连接（例如未登录）。 */
  enabled?: boolean
  /** 是否输出调试日志。 */
  debug?: boolean
}

type Status = 'connecting' | 'open' | 'closed'

/** 最大重连间隔。 */
const MAX_BACKOFF = 30000

/**
 * 建立到面板的 WebSocket 连接。
 *
 * 返回连接状态与手动重连函数。
 */
export function useWebSocket(path: string, options: Options = {}) {
  const { enabled = true, debug = false } = options

  const [status, setStatus] = useState<Status>('closed')

  const wsRef = useRef<WebSocket | null>(null)
  const retryRef = useRef(0)
  const timerRef = useRef<number | null>(null)
  // 标记是否应该保持连接。卸载后置 false，防止重连。
  const shouldConnect = useRef(enabled)
  const onEventRef = useRef(options.onEvent)
  onEventRef.current = options.onEvent

  const log = useCallback(
    (...args: unknown[]) => {
      if (debug) console.log('[ws]', ...args)
    },
    [debug],
  )

  const connect = useCallback(() => {
    if (!shouldConnect.current) return
    if (wsRef.current && wsRef.current.readyState <= WebSocket.OPEN) return

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    // 令牌通过查询参数传递：浏览器 WebSocket API 不支持自定义请求头。
    const token = getAccessToken()
    const url = `${proto}//${window.location.host}${path}${token ? `?token=${encodeURIComponent(token)}` : ''}`

    log('连接', url)
    setStatus('connecting')

    let ws: WebSocket
    try {
      ws = new WebSocket(url)
    } catch (e) {
      log('创建连接失败', e)
      scheduleReconnect()
      return
    }
    wsRef.current = ws

    ws.onopen = () => {
      log('已连接')
      retryRef.current = 0
      setStatus('open')
    }

    ws.onmessage = (ev) => {
      try {
        const parsed = JSON.parse(ev.data as string) as WsEvent
        onEventRef.current?.(parsed)
      } catch {
        // 非 JSON 消息直接忽略（例如服务端的 ping 文本）。
      }
    }

    ws.onerror = () => {
      log('连接出错')
    }

    ws.onclose = (ev) => {
      log('连接关闭', ev.code, ev.reason)
      setStatus('closed')
      wsRef.current = null
      // 正常关闭（1000）且是我们主动断开时不再重连。
      if (shouldConnect.current) {
        scheduleReconnect()
      }
    }

    function scheduleReconnect() {
      if (!shouldConnect.current) return
      // 指数退避：1s, 2s, 4s, 8s, 16s, 30s 封顶。
      const delay = Math.min(1000 * 2 ** retryRef.current, MAX_BACKOFF)
      retryRef.current += 1
      log(`将在 ${delay}ms 后重连（第 ${retryRef.current} 次）`)
      if (timerRef.current) window.clearTimeout(timerRef.current)
      timerRef.current = window.setTimeout(() => {
        timerRef.current = null
        connect()
      }, delay)
    }
  }, [path, log])

  const disconnect = useCallback(() => {
    shouldConnect.current = false
    if (timerRef.current) {
      window.clearTimeout(timerRef.current)
      timerRef.current = null
    }
    const ws = wsRef.current
    wsRef.current = null
    if (ws) {
      // 先摘掉 onclose，避免它触发重连逻辑。
      ws.onclose = null
      ws.close(1000, 'client closing')
    }
    setStatus('closed')
  }, [])

  const reconnect = useCallback(() => {
    disconnect()
    retryRef.current = 0
    shouldConnect.current = true
    connect()
  }, [connect, disconnect])

  useEffect(() => {
    shouldConnect.current = enabled
    if (enabled) {
      connect()
    } else {
      disconnect()
    }
    return () => {
      disconnect()
    }
  }, [enabled, connect, disconnect])

  return { status, reconnect }
}
