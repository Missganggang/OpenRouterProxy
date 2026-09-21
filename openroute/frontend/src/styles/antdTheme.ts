/**
 * Ant Design 主题配置。
 *
 * 把 CSS 变量里的品牌色同步给 antd，
 * 保证「自定义组件」与「antd 组件」的视觉一致。
 * 切换主题时返回不同的 token，由 ConfigProvider 触发重渲染。
 */
import type { ThemeConfig } from 'antd'
import { theme as antdThemeAlgo } from 'antd'

import type { ThemeName } from '../store/theme'

/** 品牌主色，与 variables.css 中的 --or-primary 保持一致。 */
const PRIMARY = '#165dff'

/** 语义色，与 variables.css 保持一致。 */
const SUCCESS = '#00b42a'
const WARNING = '#ff7d00'
const ERROR = '#f53f3f'

/**
 * 按主题名生成 antd 主题配置。
 *
 * 透明主题下背景设为透明，让 CSS 变量控制的毛玻璃层透出来。
 */
export function antdTheme(name: ThemeName): ThemeConfig {
  const transparent = name === 'transparent'

  return {
    algorithm: antdThemeAlgo.defaultAlgorithm,
    token: {
      colorPrimary: PRIMARY,
      colorSuccess: SUCCESS,
      colorWarning: WARNING,
      colorError: ERROR,
      colorInfo: PRIMARY,
      borderRadius: 6,
      fontSize: 14,
      // 紧凑一些，管理后台一屏能看更多行。
      controlHeight: 32,
      wireframe: false,
      fontFamily:
        '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", ' +
        '"Hiragino Sans GB", "Microsoft YaHei", "Helvetica Neue", Helvetica, Arial, sans-serif',
    },
    components: {
      Layout: {
        headerBg: transparent ? 'rgba(255,255,255,0.72)' : '#ffffff',
        siderBg: transparent ? 'rgba(255,255,255,0.72)' : '#ffffff',
        bodyBg: transparent ? 'transparent' : '#f7f8fa',
        headerHeight: 56,
        headerPadding: 0,
      },
      Menu: {
        itemBg: 'transparent',
        subMenuItemBg: 'transparent',
        // 选中项用「浅蓝底 + 主色文字」，比整块反白更轻。
        itemSelectedBg: 'rgba(22, 93, 255, 0.10)',
        itemSelectedColor: PRIMARY,
        itemMarginInline: 8,
        itemBorderRadius: 6,
      },
      Card: {
        // 透明主题下卡片自身透明，由 CSS 变量控制毛玻璃。
        colorBgContainer: transparent ? 'rgba(255,255,255,0.78)' : '#ffffff',
      },
      Table: {
        headerBg: transparent ? 'rgba(247,248,250,0.7)' : '#f7f8fa',
        // 表头字重略重，提升可读性。
        headerSplitColor: 'transparent',
        rowHoverBg: transparent ? 'rgba(22,93,255,0.04)' : '#f7f8fa',
      },
      Statistic: {
        contentFontSize: 24,
      },
      Modal: {
        contentBg: transparent ? 'rgba(255,255,255,0.96)' : '#ffffff',
      },
      Drawer: {
        colorBgElevated: transparent ? 'rgba(255,255,255,0.96)' : '#ffffff',
      },
    },
  }
}
