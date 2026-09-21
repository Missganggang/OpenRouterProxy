/**
 * 规则同步状态标签（规格书 5.4 的状态机可视化，规格书 11.3）。
 *
 * 状态配色与规格书一致：
 *   unsynced 未同步（黄色）/ syncing 同步中（蓝色）
 *   normal 正常（绿色）/ failed 同步失败（红色）
 *
 * 同步失败时点击标签可以展开查看节点返回的原始错误（规格书 6.4 要求）。
 */
import { Tag, Tooltip, Modal, Typography } from 'antd'
import {
  CheckCircleOutlined,
  ClockCircleOutlined,
  CloseCircleOutlined,
  SyncOutlined,
} from '@ant-design/icons'

import type { SyncStatus } from '../api/types'
import { useI18n } from '../locales'

const { Paragraph, Text } = Typography

interface Props {
  status: SyncStatus
  /** 同步失败时的原始错误信息。 */
  error?: string
  /** 最近一次同步成功时间。 */
  syncedAt?: string | null
  /** 是否显示图标（表格里通常隐藏以节省空间）。 */
  showIcon?: boolean
}

/** 各状态的展示配置。 */
const STATUS_META: Record<
  SyncStatus,
  { color: string; i18nKey: string; icon: React.ReactNode }
> = {
  unsynced: {
    color: 'warning',
    i18nKey: 'rule.sync.unsynced',
    icon: <ClockCircleOutlined />,
  },
  syncing: {
    color: 'processing',
    i18nKey: 'rule.sync.syncing',
    icon: <SyncOutlined spin />,
  },
  normal: {
    color: 'success',
    i18nKey: 'rule.sync.normal',
    icon: <CheckCircleOutlined />,
  },
  failed: {
    color: 'error',
    i18nKey: 'rule.sync.failed',
    icon: <CloseCircleOutlined />,
  },
}

export default function SyncStatusTag({ status, error, syncedAt, showIcon = true }: Props) {
  const { t } = useI18n()
  const meta = STATUS_META[status] ?? STATUS_META.unsynced

  const tag = (
    <Tag color={meta.color} icon={showIcon ? meta.icon : undefined} style={{ margin: 0 }}>
      {t(meta.i18nKey)}
    </Tag>
  )

  // 正常状态只显示标签，额外信息放进 Tooltip。
  if (status === 'normal' && syncedAt) {
    return <Tooltip title={`${t('common.updatedAt')}: ${formatTime(syncedAt)}`}>{tag}</Tooltip>
  }

  // 失败状态：可点击查看原始错误。
  if (status === 'failed') {
    return (
      <Tooltip title={error ? '点击查看失败原因' : undefined}>
        <span
          style={{ cursor: error ? 'pointer' : 'default' }}
          onClick={() => {
            if (!error) return
            Modal.error({
              title: t('rule.sync.failed'),
              width: 640,
              content: (
                <div>
                  <Paragraph type="secondary" style={{ marginBottom: 8 }}>
                    节点返回的原始错误：
                  </Paragraph>
                  <Paragraph>
                    <Text code style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                      {error}
                    </Text>
                  </Paragraph>
                </div>
              ),
            })
          }}
        >
          {tag}
        </span>
      </Tooltip>
    )
  }

  return tag
}

/** 把 RFC3339 时间转成本地可读文本。 */
function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}
