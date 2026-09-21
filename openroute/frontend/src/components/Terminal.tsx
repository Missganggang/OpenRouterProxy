/**
 * WebSSH 终端（规格书 6.1 / 9.1）。
 *
 * 与面板的交互协议（见 internal/api/handler_ws.go 的 NodeTerminal）：
 *   浏览器 → 面板：{"type":"input","data":"..."} / {"type":"resize","cols":N,"rows":N} / {"type":"ping"}
 *   面板 → 浏览器：{"type":"output","data":"..."} / {"type":"error","message":"..."} / {"type":"ready"|"pong"}
 *
 * 工程要点：
 *  - 终端实例在 useEffect 内创建、在清理函数里 dispose，因此 React 18 StrictMode
 *    的「挂载 → 卸载 → 再挂载」不会留下两个实例（刻意不用模块级单例）；
 *  - 窗口尺寸同时受两条链路影响：xterm 自身的 onResize（字号变化等）与容器尺寸变化
 *    （ResizeObserver + window resize），两者都会回填尺寸给节点；
 *  - 断开与错误都要给出「人话」而不是一块黑屏：状态栏 + 提示 + 重连按钮。
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Space, Tag, Tooltip, Typography } from 'antd'
import { CloseOutlined, ReloadOutlined } from '@ant-design/icons'
import { Terminal as XTerm } from 'xterm'
import { FitAddon } from 'xterm-addon-fit'

import { getAccessToken } from '../api/client'
import { useI18n } from '../locales'

interface Props {
  /** 节点 ID，对应后端 /api/v1/nodes/:id/terminal。 */
  nodeId: number
  /** 关闭终端的回调（由父组件决定是否隐藏整个面板）。 */
  onClose?: () => void
  /** 终端容器高度，默认沿用 .or-terminal-wrap 的 520px。 */
  height?: number
}

/** 面板下发的消息。 */
interface TerminalServerMessage {
  type?: 'output' | 'error' | 'ready' | 'pong' | string
  data?: string
  message?: string
}

/** 连接状态机。 */
type ConnStatus = 'connecting' | 'open' | 'closed' | 'dead'

/** 保持连接的心跳间隔：后端读超时是 60 秒，20 秒足够留出抖动余量。 */
const PING_INTERVAL = 20000

/** 建连超时：反代吞掉 Upgrade 时浏览器不会回调 onerror，只能自己兜底。 */
const CONNECT_TIMEOUT = 15000

/** 尺寸回填的防抖时间：拖拽窗口时会触发几十次回调，不防抖会打满长连接。 */
const RESIZE_DEBOUNCE = 120

