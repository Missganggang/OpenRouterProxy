import { useEffect, useState } from 'react'
import { Button, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Table, message } from 'antd'
import { ruleApi, showApiError, userApi } from '../../api'
import type { RuleGroup, UserGroup } from '../../api/types'

export default function UserGroups({ groups, onChanged }: { groups: UserGroup[]; onChanged: () => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<UserGroup | null>(null)
  const [editorOpen, setEditorOpen] = useState(false)
  const [saving, setSaving] = useState(false)
  const [ruleGroups, setRuleGroups] = useState<RuleGroup[]>([])
  const [form] = Form.useForm<Partial<UserGroup>>()
  useEffect(() => {
    if (open) void ruleApi.listGroups().then((r) => setRuleGroups(r.items)).catch(showApiError)
  }, [open])
  const edit = (group: UserGroup | null) => {
    setEditing(group)
    form.resetFields()
    form.setFieldsValue(group ?? { traffic_limit: 0, speed_limit: 0, ip_limit: 0, conn_limit: 0, rule_group_ids: [] })
    setEditorOpen(true)
  }
  const save = async () => {
    const values = await form.validateFields()
    setSaving(true)
    try {
      if (editing) await userApi.updateGroup(editing.id, values)
      else await userApi.createGroup(values)
      await onChanged()
      setEditorOpen(false)
      message.success('用户分组已保存')
    } catch (error) { showApiError(error) }
    finally { setSaving(false) }
  }
  return <>
    <Button onClick={() => setOpen(true)}>管理用户分组</Button>
    <Modal title="用户分组" open={open} onCancel={() => setOpen(false)} footer={null} width={780}>
      <Button type="primary" onClick={() => edit(null)} style={{ marginBottom: 12 }}>创建分组</Button>
      <Table<UserGroup> rowKey="id" dataSource={groups} size="small" columns={[
        { title: '名称', dataIndex: 'name' },
        { title: '备注', dataIndex: 'remark' },
        { title: '操作', key: 'actions', render: (_, group) => <Space>
          <Button onClick={() => edit(group)}>编辑</Button>
          <Popconfirm title={`删除分组 ${group.name}？`} description="分组下用户的归属将由服务器校验。" onConfirm={async () => {
            try { await userApi.removeGroup(group.id); await onChanged() } catch (error) { showApiError(error) }
          }}><Button danger>删除</Button></Popconfirm>
        </Space> },
      ]} />
    </Modal>
    <Modal title={editing ? '编辑用户分组' : '创建用户分组'} open={editorOpen} onCancel={() => setEditorOpen(false)} onOk={() => void save()} confirmLoading={saving}>
      <Form form={form} layout="vertical">
        <Form.Item name="name" label="名称" rules={[{ required: true, whitespace: true }]}><Input maxLength={64} /></Form.Item>
        <Form.Item name="traffic_limit" label="流量上限（字节，0 为不限）"><InputNumber min={0} precision={0} style={{ width: '100%' }} /></Form.Item>
        <Form.Item name="speed_limit" label="速度上限（KB/s，0 为不限）"><InputNumber min={0} precision={0} style={{ width: '100%' }} /></Form.Item>
        <Form.Item name="ip_limit" label="IP 数上限（0 为不限）"><InputNumber min={0} precision={0} /></Form.Item>
        <Form.Item name="conn_limit" label="连接数上限（0 为不限）"><InputNumber min={0} precision={0} /></Form.Item>
        <Form.Item name="rule_group_ids" label="可用规则分组"><Select mode="multiple" options={ruleGroups.map((r) => ({label: r.name, value: r.id}))} /></Form.Item>
        <Form.Item name="remark" label="备注"><Input.TextArea /></Form.Item>
      </Form>
    </Modal>
  </>
}
