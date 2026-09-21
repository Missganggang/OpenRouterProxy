/**
 * 节点分组（规格书 6.2 / 9.2）。
 *
 * 分组只用于批量管理与筛选，不改变任何转发行为，因此这里的操作都很轻：
 *  - CRUD：名称 + 备注；
 *  - 查看节点：抽屉里列出组内节点（只读，避免误改造成「为什么规则没生效」）；
 *  - 导出 CSV：交给后端生成，前端只负责触发下载；
 *  - 排序：后端没有 sort 字段，顺序只影响本机展示，存在 localStorage，
 *    这样既不假装后端支持排序，也不用为了拖拽引入新依赖。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Button,
  Drawer,
  Empty,
  Form,
  Input,
  Modal,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  ArrowDownOutlined,
  ArrowUpOutlined,
  DeleteOutlined,
  EditOutlined,
  ExportOutlined,
  EyeOutlined,
  PlusOutlined,
  ReloadOutlined,
} from '@ant-design/icons'

import { nodeGroupApi, showApiError } from '../../api'
import type { Node, NodeGroup } from '../../api/types'
import { usePolling } from '../../hooks/usePolling'
import { useI18n } from '../../locales'

const { Text } = Typography

/** 本机保存的分组展示顺序。 */
const ORDER_KEY = 'openroute_node_group_order'

