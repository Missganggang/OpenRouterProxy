/**
 * 基于 textarea 的 JSON 编辑器（规格书 9.1、6.4）。
 *
 * 用于规则的 `options` 字段等自由 JSON 场景：不做树形编辑，
 * 只保证「输入即校验」——合法性徽标 + 带行号的错误提示，
 * 让用户能自己定位到出问题的那一行。
 */
import { useEffect, useMemo, useState } from 'react'
import { Input, Space, Tag, Typography } from 'antd'

import { useI18n } from '../locales'

const { Text } = Typography

interface Props {
  /** 当前文本值。 */
  value?: string
  /** 文本变化回调。 */
  onChange?: (value: string) => void
  /** 文本域高度。 */
  rows?: number
  /** 占位提示。 */
  placeholder?: string
  /** 只读模式（仅展示与校验）。 */
  readOnly?: boolean
}

/** 一条 JSON 校验结果。 */
interface JsonCheck {
  /** 空文本视为合法（多数场景下 options 允许为空）。 */
  valid: boolean
  /** 解析出的值，非法时为 undefined。 */
  parsed?: unknown
  /** 面向用户的错误描述。 */
  error?: string
  /** 出错的行号（从 1 开始）。 */
  line?: number
}

/**
 * 解析并校验 JSON 文本。
 *
 * `JSON.parse` 的错误消息在不同引擎里行号位置不一致：
 * V8 形如 `... at position 34 (line 3 column 5)`，也有 `at line 3 column 5` 的写法。
 * 用两个正则从消息里抠出行号与列号，抠不到时退化为行内字符位置换算。
 */
function checkJson(text: string): JsonCheck {
  const trimmed = text.trim()
  if (!trimmed) return { valid: true }

  try {
    return { valid: true, parsed: JSON.parse(trimmed) }
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    const posMatch = /position\s+(\d+)/i.exec(msg)
    const lineMatch = /line\s+(\d+)/i.exec(msg)
    const colMatch = /column\s+(\d+)/i.exec(msg)

    let line: number | undefined
    if (lineMatch?.[1]) {
      line = Number(lineMatch[1])
    } else if (posMatch?.[1]) {
      // 没有显式行号时，用字符偏移量数出所在行。
      const pos = Math.min(Number(posMatch[1]), text.length)
      line = text.slice(0, pos).split('\n').length
    }

    const parts = [msg.replace(/^JSON\.parse:\s*/i, '')]
    if (line !== undefined) parts.push(`第 ${line} 行`)
    if (colMatch?.[1]) parts.push(`第 ${colMatch[1]} 列`)

    return { valid: false, error: parts.join(' · '), line }
  }
}

export default function JsonEditor({
  value,
  onChange,
  rows = 6,
  placeholder,
  readOnly = false,
}: Props) {
  const { t } = useI18n()
  const [text, setText] = useState(value ?? '')

  // 受控同步：外部传入的值变化时覆盖内部文本（例如切换编辑对象）。
  useEffect(() => {
    setText(value ?? '')
  }, [value])

  const check = useMemo(() => checkJson(text), [text])
  const empty = text.trim().length === 0

  const change = (next: string) => {
    setText(next)
    onChange?.(next)
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={4}>
      <Input.TextArea
        className="or-mono"
        rows={rows}
        value={text}
        readOnly={readOnly}
        placeholder={placeholder ?? '{}'}
        onChange={(e) => change(e.target.value)}
        spellCheck={false}
      />

      <Space size={8} align="center" wrap>
        {/* 徽标：仅在用户动过输入之后才提示「非法」，避免打开即报红。 */}
        {empty ? (
          <Tag style={{ margin: 0 }}>{t('json.empty')}</Tag>
        ) : check.valid ? (
          <Tag color="success" style={{ margin: 0 }}>
            {t('json.valid')}
          </Tag>
        ) : (
          <Tag color="error" style={{ margin: 0 }}>
            {t('json.invalid')}
          </Tag>
        )}

        {!empty && !check.valid && (
          <Text type="danger" style={{ fontSize: 12 }}>
            {check.error}
          </Text>
        )}

        {!empty && check.valid && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('json.parsedOk')}
          </Text>
        )}
      </Space>
    </Space>
  )
}
