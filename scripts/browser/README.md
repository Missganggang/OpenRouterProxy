# 浏览器级回归测试

用无头 Chrome 真实渲染页面，检查是否有白屏、控制台报错、字段口径不一致。

**为什么需要它们**：`tsc` 只能校验类型。像「把统计周期对象当数字渲染」这类问题，
编译期完全合法，只有在浏览器里跑起来才会暴露——
线上曾经因此出现过「登录后页面闪一下就全白」的故障，这些脚本就是那次故障的产物。

前置：本机需装 Chrome（默认路径 `C:\Program Files\Google\Chrome\Application\chrome.exe`）。
脚本通过 CDP（Chrome DevTools Protocol）驱动，无需额外 npm 依赖。

## 脚本

### `smoke.mjs` —— 全站页面冒烟

逐个打开所有页面，报告文本长度与控制台错误。
用来回答「有没有哪个页面是白屏的」。

```bash
node scripts/browser/smoke.mjs https://x.aarcx.com admin '密码'
```

输出示例：

```
页面                  文本长度      结果
──────────────────────────────────────────────
/dashboard          204       OK
/nodes              271       OK
...
全部 13 个页面正常
```

### `dashboard-watch.mjs` —— 仪表盘持续观察（关键回归）

登录后打开 `/dashboard`，在 1.5s ~ 10s 之间多次采样，
确认页面**不会在渲染后崩掉**。

```bash
node scripts/browser/dashboard-watch.mjs https://x.aarcx.com admin '密码'
```

**为什么必须跨过 4 秒**：旧版故障的时序是「2 秒时还在、4 秒时变白」
（数据到达后触发渲染异常）。只看首屏会漏掉这类问题，所以这个脚本刻意多次采样。

### `dashboard-assert.mjs` —— 仪表盘内容断言

比 `dashboard-watch` 更严格：断言页面里不出现 `NaN`、`undefined`、`[object Object]`，
并且四张流量卡片都渲染出了格式化后的字节值。

```bash
node scripts/browser/dashboard-assert.mjs https://x.aarcx.com '密码'
```

## 使用建议

- **改完前端类型定义后**：先跑 `dashboard-assert`（最容易踩字段口径的页面），再跑 `smoke`。
- **上线后**：跑 `smoke` 确认全站可用。
- 三个脚本都用无头模式，退出码 0 表示通过，可直接接进 CI。

## 已知限制

- 通过 CDP 模拟表单输入对 React 受控组件不可靠
  （填入的值可能进不了组件状态，导致表单不提交）。
  因此脚本统一改用**接口登录拿会话 Cookie**，再直接访问目标页面，
  绕开表单模拟，专注验证渲染。
- 依赖 Chrome 的安装路径；换机器需调整脚本顶部的 `CHROME` 常量。