/** 读取本机保存的顺序（数组元素是分组 ID）。 */
function readOrder(): number[] {
  try {
    const raw = localStorage.getItem(ORDER_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((v): v is number => typeof v === 'number') : []
  } catch {
    return []
  }
}

/** 写入本机顺序。 */
function writeOrder(ids: number[]): void {
  try {
    localStorage.setItem(ORDER_KEY, JSON.stringify(ids))
  } catch {
    // 无痕模式下 localStorage 可能不可写，忽略即可：顺序只是展示偏好。
  }
}

/** 按本机顺序重排分组列表；未登记的 ID 追加在末尾，保持后端顺序。 */
function applyOrder<T extends { id: number }>(items: T[], order: number[]): T[] {
  if (order.length === 0) return items
  const rank = new Map(order.map((id, i) => [id, i]))
  return [...items].sort((a, b) => {
    const ra = rank.has(a.id) ? (rank.get(a.id) as number) : Number.MAX_SAFE_INTEGER
    const rb = rank.has(b.id) ? (rank.get(b.id) as number) : Number.MAX_SAFE_INTEGER
    return ra - rb
  })
}

export default function NodeGroupsPage() {
  const { t } = useI18n()
  const navigate = useNavigate()
  const [form] = Form.useForm<{ name: string; remark?: string }>()

  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<NodeGroup | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [exporting, setExporting] = useState(0)

  // 组内节点抽屉
  const [viewGroup, setViewGroup] = useState<NodeGroup | null>(null)
  const [nodes, setNodes] = useState<Node[]>([])
  const [nodesLoading, setNodesLoading] = useState(false)
  const [nodesError, setNodesError] = useState('')

  // ── 数据 ────────────────────────────────────────────────────────────────
  const { data, loading, refresh } = usePolling(() => nodeGroupApi.list({ page_size: 200 }), {
    interval: 60000,
  })

  // 本机展示顺序（后端暂无 sort 字段，见 group.orderHint 的说明）。
  const [localOrder, setLocalOrder] = useState<number[]>(readOrder)

  const groups = useMemo(() => applyOrder(data?.items ?? [], localOrder), [data, localOrder])

  /** 上移 / 下移：仅调整本机顺序。 */
  const move = useCallback(
    (id: number, delta: -1 | 1) => {
      const ids = groups.map((g) => g.id)
      const index = ids.indexOf(id)
      const target = index + delta
      if (index < 0 || target < 0 || target >= ids.length) return
      const next = [...ids]
      const [moved] = next.splice(index, 1)
      next.splice(target, 0, moved)
      setLocalOrder(next)
      writeOrder(next)
    },
    [groups],
  )

  /** 打开「新建分组」。 */
  const openCreate = useCallback(() => {
    setEditing(null)
    form.resetFields()
    setFormOpen(true)
  }, [form])

  /** 打开「编辑分组」。 */
  const openEdit = useCallback(
    (group: NodeGroup) => {
      setEditing(group)
      form.setFieldsValue({ name: group.name, remark: group.remark })
      setFormOpen(true)
    },
    [form],
  )

  /** 提交新建 / 编辑。 */
  const submit = useCallback(async () => {
    let values: { name: string; remark?: string }
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSubmitting(true)
    try {
      if (editing) {
        await nodeGroupApi.update(editing.id, values)
        message.success(t('group.updated'))
      } else {
        await nodeGroupApi.create(values)
        message.success(t('group.created'))
      }
      setFormOpen(false)
      await refresh()
    } catch (err) {
      showApiError(err)
    } finally {
      setSubmitting(false)
    }
  }, [editing, form, refresh, t])

  /** 删除分组：确认框里写明影响范围（规格书 9.3）。 */
  const confirmDelete = useCallback(
    (group: NodeGroup) => {
      const memberCount = group.id === viewGroup?.id ? nodes.length : undefined
      Modal.confirm({
        title: t('group.deleteConfirm', { name: group.name }),
        content: (
          <div>
            <p style={{ marginBottom: 4 }}>
              {memberCount === undefined
                ? t('group.deleteImpactUnknown')
                : t('group.deleteImpact', { n: memberCount })}
            </p>
            <Text type="secondary" style={{ fontSize: 12 }}>
              {t('group.deleteImpactDetail')}
            </Text>
          </div>
        ),
        okText: t('common.delete'),
        okButtonProps: { danger: true },
        cancelText: t('common.cancel'),
        onOk: async () => {
          try {
            await nodeGroupApi.remove(group.id)
            message.success(t('group.deleted'))
            // 删掉本机顺序里的残留 ID，避免顺序表越滚越长。
            const next = localOrder.filter((id) => id !== group.id)
            setLocalOrder(next)
            writeOrder(next)
            await refresh()
          } catch (err) {
            showApiError(err)
            throw err
          }
        },
      })
    },
    [localOrder, nodes.length, refresh, t, viewGroup],
  )

  /** 打开「查看节点」抽屉。 */
  const openView = useCallback(
    async (group: NodeGroup) => {
      setViewGroup(group)
      setNodes([])
      setNodesError('')
      setNodesLoading(true)
      try {
        const list = await nodeGroupApi.nodes(group.id)
        setNodes(list ?? [])
      } catch (err) {
        setNodesError(err instanceof Error ? err.message : t('group.nodesLoadFailed'))
      } finally {
        setNodesLoading(false)
      }
    },
    [t],
  )

  /** 导出 CSV：下载由 api 层负责，这里只处理失败提示。 */
  const handleExport = useCallback(
    async (group: NodeGroup) => {
      setExporting(group.id)
      try {
        await nodeGroupApi.exportCSV(group.id, group.name)
        message.success(t('group.exportDone', { name: group.name }))
      } catch (err) {
        showApiError(err)
      } finally {
        setExporting(0)
      }
    },
    [t],
  )

  // 抽屉里的分组被删除后同步关闭，避免停留在失效资源上。
  useEffect(() => {
    if (viewGroup && !groups.some((g) => g.id === viewGroup.id) && !loading) {
      setViewGroup(null)
    }
  }, [groups, loading, viewGroup])

  // ── 列定义 ──────────────────────────────────────────────────────────────
  const columns: ColumnsType<NodeGroup> = useMemo(
    () => [
      {
        title: t('common.name'),
        dataIndex: 'name',
        render: (v: string, group) => (
          <Space size={6}>
            <a onClick={() => void openView(group)} style={{ fontWeight: 500 }}>
              {v}
            </a>
            {group.remark && (
              <Tooltip title={group.remark}>
                <Tag color="default" style={{ margin: 0 }}>
                  {t('common.remark')}
                </Tag>
              </Tooltip>
            )}
          </Space>
        ),
      },
      {
        title: t('common.remark'),
        dataIndex: 'remark',
        render: (v: string) => (v ? v : <Text type="secondary">—</Text>),
      },
      {
        title: t('common.createdAt'),
        dataIndex: 'created_at',
        width: 180,
        render: (v: string) => <Text type="secondary">{formatTime(v)}</Text>,
      },
      {
        title: t('group.order'),
        key: 'order',
        width: 110,
        render: (_v, group) => {
          const index = groups.findIndex((g) => g.id === group.id)
          return (
            <Space size={2}>
              <Tooltip title={t('group.moveUp')}>
                <Button
                  type="text"
                  size="small"
                  icon={<ArrowUpOutlined />}
                  disabled={index <= 0}
                  onClick={() => move(group.id, -1)}
                />
              </Tooltip>
              <Tooltip title={t('group.moveDown')}>
                <Button
                  type="text"
                  size="small"
                  icon={<ArrowDownOutlined />}
                  disabled={index < 0 || index >= groups.length - 1}
                  onClick={() => move(group.id, 1)}
                />
              </Tooltip>
            </Space>
          )
        },
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 220,
        render: (_v, group) => (
          <div className="or-actions">
            <Tooltip title={t('group.viewNodes')}>
              <Button
                type="text"
                size="small"
                icon={<EyeOutlined />}
                onClick={() => void openView(group)}
              />
            </Tooltip>
            <Tooltip title={t('group.exportCsv')}>
              <Button
                type="text"
                size="small"
                icon={<ExportOutlined />}
                loading={exporting === group.id}
                onClick={() => void handleExport(group)}
              />
            </Tooltip>
            <Tooltip title={t('common.edit')}>
              <Button
                type="text"
                size="small"
                icon={<EditOutlined />}
                onClick={() => openEdit(group)}
              />
            </Tooltip>
            <Tooltip title={t('common.delete')}>
              <Button
                type="text"
                size="small"
                danger
                icon={<DeleteOutlined />}
                onClick={() => confirmDelete(group)}
              />
            </Tooltip>
          </div>
        ),
      },
    ],
    [confirmDelete, exporting, groups, handleExport, move, openEdit, openView, t],
  )

  /** 组内节点表格。 */
  const nodeColumns: ColumnsType<Node> = useMemo(
    () => [
      {
        title: t('common.name'),
        dataIndex: 'name',
        render: (v: string, node) => (
          <Space size={6}>
            <span className={node.online ? 'or-dot or-dot-online' : 'or-dot or-dot-offline'} />
            <a onClick={() => navigate(`/nodes/${node.id}`)}>{v}</a>
          </Space>
        ),
      },
      {
        title: t('node.role'),
        dataIndex: 'role',
        width: 90,
        render: (role: Node['role']) => <Tag style={{ margin: 0 }}>{t(`node.role.${role}`)}</Tag>,
      },
      {
        title: t('node.publicIp'),
        dataIndex: 'public_ipv4',
        width: 160,
        render: (v: string) => <span className="or-mono">{v || '—'}</span>,
      },
      {
        title: t('common.status'),
        dataIndex: 'online',
        width: 100,
        render: (online: boolean) => (
          <Tag color={online ? 'success' : 'default'} style={{ margin: 0 }}>
            {online ? t('node.online') : t('node.offline')}
          </Tag>
        ),
      },
    ],
    [navigate, t],
  )

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h1 className="or-page-title">{t('group.title')}</h1>
          <p className="or-page-desc">{t('group.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void refresh()}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('group.add')}
          </Button>
        </Space>
      </div>

      <Text type="secondary" style={{ fontSize: 12 }}>
        {t('group.orderHint')}
      </Text>

      <Table<NodeGroup>
        rowKey="id"
        size="small"
        style={{ marginTop: 8 }}
        columns={columns}
        dataSource={groups}
        loading={loading && groups.length === 0}
        pagination={false}
        locale={{
          emptyText: (
            <div className="or-empty">
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('group.empty')}>
                <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                  {t('group.add')}
                </Button>
              </Empty>
            </div>
          ),
        }}
      />

      {/* 新建 / 编辑分组 */}
      <Modal
        open={formOpen}
        title={editing ? t('group.editTitle') : t('group.add')}
        onCancel={() => setFormOpen(false)}
        onOk={() => void submit()}
        confirmLoading={submitting}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        width={520}
        destroyOnClose
      >
        <Form form={form} layout="vertical" preserve={false}>
          <Form.Item
            name="name"
            label={t('common.name')}
            rules={[
              { required: true, message: t('group.nameRequired') },
              { max: 64, message: t('group.nameTooLong') },
            ]}
          >
            <Input placeholder="hk-inbound" autoComplete="off" />
          </Form.Item>
          <Form.Item name="remark" label={t('common.remark')}>
            <Input.TextArea
              rows={3}
              maxLength={255}
              showCount
              placeholder={t('group.remarkPlaceholder')}
            />
          </Form.Item>
        </Form>
      </Modal>

      {/* 查看组内节点 */}
      <Drawer
        open={viewGroup !== null}
        width={720}
        title={
          viewGroup
            ? t('group.nodesTitle', { name: viewGroup.name, n: nodes.length })
            : t('group.viewNodes')
        }
        onClose={() => setViewGroup(null)}
        extra={
          viewGroup && (
            <Space>
              <Button
                size="small"
                icon={<ExportOutlined />}
                loading={exporting === viewGroup.id}
                onClick={() => void handleExport(viewGroup)}
              >
                {t('group.exportCsv')}
              </Button>
              <Button size="small" type="link" onClick={() => navigate('/nodes')}>
                {t('group.goNodes')}
              </Button>
            </Space>
          )
        }
      >
        {nodesError ? (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description={nodesError}
          >
            <Button size="small" onClick={() => viewGroup && void openView(viewGroup)}>
              {t('common.refresh')}
            </Button>
          </Empty>
        ) : (
          <Table<Node>
            rowKey="id"
            size="small"
            columns={nodeColumns}
            dataSource={nodes}
            loading={nodesLoading}
            pagination={false}
            locale={{
              emptyText: (
                <div className="or-empty">
                  <Empty
                    image={Empty.PRESENTED_IMAGE_SIMPLE}
                    description={t('group.nodesEmpty')}
                  />
                </div>
              ),
            }}
          />
        )}
      </Drawer>
    </div>
  )
}

/** 时间展示：仅显示到分钟，分组列表不需要秒级精度。 */
function formatTime(value: string): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}
