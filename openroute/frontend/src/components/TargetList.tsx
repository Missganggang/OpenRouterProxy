/**
 * 多目标地址编辑器（规格书 6.3「多目标地址负载均衡」、11.8）。
 *
 * 一行 = 一个目标 `{host, port, weight, status}`：
 *  - 拖拽手柄调整顺序，顺序即目标优先级（主备/故障转移时生效）
 *  - 权重用滑杆 + 数字输入联动，`weighted` 策略下参与加权
 *  - 状态开关控制目标是否纳入分发池（down = 摘除）
 *
 * 拖拽刻意用原生 HTML5 drag 事件实现，不引入 dnd 类依赖。
 *
 * 状态管理：内部持有「行」数组作为渲染真源（带稳定 key），
 * 只在外部值真的变化时回灌。若每轮渲染都从 props 重建行，
 * key 会不断变化，输入框会在每次按键后被卸载重建，焦点丢失。
 */
import { useEffect, useRef, useState } from 'react'
import { Button, Input, InputNumber, Slider, Space, Tooltip, Typography } from 'antd'
import {
  DeleteOutlined,
  HolderOutlined,
  PushpinOutlined,
  PlusOutlined,
} from '@ant-design/icons'

import type { Target } from '../api/types'
import { useI18n } from '../locales'

const { Text } = Typography

interface Props {
  value?: Target[]
  onChange?: (targets: Target[]) => void
  /** 是否禁用全部交互。 */
  disabled?: boolean
  /** 是否显示权重列（`failover` 策略下权重无意义，可隐藏）。 */
  showWeight?: boolean
  /** 权重上限，用于滑杆刻度。 */
  maxWeight?: number
  /** 到达该行数后禁用「添加」按钮。 */
  maxRows?: number
}

/** 单个目标行的编辑态：port 允许暂时为空（输入过程中）。 */
interface Row {
  key: string
  host: string
  port: number | null
  weight: number
  status: 'up' | 'down'
}

let rowSeq = 0
function nextKey(): string {
  rowSeq += 1
  return `tgt-${rowSeq}`
}

/** 把外部值转成内部行。 */
function toRows(targets: Target[]): Row[] {
  return targets.map((t) => ({
    key: nextKey(),
    host: typeof t.host === 'string' ? t.host : '',
    port: typeof t.port === 'number' && t.port > 0 ? t.port : null,
    weight: typeof t.weight === 'number' && t.weight > 0 ? t.weight : 1,
    status: t.status === 'down' ? 'down' : 'up',
  }))
}

/**
 * 把内部行转成外部值。
 *
 * 尚未填完的行（缺 host 或 port）不向上推送：
 * 用户点「添加」后立刻提交表单时，不应因为空行而校验失败。
 * 但该行仍留在界面上，用户可以继续补全。
 */
function toTargets(rows: Row[]): Target[] {
  return rows
    .filter((r) => r.host.trim() !== '' && r.port !== null)
    .map((r) => ({
      host: r.host.trim(),
      port: r.port as number,
      weight: r.weight,
      status: r.status,
    }))
}

/** 用于判断外部值是否发生了「结构变化」的指纹。 */
function signature(targets: Target[]): string {
  return JSON.stringify(
    targets.map((t) => [t.host ?? '', t.port ?? 0, t.weight ?? 1, t.status ?? 'up']),
  )
}

/** 校验主机名：域名或 IPv4/IPv6 字面量。 */
export function isValidHost(host: string): boolean {
  const h = host.trim()
  if (!h || h.length > 253) return false
  // IPv6 字面量（含 IPv4-mapped 形式）
  if (h.includes(':')) {
    return /^[0-9a-fA-F:.]+$/.test(h) && /^[0-9a-fA-F:]+$/.test(h.replace(/\./g, ''))
  }
  // IPv4
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) {
    return h.split('.').every((seg) => Number(seg) >= 0 && Number(seg) <= 255)
  }
  // 域名：允许通配前缀 `*.`
  if (h.startsWith('*.')) return isValidHost(h.slice(2))
  return /^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$/.test(
    h,
  )
}

