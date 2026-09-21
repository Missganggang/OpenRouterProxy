/**
 * 设备组页面（规格书 9.2、6.3、8.8）。
 *
 * 入口组 / 出口组两个 Tab 共用一个表格；创建与编辑都走 `DeviceGroupForm`
 * 动态表单。删除时后端在被规则引用时返回 40903，这里给出明确指引
 * 而不是一句「操作失败」。
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Drawer,
  Empty,
  Input,
  Modal,
  Progress,
  Space,
  Spin,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import {
  ArrowDownOutlined,
  ArrowUpOutlined,
  DeleteOutlined,
  EditOutlined,
  HeartOutlined,
  PlusOutlined,
  ReloadOutlined,
  SwapOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'

import { ApiError, deviceGroupApi, nodeApi, showApiError } from '../../api'
import type { DeviceGroup, DeviceGroupType, Node } from '../../api/types'
import type { GroupHealth } from '../../api/modules/deviceGroup'
import DeviceGroupForm from '../../components/DeviceGroupForm'
import { useI18n } from '../../locales'
import { usePolling } from '../../hooks/usePolling'

const { Text, Paragraph } = Typography

/** 被规则引用时的错误码（规格书 8.8）。 */
const CODE_GROUP_IN_USE = 40903

export default function DeviceGroupsPage() {
  const { t } = useI18n()
  const [activeType, setActiveType] = useState<DeviceGroupType>('inbound')

  const [groups, setGroups] = useState<DeviceGroup[]>([])
  const [loading, setLoading] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [keyword, setKeyword] = useState('')

  // 节点 ID → 节点，用于在表格里展示组成员名称与角色。
  const [nodeMap, setNodeMap] = useState<Record<number, Node>>({})

  // 创建 / 编辑抽屉
  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<DeviceGroup | null>(null)

  // 健康视图
  const [healthGroup, setHealthGroup] = useState<DeviceGroup | null>(null)

  // 成员排序
  const [reorderGroup, setReorderGroup] = useState<DeviceGroup | null>(null)

  /** 拉取设备组列表。 */
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await deviceGroupApi.list({ page: 1, page_size: 200 })
      setGroups(res.items)
    } catch (err) {
      showApiError(err, t('deviceGroup.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  /** 拉取节点表（一次即可，用于名称/角色展示）。 */
  const loadNodes = useCallback(async () => {
    try {
      const res = await nodeApi.list({ page: 1, page_size: 500 })
      const map: Record<number, Node> = {}
      res.items.forEach((n) => {
        map[n.id] = n
      })
      setNodeMap(map)
    } catch {
      // 节点列表拉取失败只影响名称展示，不阻断设备组页面。
    }
  }, [])

  useEffect(() => {
    void load()
    void loadNodes()
  }, [load, loadNodes])

  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    return groups.filter((g) => {
      if (g.type !== activeType) return false
      if (!kw) return true
      return (
        g.name.toLowerCase().includes(kw) || (g.remark ?? '').toLowerCase().includes(kw)
      )
    })
  }, [groups, activeType, keyword])

  /** 同类型的其它组（供故障转移选择）。 */
  const allGroups = useMemo(() => groups, [groups])

  const openCreate = () => {
    setEditing(null)
    setEditorOpen(true)
  }

  const openEdit = (g: DeviceGroup) => {
    setEditing(g)
    setEditorOpen(true)
  }

  /** 提交创建 / 更新。 */
  const handleSubmit = async (input: Parameters<
    NonNullable<React.ComponentProps<typeof DeviceGroupForm>['onSubmit']>
  >[0]) => {
    setSubmitting(true)
    try {
      if (editing) {
        await deviceGroupApi.update(editing.id, input)
        message.success(t('deviceGroup.updateSuccess'))
      } else {
        await deviceGroupApi.create(input)
        message.success(t('deviceGroup.createSuccess'))
      }
      setEditorOpen(false)
      setEditing(null)
      await load()
    } catch (err) {
      // 42204 等配置类错误由后端给出字段级提示，这里原样上报。
      showApiError(err, t('deviceGroup.saveFailed'))
    } finally {
      setSubmitting(false)
    }
  }

  /** 删除设备组，单独处理「被引用」的 40903。 */
  const handleDelete = (g: DeviceGroup) => {
    Modal.confirm({
      title: t('deviceGroup.deleteConfirm', { name: g.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      width: 520,
      content: (
        <Space direction="vertical" size={6}>
          <Text>
            {t('deviceGroup.deleteImpact', { count: g.node_ids.length })}
          </Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('deviceGroup.deleteImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await deviceGroupApi.remove(g.id)
          message.success(t('deviceGroup.deleteSuccess'))
          await load()
        } catch (err) {
          if (err instanceof ApiError && err.code === CODE_GROUP_IN_USE) {
            // 40903：组被规则引用，给出「先改规则」的明确指引。
            Modal.error({
              title: t('deviceGroup.inUseTitle'),
              okText: t('common.ok'),
              content: (
                <Space direction="vertical" size={8}>
                  <Text>{err.message}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('deviceGroup.inUseDetail')}
                  </Text>
                  {err.details?.hint && (
                    <Alert type="info" showIcon message={err.details.hint} />
                  )}
                </Space>
              ),
            })
            return
          }
          showApiError(err, t('deviceGroup.deleteFailed'))
          // 抛出以阻止 Modal 关闭（antd 会把 reject 当作校验失败）。
          throw err
        }
      },
    })
  }

  const columns: ColumnsType<DeviceGroup> = [
    {
      title: t('common.name'),
      dataIndex: 'name',
      key: 'name',
      width: 200,
      fixed: 'left',
      render: (name: string, row) => (
        <Space direction="vertical" size={0}>
          <Text strong>{name}</Text>
          {row.remark ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {row.remark}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: t('deviceGroup.nodeCount'),
      dataIndex: 'node_ids',
      key: 'node_ids',
      width: 260,
      render: (ids: number[]) => {
        if (!ids?.length) return <Text type="secondary">{t('deviceGroup.noNodes')}</Text>
        return (
          <Space size={4} wrap>
            {ids.map((id) => {
              const n = nodeMap[id]
              return (
                <Tooltip
                  key={id}
                  title={n ? `${n.name} · ${roleText(n.role, t)}` : `#${id}`}
                >
                  <Tag color={n?.online ? 'green' : 'default'} style={{ margin: 0 }}>
                    {n?.name ?? `#${id}`}
                  </Tag>
                </Tooltip>
              )
            })}
          </Space>
        )
      },
    },
    {
      title: t('deviceGroup.protocol'),
      key: 'protocol',
      width: 100,
      render: (_, row) => {
        const cfg = row.config as { protocol?: string } | undefined
        return <Tag className="or-mono">{cfg?.protocol ?? '-'}</Tag>
      },
    },
    {
      title: t('deviceGroup.balance'),
      dataIndex: 'balance',
      key: 'balance',
      width: 130,
      render: (v: string, row) =>
        row.type === 'inbound' ? (
          <Tooltip title={t('deviceGroup.inboundNoBalance')}>
            <Tag>{t('deviceGroup.na')}</Tag>
          </Tooltip>
        ) : (
          <Text className="or-mono">{v || 'least_conn'}</Text>
        ),
    },
    {
      title: t('deviceGroup.healthCheck'),
      key: 'health_check',
      width: 110,
      render: (_, row) =>
        row.type === 'inbound' ? (
          <Text type="secondary">-</Text>
        ) : (
          <Tag color={row.health_check_enable ? 'green' : 'default'}>
            {row.health_check_enable
              ? `${row.health_check_interval}s / ${row.health_check_fail_count}`
              : t('common.disabled')}
          </Tag>
        ),
    },
    {
      title: t('deviceGroup.failover'),
      key: 'failover',
      width: 150,
      render: (_, row) => {
        if (row.type === 'inbound' || !row.failover_group_id) {
          return <Text type="secondary">{t('deviceGroup.noFailover')}</Text>
        }
        const target = groups.find((g) => g.id === row.failover_group_id)
        return <Tag color="orange">{target?.name ?? `#${row.failover_group_id}`}</Tag>
      },
    },
    {
      title: t('common.updatedAt'),
      dataIndex: 'updated_at',
      key: 'updated_at',
      width: 170,
      render: (v: string) => <Text type="secondary">{formatTime(v)}</Text>,
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 210,
      fixed: 'right',
      render: (_, row) => (
        <div className="or-actions">
          <Tooltip title={t('common.edit')}>
            <Button size="small" type="text" icon={<EditOutlined />} onClick={() => openEdit(row)} />
          </Tooltip>
          <Tooltip title={t('deviceGroup.health')}>
            <Button
              size="small"
              type="text"
              icon={<HeartOutlined />}
              onClick={() => setHealthGroup(row)}
            />
          </Tooltip>
          <Tooltip title={t('deviceGroup.reorder')}>
            <Button
              size="small"
              type="text"
              icon={<SwapOutlined />}
              disabled={row.node_ids.length < 2}
              onClick={() => setReorderGroup(row)}
            />
          </Tooltip>
          <Tooltip title={t('common.delete')}>
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              onClick={() => handleDelete(row)}
            />
          </Tooltip>
        </div>
      ),
    },
  ]

  return (
    <div>
      <div className="or-page-header">
        <div>
          <h2 className="or-page-title">{t('deviceGroup.title')}</h2>
          <p className="or-page-desc">{t('deviceGroup.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('deviceGroup.add')}
          </Button>
        </Space>
      </div>

      <Card size="small" style={{ marginBottom: 12 }}>
        <div className="or-toolbar" style={{ marginBottom: 0 }}>
          <Input.Search
            allowClear
            style={{ width: 260 }}
            placeholder={t('deviceGroup.searchPlaceholder')}
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('common.total', { n: filtered.length })}
          </Text>
        </div>
      </Card>

      <Tabs
        activeKey={activeType}
        onChange={(k) => setActiveType(k as DeviceGroupType)}
        items={[
          {
            key: 'inbound',
            label: (
              <Space size={4}>
                {t('deviceGroup.inbound')}
                <Tag style={{ margin: 0 }}>
                  {groups.filter((g) => g.type === 'inbound').length}
                </Tag>
              </Space>
            ),
            children: (
              <>
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 12 }}
                  message={t('deviceGroup.inboundNoBalance')}
                  description={
                    <Text style={{ fontSize: 12 }}>
                      {t('deviceGroup.inboundNoBalanceDetail')}
                    </Text>
                  }
                />
                <Table<DeviceGroup>
                  rowKey="id"
                  size="small"
                  loading={loading}
                  columns={columns}
                  dataSource={filtered}
                  scroll={{ x: 1200 }}
                  pagination={{
                    pageSize: 20,
                    showSizeChanger: true,
                    showTotal: (total) => t('common.total', { n: total }),
                  }}
                  locale={{
                    emptyText: (
                      <Empty
                        className="or-empty"
                        description={
                          <Space direction="vertical" size={4}>
                            <Text>{t('deviceGroup.inboundEmpty')}</Text>
                            <Text type="secondary" style={{ fontSize: 12 }}>
                              {t('deviceGroup.inboundEmptyHint')}
                            </Text>
                          </Space>
                        }
                      >
                        <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                          {t('deviceGroup.add')}
                        </Button>
                      </Empty>
                    ),
                  }}
                />
              </>
            ),
          },
          {
            key: 'outbound',
            label: (
              <Space size={4}>
                {t('deviceGroup.outbound')}
                <Tag style={{ margin: 0 }}>
                  {groups.filter((g) => g.type === 'outbound').length}
                </Tag>
              </Space>
            ),
            children: (
              <Table<DeviceGroup>
                rowKey="id"
                size="small"
                loading={loading}
                columns={columns}
                dataSource={filtered}
                scroll={{ x: 1200 }}
                pagination={{
                  pageSize: 20,
                  showSizeChanger: true,
                  showTotal: (total) => t('common.total', { n: total }),
                }}
                locale={{
                  emptyText: (
                    <Empty
                      className="or-empty"
                      description={
                        <Space direction="vertical" size={4}>
                          <Text>{t('deviceGroup.outboundEmpty')}</Text>
                          <Text type="secondary" style={{ fontSize: 12 }}>
                            {t('deviceGroup.outboundEmptyHint')}
                          </Text>
                        </Space>
                      }
                    >
                      <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                        {t('deviceGroup.add')}
                      </Button>
                    </Empty>
                  ),
                }}
              />
            ),
          },
        ]}
      />

      {/* ── 创建 / 编辑抽屉 ─────────────────────────────── */}
      <Drawer
        destroyOnClose
        width={860}
        open={editorOpen}
        title={
          editing
            ? `${t('common.edit')} · ${editing.name}`
            : `${t('deviceGroup.add')} · ${activeType === 'inbound' ? t('deviceGroup.inbound') : t('deviceGroup.outbound')}`
        }
        onClose={() => {
          setEditorOpen(false)
          setEditing(null)
        }}
      >
        <DeviceGroupForm
          key={editing?.id ?? `new-${activeType}`}
          group={editing}
          type={editing?.type ?? activeType}
          allGroups={allGroups}
          submitting={submitting}
          onSubmit={handleSubmit}
          onCancel={() => {
            setEditorOpen(false)
            setEditing(null)
          }}
        />
      </Drawer>

      {/* ── 健康视图 ────────────────────────────────────── */}
      <HealthDrawer
        group={healthGroup}
        onClose={() => setHealthGroup(null)}
        t={t}
      />

      {/* ── 成员排序 ────────────────────────────────────── */}
      <ReorderModal
        group={reorderGroup}
        nodeMap={nodeMap}
        onClose={() => setReorderGroup(null)}
        onSaved={() => {
          setReorderGroup(null)
          void load()
        }}
        t={t}
      />
    </div>
  )
}

/* ───────────────────────── 健康视图 ───────────────────────── */

interface HealthDrawerProps {
  group: DeviceGroup | null
  onClose: () => void
  t: (key: string, vars?: Record<string, string | number>) => string
}

/** 组内节点健康状态与负载分布（规格书 8.8 GET /health）。 */
function HealthDrawer({ group, onClose, t }: HealthDrawerProps) {
  const id = group?.id ?? 0
  // 健康数据用轮询保持新鲜：节点上下线会影响负载分布。
  const { data, loading, refresh } = usePolling<GroupHealth | null>(
    useCallback(async () => {
      if (!id) return null
      try {
        return await deviceGroupApi.health(id)
      } catch (err) {
        showApiError(err, t('deviceGroup.healthLoadFailed'))
        return null
      }
    }, [id, t]),
    { interval: id ? 15000 : 0, immediate: true },
  )

  return (
    <Drawer
      width={720}
      open={Boolean(group)}
      onClose={onClose}
      title={`${t('deviceGroup.health')} · ${group?.name ?? ''}`}
      extra={
        <Button icon={<ReloadOutlined />} onClick={() => void refresh()}>
          {t('common.refresh')}
        </Button>
      }
    >
      {!data && loading ? (
        <Spin />
      ) : !data ? (
        <Empty className="or-empty" description={t('deviceGroup.healthEmpty')} />
      ) : (
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Alert
            type={data.available === 0 ? 'error' : data.available < data.total ? 'warning' : 'success'}
            showIcon
            message={t('deviceGroup.healthSummary', {
              available: data.available,
              total: data.total,
            })}
            description={
              group?.type === 'outbound' ? (
                <Text style={{ fontSize: 12 }}>
                  {t('deviceGroup.loadRatioHint')}
                </Text>
              ) : (
                <Text style={{ fontSize: 12 }}>{t('deviceGroup.inboundNoBalance')}</Text>
              )
            }
          />

          <Table
            rowKey="node_id"
            size="small"
            loading={loading}
            pagination={false}
            dataSource={data.nodes}
            columns={[
              {
                title: t('common.name'),
                dataIndex: 'node_name',
                key: 'node_name',
                render: (v: string, row) => (
                  <Space size={6}>
                    <span
                      className={`or-dot ${row.online ? 'or-dot-online' : 'or-dot-offline'}`}
                    />
                    <Text>{v}</Text>
                  </Space>
                ),
              },
              {
                title: t('common.status'),
                key: 'status',
                width: 140,
                render: (_, row) => (
                  <Space size={4}>
                    <Tag color={row.online ? 'green' : 'default'}>
                      {row.online ? t('node.online') : t('node.offline')}
                    </Tag>
                    <Tag color={row.healthy ? 'green' : 'red'}>
                      {row.healthy ? t('deviceGroup.healthy') : t('deviceGroup.unhealthy')}
                    </Tag>
                  </Space>
                ),
              },
              {
                title: t('node.conn'),
                dataIndex: 'current_conn',
                key: 'current_conn',
                width: 90,
                render: (v: number) => <Text className="or-mono">{v}</Text>,
              },
              {
                title: t('node.weight'),
                dataIndex: 'weight',
                key: 'weight',
                width: 80,
                render: (v: number) => <Text className="or-mono">{v}</Text>,
              },
              {
                title: t('deviceGroup.failCount'),
                dataIndex: 'fail_count',
                key: 'fail_count',
                width: 100,
                render: (v: number) =>
                  v > 0 ? <Tag color="red">{v}</Tag> : <Text type="secondary">0</Text>,
              },
              {
                title: t('deviceGroup.loadRatio'),
                dataIndex: 'load_ratio',
                key: 'load_ratio',
                width: 180,
                render: (v: number) => (
                  <Tooltip title={`${Math.round((v ?? 0) * 100)}%`}>
                    <Progress
                      percent={Math.round((v ?? 0) * 100)}
                      size="small"
                      status={v > 0 ? 'active' : 'normal'}
                    />
                  </Tooltip>
                ),
              },
            ]}
            locale={{
              emptyText: <Empty className="or-empty" description={t('deviceGroup.healthEmpty')} />,
            }}
          />
        </Space>
      )}
    </Drawer>
  )
}

/* ───────────────────────── 成员排序 ───────────────────────── */

interface ReorderModalProps {
  group: DeviceGroup | null
  nodeMap: Record<number, Node>
  onClose: () => void
  onSaved: () => void
  t: (key: string, vars?: Record<string, string | number>) => string
}

/**
 * 组内节点顺序调整。
 *
 * 顺序参与负载均衡决策（规格书 6.3「组内优先级」），
 * 因此用上移/下移按钮而非拖拽，避免 1280px 窄屏下拖拽误操作。
 */
function ReorderModal({ group, nodeMap, onClose, onSaved, t }: ReorderModalProps) {
  const [order, setOrder] = useState<number[]>([])
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    setOrder(group?.node_ids ?? [])
  }, [group])

  const move = (index: number, delta: number) => {
    const next = [...order]
    const target = index + delta
    if (target < 0 || target >= next.length) return
    const a = next[index]
    const b = next[target]
    if (a === undefined || b === undefined) return
    next[index] = b
    next[target] = a
    setOrder(next)
  }

  const save = async () => {
    if (!group) return
    setSaving(true)
    try {
      await deviceGroupApi.reorder(group.id, order)
      message.success(t('deviceGroup.reorderSuccess'))
      onSaved()
    } catch (err) {
      showApiError(err, t('deviceGroup.reorderFailed'))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      open={Boolean(group)}
      title={`${t('deviceGroup.reorder')} · ${group?.name ?? ''}`}
      onCancel={onClose}
      onOk={() => void save()}
      confirmLoading={saving}
      okText={t('common.save')}
      cancelText={t('common.cancel')}
      width={560}
    >
      <Paragraph type="secondary" style={{ fontSize: 12 }}>
        {t('deviceGroup.reorderHint')}
      </Paragraph>
      <Space direction="vertical" style={{ width: '100%' }} size={6}>
        {order.map((id, index) => {
          const n = nodeMap[id]
          return (
            <div
              key={id}
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                padding: '6px 10px',
                border: '1px solid var(--or-border)',
                borderRadius: 'var(--or-radius)',
              }}
            >
              <Space>
                <Text className="or-mono" type="secondary" style={{ width: 20 }}>
                  {index + 1}
                </Text>
                <Text>{n?.name ?? `#${id}`}</Text>
                {n && <Tag>{roleText(n.role, t)}</Tag>}
              </Space>
              <Space size={4}>
                <Button
                  size="small"
                  icon={<ArrowUpOutlined />}
                  disabled={index === 0}
                  onClick={() => move(index, -1)}
                />
                <Button
                  size="small"
                  icon={<ArrowDownOutlined />}
                  disabled={index === order.length - 1}
                  onClick={() => move(index, 1)}
                />
              </Space>
            </div>
          )
        })}
      </Space>
    </Modal>
  )
}

/** 角色文案。 */
function roleText(role: Node['role'], t: (k: string) => string): string {
  if (role === 'inbound') return t('node.role.inbound')
  if (role === 'outbound') return t('node.role.outbound')
  return t('node.role.both')
}

/** RFC3339 → 本地可读时间。 */
function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}
