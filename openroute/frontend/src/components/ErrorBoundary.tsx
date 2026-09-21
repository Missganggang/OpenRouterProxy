/**
 * 渲染错误边界。
 *
 * 存在的意义：React 在渲染阶段抛出异常时会卸载整棵树，
 * 页面就变成一片空白（本次部署正是踩到这个坑——仪表盘误把对象当数字渲染，
 * 首屏闪一下就全白，且控制台之外没有任何提示）。
 *
 * 这里把错误兜住，展示可读的错误信息与堆栈，
 * 让问题「可见、可复制、可排查」，而不是静默白屏。
 */
import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Button, Result, Typography, Space } from 'antd'

const { Paragraph, Text } = Typography

interface Props {
  children: ReactNode
  /** 出错时的附加说明，便于定位是哪个模块。 */
  title?: string
}

interface State {
  error: Error | null
  info: ErrorInfo | null
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, info: null }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // 同时打到控制台，方便用户直接截图报障。
    console.error('[OpenRoute] 页面渲染出错', error, info.componentStack)
    this.setState({ info })
  }

  private reset = (): void => {
    this.setState({ error: null, info: null })
  }

  private reload = (): void => {
    window.location.reload()
  }

  render(): ReactNode {
    const { error, info } = this.state
    if (!error) {
      return this.props.children
    }

    return (
      <div style={{ padding: 24 }}>
        <Result
          status="error"
          title={this.props.title ?? '页面渲染出错'}
          subTitle="界面已停止渲染以避免更严重的问题。下面的信息可以直接复制用于排查。"
          extra={
            <Space>
              <Button type="primary" onClick={this.reload}>
                重新加载页面
              </Button>
              <Button onClick={this.reset}>尝试恢复</Button>
            </Space>
          }
        >
          <Paragraph>
            <Text strong>错误信息：</Text>
          </Paragraph>
          <Paragraph>
            <Text code style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
              {error.message || String(error)}
            </Text>
          </Paragraph>
          {info?.componentStack && (
            <>
              <Paragraph style={{ marginTop: 16 }}>
                <Text strong>组件调用栈：</Text>
              </Paragraph>
              <Paragraph>
                <Text
                  code
                  style={{
                    whiteSpace: 'pre-wrap',
                    fontSize: 12,
                    display: 'block',
                    maxHeight: 260,
                    overflow: 'auto',
                  }}
                >
                  {info.componentStack.trim()}
                </Text>
              </Paragraph>
            </>
          )}
          <Paragraph type="secondary" style={{ fontSize: 12 }}>
            如果是刚升级完出现的问题，可按 Ctrl+Shift+R 强制刷新以清掉旧缓存。
          </Paragraph>
        </Result>
      </div>
    )
  }
}
