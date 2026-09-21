/**
 * 语言切换控件（规格书 9.3：默认 zh-CN，可切 en-US）。
 */
import { Segmented } from 'antd'

import { useI18n, type Lang } from '../locales'

interface Props {
  size?: 'small' | 'middle' | 'large'
}

export default function LanguageSwitch({ size = 'small' }: Props) {
  const lang = useI18n((s) => s.lang)
  const setLang = useI18n((s) => s.setLang)

  return (
    <Segmented
      size={size}
      value={lang}
      onChange={(v) => setLang(v as Lang)}
      options={[
        { label: '中文', value: 'zh-CN' },
        { label: 'EN', value: 'en-US' },
      ]}
    />
  )
}
