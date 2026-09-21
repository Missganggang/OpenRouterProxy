/**
 * 流量曲线（规格书 6.10 / 9.2）。
 *
 * 由仪表盘与流量统计页共用：
 *  - X 轴是时间桶（后端给的是「YYYY-MM-DD HH:00」或「YYYY-MM-DD」）；
 *  - Y 轴是字节数，轴上与 tooltip 都做 1024 进制格式化，避免出现「1234567890」这类读数；
 *  - 展示的是乘倍率后的字节，`showRaw` 打开时改用原始字节（规格书 6.10 的统计口径）。
 */
import { useMemo } from 'react'
import { Empty, Skeleton } from 'antd'
import { Line } from '@ant-design/plots'

import type { TrafficPoint } from '../api/types'
import { useI18n } from '../locales'

interface Props {
  points: TrafficPoint[]
  loading?: boolean
  height?: number
  /** 显示原始字节（未乘倍率）而非计费字节。 */
  showRaw?: boolean
}

/** 曲线主色，与 CSS 变量 --or-primary 保持一致。 */
const LINE_COLOR = '#165dff'

/** 图表内部使用的数据行。 */
interface ChartRow {
  /** 用于排序与 tooltip 标题的可读时间。 */
  label: string
  /** 排序键（时间戳，解析失败时为下标）。 */
  order: number
  value: number
}

/**
 * 1024 进制的字节格式化。
 *
 * 与 NodeCard、节点详情共用，因此导出。
 * 例：1536 → "1.5 KB"；0 → "0 B"。
 */
export function formatBytes(n: unknown, digits = 2): string {
  // 刻意用 unknown 接收并做运行时兜底：这个函数散落在十多个页面里，
  // 一旦上游字段口径变化（例如把 {total,raw} 对象当数字传进来），
  // 严格类型只能在编译期拦住一部分调用点，运行期仍会渲染出 NaN。
  // 这里统一降级为 '0 B'，保证界面不会因为一个字段而整页崩掉。
  const num = typeof n === 'number' ? n : Number(n)
  if (!Number.isFinite(num) || num <= 0) return '0 B'

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let value = num
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  // 字节层不保留小数；其余按量级决定精度，避免「1.00 GB」这种噪声。
  const fixed = unit === 0 ? 0 : value >= 100 ? 1 : digits
  return `${value.toFixed(fixed)} ${units[unit]}`
}

/** 把时间桶标识转成图表用的可读文本与排序键。 */
function normalizeTime(time: string, index: number): { label: string; order: number } {
  // 后端给的是「YYYY-MM-DD HH:00」或「YYYY-MM-DD」，Safari 不接受空格分隔，
  // 统一替换成 ISO 的 T 再解析。
  const parsed = new Date(time.replace(' ', 'T'))
  if (Number.isNaN(parsed.getTime())) {
    return { label: time, order: index }
  }
  return { label: parsed.toLocaleString(), order: parsed.getTime() }
}

export default function TrafficChart({ points, loading, height = 280, showRaw }: Props) {
  const { t } = useI18n()

  const rows = useMemo<ChartRow[]>(
    () =>
      (points ?? []).map((p, i) => {
        const { label, order } = normalizeTime(p.time, i)
        const bytes = showRaw ? p.raw_bytes : p.bytes
        return { label, order, value: Number.isFinite(bytes) ? bytes : 0 }
      }),
    [points, showRaw],
  )

  const config = useMemo(
    () => ({
      data: rows,
      xField: 'label',
      yField: 'value',
      height,
      autoFit: true,
      smooth: true,
      animate: false,
      // 采样点过多时 x 轴标签会互相覆盖，交给 G2 自动抽稀。
      axis: {
        x: { title: false, labelAutoRotate: true },
        y: {
          title: false,
          labelFormatter: (v: number | string) => formatBytes(Number(v)),
        },
      },
      style: { stroke: LINE_COLOR, lineWidth: 2 },
      area: {
        style: {
          fill: LINE_COLOR,
          fillOpacity: 0.12,
        },
      },
      tooltip: {
        items: [
          {
            channel: 'y',
            valueFormatter: (v: number) => formatBytes(Number(v)),
          },
        ],
      },
    }),
    [rows, height],
  )

  if (loading && rows.length === 0) {
    return <Skeleton active paragraph={{ rows: 5 }} />
  }

  // 空数据必须显式处理：G2 在空数组下会渲染一块空白画布。
  if (rows.length === 0) {
    return (
      <div
        style={{
          height,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
        }}
      >
        <Empty
          image={Empty.PRESENTED_IMAGE_SIMPLE}
          description={t('traffic.title')}
        />
      </div>
    )
  }

  return <Line {...config} />
}
