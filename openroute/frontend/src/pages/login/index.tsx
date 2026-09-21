/**
 * 登录页（规格书 9.2）。
 *
 * 包含：用户名、密码、按需出现的图形验证码、主题切换。
 * 连续登录失败 3 次后，后端会在响应里要求验证码（错误码 40105），
 * 前端据此动态展示验证码输入框与图片。
 */
import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Button, Card, Form, Input, Space, Typography, message } from 'antd'
import { GlobalOutlined, LockOutlined, SafetyOutlined, UserOutlined } from '@ant-design/icons'

import { authApi } from '../../api'
import { ApiError, showApiError } from '../../api/client'
import { useAuthStore } from '../../store/auth'
import { useThemeStore } from '../../store/theme'
import { useI18n } from '../../locales'

const { Title, Text } = Typography

interface FormValues {
  username: string
  password: string
  captcha?: string
}

export default function LoginPage() {
  const [form] = Form.useForm<FormValues>()
  const navigate = useNavigate()
  const location = useLocation()
  const { t } = useI18n()

  const login = useAuthStore((s) => s.login)
  const loggedIn = useAuthStore((s) => s.loggedIn)
  const loading = useAuthStore((s) => s.loading)
  const siteName = useThemeStore((s) => s.site_name)
  const logo = useThemeStore((s) => s.logo)

  const [submitting, setSubmitting] = useState(false)

  // 验证码状态：由后端返回 40105 时置为需要。
  const [needCaptcha, setNeedCaptcha] = useState(false)
  const [captchaId, setCaptchaId] = useState('')
  const [captchaSvg, setCaptchaSvg] = useState('')

  // 已登录用户访问登录页时直接跳走。
  useEffect(() => {
    if (loggedIn) {
      const from = (location.state as { from?: string } | null)?.from
      navigate(from && from !== '/login' ? from : '/dashboard', { replace: true })
    }
  }, [loggedIn, navigate, location.state])

  /** 拉取一张新的验证码。 */
  const loadCaptcha = async () => {
    try {
      const result = await authApi.captcha()
      setCaptchaId(result.captcha_id)
      setCaptchaSvg(result.svg)
      setNeedCaptcha(true)
    } catch {
      // 验证码获取失败时不阻塞登录尝试，用户仍可提交（后端会再次要求）。
      message.warning('验证码加载失败，请点击图片重试')
    }
  }

  const onFinish = async (values: FormValues) => {
    setSubmitting(true)
    try {
      await login(values.username, values.password, values.captcha, captchaId)
      message.success('登录成功')
      const from = (location.state as { from?: string } | null)?.from
      navigate(from && from !== '/login' ? from : '/dashboard', { replace: true })
    } catch (err) {
      // 后端要求验证码：展示验证码输入框并刷新一张图。
      if (err instanceof ApiError && err.code === 40105) {
        await loadCaptcha()
        message.info('连续失败次数较多，请填写验证码')
      } else if (err instanceof ApiError && (err.code === 40106 || err.code === 40104)) {
        // 验证码错误或密码错误：刷新验证码，避免复用已失效的图。
        if (needCaptcha) {
          await loadCaptcha()
          form.setFieldValue('captcha', '')
        }
        showApiError(err)
      } else {
        showApiError(err, '登录失败')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: 24,
      }}
    >
      <Card
        className="or-glass"
        style={{
          width: 380,
          boxShadow: 'var(--or-shadow-lg)',
        }}
        styles={{ body: { padding: 32 } }}
      >
        <Space direction="vertical" size={4} style={{ width: '100%', textAlign: 'center' }}>
          <div style={{ marginBottom: 8 }}>
            {logo ? (
              <img src={logo} alt={siteName} style={{ height: 40 }} />
            ) : (
              <GlobalOutlined style={{ fontSize: 36, color: 'var(--or-primary)' }} />
            )}
          </div>
          <Title level={4} style={{ margin: 0 }}>
            {siteName}
          </Title>
          <Text type="secondary" style={{ fontSize: 13 }}>
            {t('auth.loginTitle')}
          </Text>
        </Space>

        <Form
          form={form}
          layout="vertical"
          onFinish={onFinish}
          autoComplete="off"
          style={{ marginTop: 24 }}
          requiredMark={false}
        >
          <Form.Item
            name="username"
            label={t('auth.username')}
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input
              prefix={<UserOutlined style={{ color: 'var(--or-text-tertiary)' }} />}
              placeholder={t('auth.username')}
              size="large"
              autoFocus
            />
          </Form.Item>

          <Form.Item
            name="password"
            label={t('auth.password')}
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password
              prefix={<LockOutlined style={{ color: 'var(--or-text-tertiary)' }} />}
              placeholder={t('auth.password')}
              size="large"
            />
          </Form.Item>

          {needCaptcha && (
            <Form.Item
              name="captcha"
              label={t('auth.captcha')}
              rules={[{ required: true, message: '请输入验证码' }]}
            >
              <Space.Compact style={{ width: '100%' }}>
                <Input
                  prefix={<SafetyOutlined style={{ color: 'var(--or-text-tertiary)' }} />}
                  placeholder={t('auth.captcha')}
                  size="large"
                  maxLength={8}
                />
                <div
                  onClick={() => void loadCaptcha()}
                  title="点击刷新验证码"
                  style={{
                    cursor: 'pointer',
                    border: '1px solid var(--or-border)',
                    borderLeft: 'none',
                    borderRadius: '0 6px 6px 0',
                    display: 'flex',
                    alignItems: 'center',
                    padding: '0 4px',
                    background: '#f2f4f7',
                  }}
                  // 验证码由后端返回的 SVG 字符串渲染，内容来自本面板自身。
                  dangerouslySetInnerHTML={{ __html: captchaSvg }}
                />
              </Space.Compact>
            </Form.Item>
          )}

          <Form.Item style={{ marginBottom: 0, marginTop: 8 }}>
            <Button
              type="primary"
              htmlType="submit"
              size="large"
              block
              loading={submitting || loading}
            >
              {t('auth.login')}
            </Button>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}
