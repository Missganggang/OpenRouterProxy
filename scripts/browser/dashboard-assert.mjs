/**
 * 仪表盘详情核对。
 *
 * 冒烟测试只能判断「没有白屏」，这里进一步断言关键内容确实渲染出来了：
 * 流量卡片显示的是格式化后的字节（如 "0 B"），而不是 "NaN" / "undefined"。
 *
 * 这正是本次故障的表现：overview.today 是对象却被当数字用，
 * formatBytes 输出 NaN，React 随后崩掉整棵树。
 */

import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const BASE = process.argv[2]
const PASS = process.argv[3]
const PORT = 9223
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'

async function main() {
  const profile = `${process.env.TEMP}\\or-detail-${Date.now()}`
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
  await send('Page.enable')

  // 先登录，拿到会话
  await send('Page.navigate', { url: `${BASE}/login` })
  await sleep(2500)
  await send('Runtime.evaluate', {
    expression: `fetch('${BASE}/api/v1/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'include',body:JSON.stringify({username:'admin',password:'${PASS}'})}).then(r=>r.json())`,
    awaitPromise: true,
    returnByValue: true,
  })

  // 打开仪表盘并等待数据
  events.length = 0
  await send('Page.navigate', { url: `${BASE}/dashboard` })
  await sleep(4000)

  const res = await send('Runtime.evaluate', {
    expression: `(() => {
      const text = document.body.innerText;
      return {
        // 关键断言：不应出现 NaN / undefined / [object Object]
        hasNaN: /NaN/.test(text),
        hasUndefined: /undefined/.test(text),
        hasObjectObject: /\\[object Object\\]/.test(text),
        // 流量卡片应出现 "0 B" 之类的格式化值
        hasByteUnit: /\\d+(\\.\\d+)?\\s*(B|KB|MB|GB|TB)/.test(text),
        hasErrorBoundary: text.includes('页面渲染出错'),
        cardTitles: ['今日','昨日','本月','累计'].filter(k => text.includes(k)),
        // 把「卡片标题 → 紧随其后的值」抓出来，确认是 0 B / 1.5 MB 这类值
        cardValues: (() => {
          const out = [];
          for (const k of ['今日','昨日','本月','累计']) {
            const i = text.indexOf(k);
            if (i >= 0) out.push(k + ' = ' + text.slice(i + k.length, i + k.length + 24).split('\\n').filter(Boolean)[0]);
          }
          return out;
        })(),
        sample: text.split('\\n').filter(Boolean).slice(0, 14)
      };
    })()`,
    returnByValue: true,
  })

  const info = res.result.value
  const failures = []

  if (info.hasErrorBoundary) failures.push('命中了渲染错误边界')
  if (info.hasNaN) failures.push('页面出现 NaN')
  if (info.hasUndefined) failures.push('页面出现 undefined')
  if (info.hasObjectObject) failures.push('页面出现 [object Object]（对象被直接渲染）')
  if (!info.hasByteUnit) failures.push('流量卡片没有渲染出格式化字节值')
  if (info.cardTitles.length < 4) {
    failures.push(`流量卡片标题不全，只有: ${info.cardTitles.join(',')}`)
  }

  console.log('流量卡片取值:')
  for (const line of info.cardValues ?? []) console.log('    ' + line)
  console.log()

  ws.close()
  chrome.kill()

  if (failures.length) {
    console.log('❌ 断言失败:')
    for (const f of failures) console.log('   - ' + f)
    process.exit(1)
  }
  console.log('✅ 仪表盘渲染正确：无 NaN / undefined，流量卡片齐全')
}

main().catch((e) => {
  console.error('失败:', e)
  process.exit(2)
})
