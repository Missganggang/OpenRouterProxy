/**
 * 规则分组页面（规格书 9.2、8.10）。
 *
 * 分组只做「归类 + 排序 + 白名单」三件事，因此页面保持轻量：
 * CRUD + 排序（上移/下移后一次性提交 reorder）。
 */
import { useCallback, useEffect, useState } from 'react'
import {
  Button,
  Card,
  Empty,
  Form,
  Input,
  InputNumber,
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
  PlusOutlined,
  ReloadOutlined,
  SaveOutlined,
} from '@ant-design/icons'

import { ruleApi, showApiError } from '../../api'
import type { RuleGroup } from '../../api/types'
import { useI18n } from '../../locales'

const { Text } = Typography

export default function RuleGroupsPage() {
  const { t } = useI18n()

  const [groups, setGroups] = useState<RuleGroup[]>([])
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<{ name: string; sort: number; remark?: string }>()

  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<RuleGroup | null>(null)
  /** 本地排序草稿：与后端不一致时才显示「保存顺序」。 */
  const [order, setOrder] = useState<number[]>([])
  const [orderDirty, setOrderDirty] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await ruleApi.listGroups({ page: 1, page_size: 300, sort: 'sort', order: 'asc' })
      // 后端 sort 相同时保持稳定顺序：按 sort 再按 id。
      const sorted = [...res.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
      setGroups(sorted)
      setOrder(sorted.map((g) => g.id))
      setOrderDirty(false)
    } catch (err) {
      showApiError(err, t('ruleGroup.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    void load()
  }, [load])

  /** 按本地顺序重排展示。 */
  const ordered = order
    .map((id) => groups.find((g) => g.id === id))
    .filter((g): g is RuleGroup => Boolean(g))

  const openCreate = () => {
    setEditing(null)
    form.setFieldsValue({
      name: '',
      sort: (groups[groups.length - 1]?.sort ?? 0) + 1,
      remark: '',
    })
    setEditorOpen(true)
  }

  const openEdit = (g: RuleGroup) => {
    setEditing(g)
    form.setFieldsValue({ name: g.name, sort: g.sort, remark: g.remark })
    setEditorOpen(true)
  }

  const submit = async () => {
    let values: { name: string; sort: number; remark?: string }
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await ruleApi.updateGroup(editing.id, values)
        message.success(t('ruleGroup.updateSuccess'))
      } else {
        await ruleApi.createGroup(values)
        message.success(t('ruleGroup.createSuccess'))
      }
      setEditorOpen(false)
      setEditing(null)
      await load()
    } catch (err) {
      showApiError(err, t('ruleGroup.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  const remove = (g: RuleGroup) => {
    Modal.confirm({
      title: t('ruleGroup.deleteConfirm', { name: g.name }),
      okText: t('common.delete'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      content: (
        <Space direction="vertical" size={6}>
          <Text>{t('ruleGroup.deleteImpact')}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t('ruleGroup.deleteImpactDetail')}
          </Text>
        </Space>
      ),
      onOk: async () => {
        try {
          await ruleApi.removeGroup(g.id)
          message.success(t('ruleGroup.deleteSuccess'))
          await load()
        } catch (err) {
          showApiError(err, t('ruleGroup.deleteFailed'))
          throw err
        }
      },
    })
  }

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
    setOrderDirty(true)
  }

  /** 一次性提交新的排序：sort 按位次重写为 1..n。 */
  const saveOrder = async () => {
    setSaving(true)
    try {
      await ruleApi.reorderGroups(order)
    } catch (err) {
      showApiError(err, t('ruleGroup.reorderFailed'))
      setSaving(false)
      return
    }
    // reorder 接口只改顺序，这里把 sort 字段也一并刷新以保持一致。
    try {
      await Promise.all(
        order.map((id, index) => ruleApi.updateGroup(id, { sort: index + 1 })),
      )
      message.success(t('ruleGroup.reorderSuccess'))
      await load()
    } catch (err) {
      showApiError(err, t('ruleGroup.reorderFailed'))
    } finally {
      setSaving(false)
    }
  }

  const columns: ColumnsType<RuleGroup> = [
    {
      title: t('ruleGroup.order'),
      key: 'order',
      width: 90,
      render: (_, __, index) => (
        <Space size={2}>
          <Text className="or-mono">{index + 1}</Text>
          <Button
            size="small"
            type="text"
            icon={<ArrowUpOutlined />}
            disabled={index === 0}
            onClick={() => move(index, -1)}
          />
          <Button
            size="small"
            type="text"
            icon={<ArrowDownOutlined />}
            disabled={index === ordered.length - 1}
            onClick={() => move(index, 1)}
          />
        </Space>
      ),
    },
    {
      title: t('common.name'),
      dataIndex: 'name',
      key: 'name',
      width: 220,
      render: (v: string) => <Text strong>{v}</Text>,
    },
    {
      title: t('ruleGroup.sort'),
      dataIndex: 'sort',
      key: 'sort',
      width: 100,
      render: (v: number) => <Tag className="or-mono">{v}</Tag>,
    },
    {
      title: t('common.remark'),
      dataIndex: 'remark',
      key: 'remark',
      render: (v: string) =>
        v ? <Text>{v}</Text> : <Text type="secondary">{t('common.none')}</Text>,
    },
    {
      title: t('common.createdAt'),
      dataIndex: 'created_at',
      key: 'created_at',
      width: 180,
      render: (v: string) => <Text type="secondary">{formatTime(v)}</Text>,
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 130,
      render: (_, row) => (
        <div className="or-actions">
          <Tooltip title={t('common.edit')}>
            <Button size="small" type="text" icon={<EditOutlined />} onClick={() => openEdit(row)} />
          </Tooltip>
          <Tooltip title={t('common.delete')}>
            <Button
              size="small"
              type="text"
              danger
              icon={<DeleteOutlined />}
              onClick={() => remove(row)}
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
          <h2 className="or-page-title">{t('ruleGroup.title')}</h2>
          <p className="or-page-desc">{t('ruleGroup.pageDesc')}</p>
        </div>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => void load()}>
            {t('common.refresh')}
          </Button>
          {orderDirty && (
            <Button
              type="primary"
              icon={<SaveOutlined />}
              loading={saving}
              onClick={() => void saveOrder()}
            >
              {t('ruleGroup.saveOrder')}
            </Button>
          )}
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            {t('ruleGroup.add')}
          </Button>
        </Space>
      </div>

      {orderDirty && (
        <Card size="small" style={{ marginBottom: 12 }}>
          <Text type="warning">{t('ruleGroup.orderDirty')}</Text>
        </Card>
      )}

      <Table<RuleGroup>
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={ordered}
        pagination={{
          pageSize: 20,
          showSizeChanger: true,
          showTotal: (n) => t('common.total', { n }),
        }}
        locale={{
          emptyText: (
            <Empty
              className="or-empty"
              description={
                <Space direction="vertical" size={4}>
                  <Text>{t('ruleGroup.empty')}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {t('ruleGroup.emptyHint')}
                  </Text>
                </Space>
              }
            >
              <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                {t('ruleGroup.add')}
              </Button>
            </Empty>
          ),
        }}
      />

      <Modal
        open={editorOpen}
        title={editing ? `${t('common.edit')} · ${editing.name}` : t('ruleGroup.add')}
        onCancel={() => {
          setEditorOpen(false)
          setEditing(null)
        }}
        onOk={() => void submit()}
        confirmLoading={saving}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        destroyOnClose
      >
        <Form form={form} layout="vertical" autoComplete="off">
          <Form.Item
            name="name"
            label={t('common.name')}
            rules={[{ required: true, message: t('ruleGroup.nameRequired') }]}
          >
            <Input placeholder={t('ruleGroup.namePlaceholder')} />
          </Form.Item>
          <Form.Item
            name="sort"
            label={t('ruleGroup.sort')}
            tooltip={t('ruleGroup.sortHint')}
            rules={[{ required: true, message: t('deviceGroup.required') }]}
          >
            <InputNumber min={0} max={99999} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="remark" label={t('common.remark')}>
            <Input.TextArea rows={2} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

/** RFC3339 → 本地时间。 */
function formatTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
