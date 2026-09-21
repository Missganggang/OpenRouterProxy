/**
 * 主题切换控件（规格书 6.16：经典 / 透明）。
 *
 * 切换即时生效，不需刷新页面——只改 <html> 上的 class。
 */
import { Segmented, Tooltip } from 'antd'
import { BgColorsOutlined, BorderOutlined } from '@ant-design/icons'

import { useThemeStore, type ThemeName } from '../store/theme'
import { useI18n } from '../locales'

interface Props {
  /** 紧凑模式：只显示图标，用于顶栏。 */
  compact?: boolean
}

export default function ThemeSwitch({ compact = false }: Props) {
  const theme = useThemeStore((s) => s.theme)
  const setTheme = useThemeStore((s) => s.setTheme)
  const { t } = useI18n()

  if (compact) {
    return (
      <Tooltip title={theme === 'classic' ? t('settings.theme.transparent') : t('settings.theme.classic')}>
        <span
          onClick={() => void setTheme(theme === 'classic' ? 'transparent' : 'classic')}
          style={{ cursor: 'pointer', padding: '0 4px' }}
        >
          <BgColorsOutlined />
        </span>
      </Tooltip>
    )
  }

  return (
    <Segmented
      size="small"
      value={theme}
      onChange={(v) => void setTheme(v as ThemeName)}
      options={[
        {
          label: t('settings.theme.classic'),
          value: 'classic',
          icon: <BorderOutlined />,
        },
        {
          label: t('settings.theme.transparent'),
          value: 'transparent',
          icon: <BgColorsOutlined />,
        },
      ]}
    />
  )
}
