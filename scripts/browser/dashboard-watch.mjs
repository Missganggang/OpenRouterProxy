/**
 * 仪表盘回归验证：登录后持续观察 10 秒，确认不再白屏。
 *
 * 为什么单独写这个脚本：
 *  1. 之前的 probe-flow 用「模拟表单输入 + 点击提交」登录。
 *     React 受控组件对合成事件很敏感，填入的值不一定进入组件状态，
 *     于是表单没提交、页面停在 /login——那是测试手法的问题，不是产品问题。
 *  2. 旧版故障的时序是「2 秒还在，4 秒变白」，
 *     因此必须跨过 4 秒这个点持续观察，只看首屏是不够的。
 *
 * 这里改用「接口登录拿到会话 Cookie，再直接打开 /dashboard」，
 * 绕开表单模拟，专注验证真正的目标：仪表盘会不会崩。
 */

import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const BASE = process.argv[2] || 'https://x.aarcx.com'
const USER = process.argv[3] || 'admin'
const PASS = process.argv[4]
const PORT = 9226
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'

if (!PASS) {
  console.error('用法: node verify-dashboard.mjs <base> <user> <pass>')
  process.exit(1)
}

async function main() {
  const profile = `${process.env.TEMP}\\or-vd-${Date.now()}`
  const chrome = spawn(CHROME, [
    '--headless=new',
    `--remote-debugging-port=${PORT}`,
    `--user-data-dir=${profile}`,
    '--no-first-run',
    '--disable-gpu',
    '--ignore-certificate-errors',
    'about:blank',
  ])

  let target = null
  for (let i = 0; i < 40; i++) {
    try {
      const list = await fetch(`http://127.0.0.1:${PORT}/json/list`).then((r) => r.json())
      target = list.find((t) => t.type === 'page')
      if (target?.webSocketDebuggerUrl) break
    } catch {}
    await sleep(250)
  }
  if (!target) {
    chrome.kill()
    throw new Error('找不到页面目标')
  }

  const ws = new WebSocket(target.webSocketDebuggerUrl)
  let id = 0
  const pending = new Map()
  const events = []
  ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data)
    if (m.id && pending.has(m.id)) {
      const p = pending.get(m.id)
      pending.delete(m.id)
      m.error ? p.rej(new Error(JSON.stringify(m.error))) : p.res(m.result)
    } else if (m.method) events.push(m)
  })
  await new Promise((r) => ws.addEventListener('open', r))

  const send = (method, params = {}) => {
    const mid = ++id
    ws.send(JSON.stringify({ id: mid, method, params }))
    return new Promise((res, rej) => pending.set(mid, { res, rej }))
  }

  await send('Runtime.enable')
  await send('Log.enable')
  await send('Page.enable')

  // 先打开站点，让页面处于同源上下文，才能在页面里发登录请求（带 Cookie）
  await send('Page.navigate', { url: `${BASE}/login` })
  await sleep(3000)

  const loginResult = await send('Runtime.evaluate', {
    expression: `fetch('${BASE}/api/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({ username: '${USER}', password: '${PASS}' })
    }).then(r => r.json()).then(j => j.code)`,
    awaitPromise: true,
    returnByValue: true,
  })

  if (loginResult.result.value !== 0) {
    console.log('登录失败，code =', loginResult.result.value)
    ws.close()
    chrome.kill()
    process.exit(2)
  }
  console.log('接口登录成功，开始观察仪表盘\n')

  // 打开仪表盘并跨过旧故障点（2s → 4s）持续观察
  events.length = 0
  await send('Page.navigate', { url: `${BASE}/dashboard` })

  const samples = []
  const checkpoints = [1500, 3000, 4500, 6000, 8000, 10000]
  let elapsed = 0
  for (const t of checkpoints) {
    await sleep(t - elapsed)
    elapsed = t
    const r = await send('Runtime.evaluate', {
      expression: `(() => {
        const t = (document.getElementById('root')?.innerText || '').trim();
        return { len: t.length, text: t };
      })()`,
      returnByValue: true,
    })
    const len = r.result.value.len
    samples.push({ t, len })
    console.log(`  t+${(t / 1000).toFixed(1)}s  文本长度=${len}${len < 40 ? '   <-- 空白！' : ''}`)
  }

  // 收集运行时错误
  const errors = []
  for (const ev of events) {
    if (ev.method === 'Runtime.exceptionThrown') {
      errors.push((ev.params.exceptionDetails.exception?.description || ev.params.exceptionDetails.text || '').split('\n')[0])
    }
    if (ev.method === 'Runtime.consoleAPICalled' && ev.params.type === 'error') {
      errors.push(ev.params.args.map((a) => a.description || a.value).join(' ').split('\n')[0])
    }
  }

  // 取一份最终页面文本用于确认内容
  const final = await send('Runtime.evaluate', {
    expression: `document.getElementById('root')?.innerText.trim().slice(0,200) || ''`,
    returnByValue: true,
  })

  ws.close()
  chrome.kill()

  console.log('\n最终页面内容片段:')
  console.log('  ' + (final.result.value || '(空)').split('\n').filter(Boolean).join(' | ').slice(0, 220))

  console.log('\n控制台错误:')
  const uniq = [...new Set(errors)]
  if (uniq.length === 0) {
    console.log('  （无）')
  } else {
    for (const e of uniq.slice(0, 10)) console.log('  ! ' + e.slice(0, 180))
  }

  // 判定：所有采样点都不能空白，且不能出现 undefined 读取类错误
  const blanked = samples.filter((s) => s.len < 40)
  const fatal = uniq.filter((e) => /Cannot read propert|is not a function|Cannot read properties/.test(e))

  console.log()
  if (blanked.length || fatal.length) {
    if (blanked.length) console.log(`❌ 仍有 ${blanked.length} 个采样点为空白: ${blanked.map((b) => 't+' + b.t / 1000 + 's').join(', ')}`)
    if (fatal.length) console.log(`❌ 出现致命错误: ${fatal[0]}`)
    process.exit(1)
  }
  console.log('✅ 仪表盘 10 秒内持续正常渲染，未出现白屏与致命错误')
}

main().catch((e) => {
  console.error('失败:', e)
  process.exit(2)
})