export default function Terminal({ nodeId, onClose, height = 520 }: Props) {
  const { t } = useI18n()

  const containerRef = useRef<HTMLDivElement | null>(null)
  const termRef = useRef<XTerm | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  /** 用 ref 持有发送函数：onData 回调在 useEffect 外注册，拿不到闭包里的局部变量。 */
  const sendRef = useRef<(payload: Record<string, unknown>) => void>(() => {})

  const [status, setStatus] = useState<ConnStatus>('connecting')
  const [errorMsg, setErrorMsg] = useState('')
  /** 自增即触发重连：把「重连」做成状态而非命令式调用，避免与卸载清理竞争。 */
  const [attempt, setAttempt] = useState(0)

  /** 把终端尺寸同步给节点（规格书 6.1：必须支持窗口尺寸同步）。 */
  const sendResize = useCallback(() => {
    const term = termRef.current
    if (!term) return
    sendRef.current({ type: 'resize', cols: term.cols, rows: term.rows })
  }, [])

  /** 重新适配容器尺寸并回填给节点。 */
  const refit = useCallback(() => {
    const fit = fitRef.current
    if (!fit) return
    try {
      fit.fit()
    } catch {
      // 容器尚未布局完成（display:none / 0 尺寸）时 fit 会抛错，忽略即可。
    }
    sendResize()
  }, [sendResize])

  // ① 创建终端：只在挂载时执行一次，卸载时销毁。
  useEffect(() => {
    const el = containerRef.current
    if (!el) return

    const term = new XTerm({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      scrollback: 5000,
      convertEol: true,
      theme: {
        background: '#1e1e1e',
        foreground: '#d7dbe0',
        cursor: '#d7dbe0',
        selectionBackground: '#334155',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(el)
    termRef.current = term
    fitRef.current = fit
    refit()

    return () => {
      // 先断开数据回调，再销毁实例，避免 dispose 过程中仍然触发 send。
      termRef.current = null
      fitRef.current = null
      term.dispose()
    }
  }, [refit])

  // ② 建立 WebSocket 连接；节点 ID 或 attempt 变化时重连。
  useEffect(() => {
    setStatus('connecting')
    setErrorMsg('')

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    // 浏览器 WebSocket API 不支持自定义请求头，令牌走查询参数（与 useWebSocket 一致）。
    const token = getAccessToken()
    const url = `${proto}//${window.location.host}/api/v1/nodes/${nodeId}/terminal${
      token ? `?token=${encodeURIComponent(token)}` : ''
    }`

    let ws: WebSocket
    try {
      ws = new WebSocket(url)
    } catch {
      setStatus('dead')
      setErrorMsg(t('terminal.connectFailed'))
      return
    }
    wsRef.current = ws

    let closed = false
    let established = false
    let pingTimer: number | null = null

    const clearTimers = () => {
      if (pingTimer !== null) {
        window.clearInterval(pingTimer)
        pingTimer = null
      }
    }

    const connectTimer = window.setTimeout(() => {
      if (established || closed) return
      setStatus('dead')
      setErrorMsg(t('terminal.connectTimeout'))
    }, CONNECT_TIMEOUT)

    /** 只有 open 状态才允许发送，否则忽略（重连间隙用户敲键盘是常态）。 */
    const send = (payload: Record<string, unknown>) => {
      if (ws.readyState !== WebSocket.OPEN) return
      try {
        ws.send(JSON.stringify(payload))
      } catch {
        // 发送失败通常意味着连接正在关闭，交给 onclose 处理。
      }
    }
    sendRef.current = send

    /** 把用户输入转成 input 帧；输入本身代表会话活着，顺手清掉上一次的断线提示。 */
    const dataSub = termRef.current?.onData((data: string) => {
      send({ type: 'input', data })
      setErrorMsg('')
    })

    /** xterm 自身尺寸变化（例如字体调整、内容回流）时同步给节点。 */
    const resizeSub = termRef.current?.onResize(() => sendResize())

    ws.onopen = () => {
      established = true
      window.clearTimeout(connectTimer)
      setStatus('open')
      setErrorMsg('')
      // 连接建立后立刻把当前尺寸报给节点，避免首屏按 80x24 渲染。
      refit()
      pingTimer = window.setInterval(() => send({ type: 'ping' }), PING_INTERVAL)
    }

    ws.onmessage = (ev: MessageEvent<string>) => {
      let msg: TerminalServerMessage
      try {
        msg = JSON.parse(ev.data) as TerminalServerMessage
      } catch {
        // 非 JSON 帧（例如反代的探测文本）直接当作输出展示，不做丢弃。
        termRef.current?.write(ev.data)
        return
      }

      switch (msg.type) {
        case 'output':
          if (msg.data) termRef.current?.write(msg.data)
          break
        case 'ready':
          termRef.current?.write(`\r\n\x1b[32m${t('terminal.banner')}\x1b[0m\r\n`)
          break
        case 'pong':
          // 心跳应答：连接仍然存活。
          break
        case 'error':
        default:
          if (msg.type === 'error') {
            // 后端在「节点离线 / 无长连接 / WebSSH 关闭」时通过 error 帧说明原因。
            setStatus('dead')
            setErrorMsg(msg.message || t('terminal.closed'))
            termRef.current?.write(`\r\n\x1b[31m${msg.message || t('terminal.closed')}\x1b[0m\r\n`)
            // 服务端在写完错误帧后即关闭连接，这里主动收尾避免半开连接。
            try {
              ws.close(1000, 'server error frame')
            } catch {
              // 忽略：连接可能已在关闭流程中。
            }
          }
          break
      }
    }

    ws.onerror = () => {
      // 403（WebSSH 被关闭 / 缺少 node:exec 权限）与网络错误都会走到这里。
      // 浏览器不暴露握手状态码，因此只能给出一句可操作的整体提示。
      if (!established) {
        setStatus('dead')
        setErrorMsg(t('terminal.handshakeFailed'))
      }
    }

    ws.onclose = () => {
      if (closed) return
      window.clearTimeout(connectTimer)
      clearTimers()
      wsRef.current = null
      // error 帧已经给出更具体的原因时不要覆盖它。
      setStatus((prev) => (prev === 'dead' ? prev : 'closed'))
    }

    return () => {
      closed = true
      window.clearTimeout(connectTimer)
      clearTimers()
      dataSub?.dispose()
      resizeSub?.dispose()
      // 摘掉 onclose，避免清理阶段的关闭触发一次多余的状态更新。
      ws.onclose = null
      ws.onmessage = null
      ws.onerror = null
      ws.onopen = null
      wsRef.current = null
      sendRef.current = () => {}
      try {
        ws.close(1000, 'client closing')
      } catch {
        // 忽略：连接可能已经关闭。
      }
    }
  }, [nodeId, attempt, refit, sendResize, t])

  // ③ 容器尺寸变化时重新适配：窗口缩放、侧边栏折叠、Tab 切换都会触发。
  useEffect(() => {
    const el = containerRef.current
    if (!el) return

    let timer: number | null = null
    const schedule = () => {
      if (timer !== null) window.clearTimeout(timer)
      timer = window.setTimeout(() => {
        timer = null
        refit()
      }, RESIZE_DEBOUNCE)
    }

    window.addEventListener('resize', schedule)
    // jsdom 与部分老浏览器没有 ResizeObserver，做存在性判断而不是直接使用。
    const observer = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(schedule) : null
    observer?.observe(el)

    return () => {
      if (timer !== null) window.clearTimeout(timer)
      window.removeEventListener('resize', schedule)
      observer?.disconnect()
    }
  }, [refit])

  /** 重连：重建 socket，但保留终端里的历史输出，便于对照上一次会话。 */
  const reconnect = useCallback(() => {
    setAttempt((n) => n + 1)
  }, [])

  const statusColor: Record<ConnStatus, string> = {
    connecting: 'processing',
    open: 'success',
    closed: 'warning',
    dead: 'error',
  }

  const statusText: Record<ConnStatus, string> = {
    connecting: t('terminal.connecting'),
    open: t('terminal.connected'),
    closed: t('terminal.closed'),
    dead: t('terminal.connectFailed'),
  }

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} wrap>
        <Space size={8} wrap>
          <Tag color={statusColor[status]} style={{ margin: 0 }}>
            {statusText[status]}
          </Tag>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            <span className="or-mono">node #{nodeId}</span>
          </Typography.Text>
          <Tooltip title={t('terminal.auditHint')}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('terminal.auditHint')}
            </Typography.Text>
          </Tooltip>
        </Space>
        <Space size={8}>
          {status !== 'open' && (
            <Button size="small" icon={<ReloadOutlined />} onClick={reconnect}>
              {t('terminal.reconnect')}
            </Button>
          )}
          {onClose && (
            <Button size="small" icon={<CloseOutlined />} onClick={onClose}>
              {t('terminal.close')}
            </Button>
          )}
        </Space>
      </Space>

      {(status === 'closed' || status === 'dead') && (
        <Alert
          type={status === 'dead' ? 'error' : 'warning'}
          showIcon
          message={errorMsg || t('terminal.reconnectHint')}
          action={
            <Button size="small" type="primary" ghost onClick={reconnect}>
              {t('terminal.reconnect')}
            </Button>
          }
        />
      )}

      <div className="or-terminal-wrap" style={{ height }}>
        <div ref={containerRef} className="or-terminal" />
      </div>

      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {t('terminal.tips')}
      </Typography.Text>
    </Space>
  )
}