export default function TargetList({
  value,
  onChange,
  disabled = false,
  showWeight = true,
  maxWeight = 100,
  maxRows = 20,
}: Props) {
  const { t } = useI18n()

  const [rows, setRows] = useState<Row[]>(() => toRows(value ?? []))
  /** 最近一次「自己推出去」的指纹，用于区分外部改动与自身回声。 */
  const lastEmitted = useRef<string>(signature(value ?? []))
  /** 是否已编辑过任意字段（决定是否显示校验错误）。 */
  const [touched, setTouched] = useState<Record<string, boolean>>({})

  // 拖拽中的行索引（原生 HTML5 drag 不携带 payload，用 state 记录源位置）。
  const [dragIndex, setDragIndex] = useState<number | null>(null)
  // 悬停高亮的目标位置，给出「拖到这里」的视觉反馈。
  const [overIndex, setOverIndex] = useState<number | null>(null)

  // 外部值变化时回灌（例如「编辑」打开另一条记录、导入后重置表单）。
  useEffect(() => {
    const incoming = value ?? []
    const sig = signature(incoming)
    if (sig === lastEmitted.current) return
    lastEmitted.current = sig
    setRows(toRows(incoming))
    setTouched({})
  }, [value])

  /** 用新的行数组更新本地状态并向上推送。 */
  const emit = (next: Row[]) => {
    setRows(next)
    const targets = toTargets(next)
    lastEmitted.current = signature(targets)
    onChange?.(targets)
  }

  const patch = (index: number, part: Partial<Row>) => {
    emit(rows.map((r, i) => (i === index ? { ...r, ...part } : r)))
  }

  const add = () => {
    emit([...rows, { key: nextKey(), host: '', port: null, weight: 1, status: 'up' }])
  }

  const remove = (index: number) => {
    emit(rows.filter((_, i) => i !== index))
  }

  /** 拖拽落位：把源行取出插入到目标位置。 */
  const drop = (dst: number) => {
    if (dragIndex === null || dragIndex === dst) {
      setDragIndex(null)
      setOverIndex(null)
      return
    }
    const next = [...rows]
    const moved = next.splice(dragIndex, 1)[0]
    if (moved) next.splice(dst, 0, moved)
    emit(next)
    setDragIndex(null)
    setOverIndex(null)
  }

  const markTouched = (key: string) => setTouched((s) => ({ ...s, [key]: true }))

  const hostError = (row: Row): string => {
    if (!touched[`${row.key}-host`]) return ''
    const h = row.host.trim()
    if (!h) return t('target.hostRequired')
    if (!isValidHost(h)) return t('target.hostInvalid')
    return ''
  }

  const portError = (row: Row): string => {
    if (!touched[`${row.key}-port`]) return ''
    if (row.port === null) return t('target.portRequired')
    if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) {
      return t('target.portInvalid')
    }
    return ''
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={6}>
      {rows.length === 0 && (
        <div className="or-empty" style={{ padding: '20px 0' }}>
          <Text type="secondary">{t('target.empty')}</Text>
        </div>
      )}

      {rows.map((row, index) => {
        const hErr = hostError(row)
        const pErr = portError(row)
        const dragging = dragIndex === index
        return (
          <div
            key={row.key}
            draggable={!disabled}
            onDragStart={() => setDragIndex(index)}
            onDragOver={(e) => {
              e.preventDefault()
              setOverIndex(index)
            }}
            onDragLeave={() => setOverIndex((v) => (v === index ? null : v))}
            onDrop={(e) => {
              e.preventDefault()
              drop(index)
            }}
            onDragEnd={() => {
              setDragIndex(null)
              setOverIndex(null)
            }}
            style={{
              display: 'flex',
              alignItems: 'flex-start',
              gap: 8,
              padding: '8px 8px 8px 4px',
              border: '1px solid var(--or-border)',
              borderLeft:
                row.status === 'down'
                  ? '3px solid var(--or-text-disabled)'
                  : '3px solid var(--or-primary)',
              borderRadius: 'var(--or-radius)',
              background:
                overIndex === index && dragIndex !== null
                  ? 'var(--or-primary-bg)'
                  : 'var(--or-bg-container)',
              opacity: dragging ? 0.45 : 1,
            }}
          >
            <Tooltip title={t('target.dragHint')}>
              <HolderOutlined
                style={{
                  cursor: disabled ? 'default' : 'grab',
                  color: 'var(--or-text-tertiary)',
                  paddingTop: 9,
                  paddingLeft: 4,
                }}
              />
            </Tooltip>

            {/* 优先级序号：故障转移时按此顺序尝试 */}
            <Tooltip title={t('target.priorityHint')}>
              <PushpinOutlined
                style={{ paddingTop: 10, color: 'var(--or-text-tertiary)', fontSize: 12 }}
              />
            </Tooltip>
            <Text
              type="secondary"
              className="or-mono"
              style={{ paddingTop: 7, minWidth: 16, textAlign: 'center' }}
            >
              {index + 1}
            </Text>

            <div style={{ flex: '1 1 240px', minWidth: 170 }}>
              <Input
                value={row.host}
                disabled={disabled}
                status={hErr ? 'error' : undefined}
                placeholder={t('target.hostPlaceholder')}
                onChange={(e) => patch(index, { host: e.target.value })}
                onBlur={() => markTouched(`${row.key}-host`)}
              />
              {hErr && (
                <Text type="danger" style={{ fontSize: 12 }}>
                  {hErr}
                </Text>
              )}
            </div>

            <div style={{ flex: '0 0 116px' }}>
              <InputNumber
                value={row.port}
                disabled={disabled}
                min={1}
                max={65535}
                precision={0}
                style={{ width: '100%' }}
                placeholder={t('target.port')}
                status={pErr ? 'error' : undefined}
                onChange={(v) => patch(index, { port: typeof v === 'number' ? v : null })}
                onBlur={() => markTouched(`${row.key}-port`)}
              />
              {pErr && (
                <Text type="danger" style={{ fontSize: 12 }}>
                  {pErr}
                </Text>
              )}
            </div>

            {showWeight && (
              <div style={{ flex: '0 0 220px', display: 'flex', alignItems: 'center', gap: 8 }}>
                <Tooltip title={t('target.weightHint')}>
                  <Text type="secondary" style={{ fontSize: 12, whiteSpace: 'nowrap' }}>
                    {t('node.weight')}
                  </Text>
                </Tooltip>
                <Slider
                  value={row.weight}
                  disabled={disabled}
                  min={1}
                  max={maxWeight}
                  style={{ flex: 1, minWidth: 70, margin: '7px 0 0' }}
                  onChange={(v: number) => patch(index, { weight: v })}
                />
                <InputNumber
                  value={row.weight}
                  disabled={disabled}
                  min={1}
                  max={maxWeight}
                  size="small"
                  style={{ width: 56 }}
                  onChange={(v) => patch(index, { weight: typeof v === 'number' ? v : 1 })}
                />
              </div>
            )}

            {/* 启用 / 停用：停用的目标不参与分发，等价于手动摘除 */}
            <Button
              size="small"
              disabled={disabled}
              type={row.status === 'up' ? 'default' : 'dashed'}
              style={{ marginTop: 3 }}
              onClick={() => patch(index, { status: row.status === 'up' ? 'down' : 'up' })}
            >
              {row.status === 'up' ? t('common.enabled') : t('common.disabled')}
            </Button>

            <Button
              size="small"
              danger
              type="text"
              disabled={disabled}
              icon={<DeleteOutlined />}
              style={{ marginTop: 3 }}
              onClick={() => remove(index)}
            />
          </div>
        )
      })}

      <Space wrap>
        <Button
          type="dashed"
          size="small"
          icon={<PlusOutlined />}
          disabled={disabled || rows.length >= maxRows}
          onClick={add}
        >
          {t('target.add')}
        </Button>
        {rows.length > 0 && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('target.orderHint')}
          </Text>
        )}
      </Space>
    </Space>
  )
}
