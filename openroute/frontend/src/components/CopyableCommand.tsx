/**
 * 可复制的命令展示块（规格书 6.1 要求「带复制按钮与二维码」）。
 *
 * 用于节点一键对接的安装命令、订阅链接等场景。
 */
import { useEffect, useState } from 'react'
import { Button, Input, Space, Spin, Tooltip, message } from 'antd'
import { CopyOutlined, QrcodeOutlined } from '@ant-design/icons'
import QRCode from 'qrcode'

interface Props {
  /** 要展示与复制的命令/链接文本。 */
  command: string
  /** 是否显示二维码按钮。 */
  showQrcode?: boolean
  /** 单行展示还是多行展示。 */
  multiline?: boolean
}

export default function CopyableCommand({
  command,
  showQrcode = true,
  multiline = false,
}: Props) {
  const [showQr, setShowQr] = useState(false)

  const copy = async () => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(command)
      } else {
        // 兜底：非安全上下文（http 访问）下 clipboard API 不可用。
        fallbackCopy(command)
      }
      message.success('已复制到剪贴板')
    } catch {
      fallbackCopy(command)
      message.success('已复制到剪贴板')
    }
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={8}>
      <Space.Compact style={{ width: '100%' }}>
        <Input.TextArea
          value={command}
          readOnly
          autoSize={multiline ? { minRows: 3, maxRows: 12 } : { minRows: 1, maxRows: 1 }}
          className="or-mono"
          style={{ resize: 'none' }}
        />
        <Tooltip title="复制">
          <Button
            icon={<CopyOutlined />}
            onClick={copy}
            style={multiline ? { height: 'auto' } : undefined}
          />
        </Tooltip>
        {showQrcode && (
          <Tooltip title={showQr ? '隐藏二维码' : '显示二维码'}>
            <Button
              icon={<QrcodeOutlined />}
              onClick={() => setShowQr((v) => !v)}
              style={multiline ? { height: 'auto' } : undefined}
            />
          </Tooltip>
        )}
      </Space.Compact>

      {showQr && (
        <div style={{ textAlign: 'center', padding: 12, background: '#fff', borderRadius: 6 }}>
          <QRCodeImage text={command} size={168} />
        </div>
      )}
    </Space>
  )
}

/** 把文本渲染为可扫描的二维码。
 *
 *  输出 data URL 而非 HTML 字符串：安装命令里含有 `#`、`&` 等字符，
 *  经危险 HTML 注入路径容易出问题，用图片源则完全规避了转义问题。
 */
function QRCodeImage({ text, size }: { text: string; size: number }) {
  const [dataUrl, setDataUrl] = useState('')
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let cancelled = false
    QRCode.toDataURL(text, {
      margin: 1,
      width: size,
      errorCorrectionLevel: 'M',
      color: { dark: '#1f2329', light: '#ffffff' },
    })
      .then((out) => {
        if (!cancelled) {
          setDataUrl(out)
          setFailed(false)
        }
      })
      .catch(() => {
        if (!cancelled) setFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [text, size])

  if (failed) {
    return <span style={{ color: 'var(--or-text-tertiary)' }}>二维码生成失败</span>
  }

  if (!dataUrl) {
    return <Spin size="small" />
  }

  return (
    <img
      src={dataUrl}
      width={size}
      height={size}
      alt="安装命令二维码"
      style={{ display: 'block', margin: '0 auto' }}
    />
  )
}

/** 非安全上下文下的复制兜底方案。 */
function fallbackCopy(text: string): void {
  const el = document.createElement('textarea')
  el.value = text
  el.style.position = 'fixed'
  el.style.opacity = '0'
  document.body.appendChild(el)
  el.select()
  try {
    document.execCommand('copy')
  } catch {
    // 忽略：极端环境下复制失败，用户仍可手动选中文本。
  }
  document.body.removeChild(el)
}
