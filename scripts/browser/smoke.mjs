/**
 * 前端页面冒烟测试。
 *
 * 用无头 Chrome 逐个打开面板页面，收集：
 *   - 控制台错误 / 未捕获异常
 *   - 页面文本长度（判断是否白屏）
 *   - 是否出现我们的「页面渲染出错」错误边界
 *
 * 之所以要真跑浏览器：TypeScript 只能校验类型，
 * 像「把对象当数字渲染」这类问题编译期是发现不了的（上一版就是这么白屏的）。
 *
 * 用法：
 *   node smoke.mjs <base-url> <username> <password>
 */

import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const BASE = process.argv[2] || 'http://127.0.0.1:18888'
const USER = process.argv[3] || 'admin'
const PASS = process.argv[4]
const PORT = 9222

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'

/** 需要逐个访问的页面路径。 */
const PAGES = [
  '/dashboard',
  '/nodes',
  '/node-groups',
  '/device-groups',
  '/forward-rules',
  '/rule-groups',
  '/users',
  '/traffic',
  '/monitor',
  '/alerts',
  '/snapshots',
  '/migration',
  '/settings',
]

/** 通过 CDP 与浏览器交互的最小客户端。 */
async function cdp(wsUrl) {
  const { WebSocket } = await import('node:http').then(() => ({ WebSocket: globalThis.WebSocket }))
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl)
    let id = 0
    const pending = new Map()
    const events = []

    ws.addEventListener('open', () =>
      resolve({
        send(method, params = {}) {
          const msgId = ++id
          ws.send(JSON.stringify({ id: msgId, method, params }))
          return new Promise((res, rej) => pending.set(msgId, { res, rej }))
        },
        events,
        close: () => ws.close(),
      }),
    )
    ws.addEventListener('error', reject)
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id && pending.has(msg.id)) {
        const { res, rej } = pending.get(msg.id)
        pending.delete(msg.id)
        msg.error ? rej(new Error(JSON.stringify(msg.error))) : res(msg.result)
      } else if (msg.method) {
        events.push(msg)
      }
    })
  })
}

async function main() {
  if (!PASS) {
    console.error('用法: node smoke.mjs <base-url> <user> <password>')
    process.exit(1)
  }

  const profile = `${process.env.TEMP}\\or-smoke-${Date.now()}`
  const chrome = spawn(CHROME, [
    '--headless=new',
    `--remote-debugging-port=${PORT}`,
    `--user-data-dir=${profile}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-gpu',
    '--ignore-certificate-errors',
    'about:blank',
  ])

  // 等 Chrome 的调试端口就绪
  let version = null
  for (let i = 0; i < 40; i++) {
    try {
      version = await fetch(`http://127.0.0.1:${PORT}/json/version`).then((r) => r.json())
      break
    } catch {
      await sleep(250)
    }
  }
  if (!version) {
    chrome.kill()
    throw new Error('Chrome 调试端口未就绪')
  }

  // 必须连到「页面」目标，而不是浏览器级端点：
  // Runtime / Page / Log 这些域只在 page target 上可用。
  let target = null
  for (let i = 0; i < 40; i++) {
    const list = await fetch(`http://127.0.0.1:${PORT}/json/list`).then((r) => r.json())
    target = list.find((t) => t.type === 'page')
    if (target?.webSocketDebuggerUrl) break
    await sleep(250)
  }
  if (!target?.webSocketDebuggerUrl) {
    chrome.kill()
    throw new Error('找不到可用的页面目标')
  }

  const client = await cdp(target.webSocketDebuggerUrl)
  await client.send('Runtime.enable')
  await client.send('Log.enable')
  await client.send('Page.enable')

  // ── 1. 登录拿 token（直接打接口，避免在页面里模拟表单）──
  const loginRes = await fetch(`${BASE}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: USER, password: PASS }),
  }).then((r) => r.json())

  if (loginRes.code !== 0) {
    chrome.kill()
    throw new Error('登录失败: ' + loginRes.message)
  }
  const token = loginRes.data.access_token
  console.log(`登录成功，准备遍历 ${PAGES.length} 个页面\n`)

  // ── 2. 把 token 与站点信息写进 localStorage / 内存 ──
  // 前端把 access_token 放在内存里，刷新会丢；因此改为先用页面登录一次。
  await client.send('Page.navigate', { url: `${BASE}/login` })
  await sleep(2500)

  await client.send('Runtime.evaluate', {
    expression: `
      (async () => {
        const r = await fetch('${BASE}/api/v1/auth/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({ username: '${USER}', password: '${PASS}' })
        });
        return (await r.json()).code;
      })()
    `,
    awaitPromise: true,
    returnByValue: true,
  })

  const results = []

  for (const path of PAGES) {
    client.events.length = 0

    await client.send('Page.navigate', { url: `${BASE}${path}` })
    // 等首屏渲染 + 接口返回
    await sleep(3200)

    const evaluated = await client.send('Runtime.evaluate', {
      expression: `(() => {
        const root = document.getElementById('root');
        const text = (root?.innerText || '').trim();
        return {
          url: location.pathname,
          textLen: text.length,
          hasErrorBoundary: text.includes('页面渲染出错'),
          bodyLen: document.body.innerText.trim().length,
          snippet: text.slice(0, 120)
        };
      })()`,
      returnByValue: true,
    })

    const info = evaluated.result.value

    // 收集控制台里的错误
    const errors = []
    for (const ev of client.events) {
      if (ev.method === 'Runtime.exceptionThrown') {
        const d = ev.params.exceptionDetails
        errors.push(d.exception?.description || d.text)
      }
      if (ev.method === 'Runtime.consoleAPICalled' && ev.params.type === 'error') {
        errors.push(ev.params.args.map((a) => a.description || a.value).join(' '))
      }
    }

    // 空白判定：正文几乎没有内容，或命中错误边界
    const blank = info.textLen < 40
    results.push({ path, ...info, errors, blank })
  }

  client.close()
  chrome.kill()

  // ── 3. 汇总 ──
  console.log('页面'.padEnd(20) + '文本长度'.padEnd(10) + '结果')
  console.log('─'.repeat(70))
  let failed = 0
  for (const r of results) {
    const bad = r.blank || r.hasErrorBoundary || r.errors.length > 0
    if (bad) failed++
    const status = r.hasErrorBoundary
      ? '渲染错误（命中错误边界）'
      : r.blank
        ? '空白页面'
        : r.errors.length
          ? '控制台报错'
          : 'OK'
    console.log(r.path.padEnd(20) + String(r.textLen).padEnd(10) + status)
    if (r.errors.length) {
      for (const e of r.errors.slice(0, 4)) {
        console.log('    ! ' + String(e).split('\n')[0].slice(0, 160))
      }
    }
  }

  console.log('─'.repeat(70))
  console.log(failed === 0 ? `全部 ${results.length} 个页面正常` : `${failed} / ${results.length} 个页面有问题`)
  process.exit(failed === 0 ? 0 : 1)
}

main().catch((e) => {
  console.error('冒烟测试失败:', e)
  process.exit(2)
})
