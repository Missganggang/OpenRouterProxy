/**
 * 检查本机到 GitHub 的可用传输通道。
 *
 * 背景：本机 `curl https://github.com` 返回 HTTP 000（完全连不上），
 * 而 git push 需要一条能通的通道。GitHub 支持多种传输方式，
 * 逐一探测后挑可用的那条，避免在已知不通的路上反复重试。
 *
 * 检查项：
 *   1. HTTPS 443（git 走 https://github.com/... 时用）
 *   2. SSH 22（git@github.com:... 默认端口）
 *   3. SSH 443（ssh.github.com:443，22 被封时的备用通道）
 *   4. DNS 解析
 *   5. 系统代理环境变量
 */

import net from 'node:net'
import dns from 'node:dns/promises'

const results = []

/** 测一个 TCP 端口能否连通。 */
function tcpCheck(host, port, timeout = 8000) {
  return new Promise((resolve) => {
    const started = Date.now()
    const sock = new net.Socket()
    let done = false
    const finish = (ok, note) => {
      if (done) return
      done = true
      sock.destroy()
      resolve({ host, port, ok, ms: Date.now() - started, note })
    }
    sock.setTimeout(timeout)
    sock.once('connect', () => finish(true))
    sock.once('timeout', () => finish(false, '超时'))
    sock.once('error', (e) => finish(false, e.code || e.message))
    sock.connect(port, host)
  })
}

async function main() {
  console.log('=== GitHub 传输通道探测 ===\n')

  // DNS
  for (const host of ['github.com', 'api.github.com', 'ssh.github.com']) {
    try {
      const addrs = await dns.lookup(host, { all: true })
      results.push({ host, port: '-', ok: true, ms: 0, note: 'DNS: ' + addrs.map((a) => a.address).join(', ') })
    } catch (e) {
      results.push({ host, port: '-', ok: false, ms: 0, note: 'DNS 解析失败: ' + e.code })
    }
  }

  // TCP 通道
  results.push(await tcpCheck('github.com', 443))
  results.push(await tcpCheck('api.github.com', 443))
  results.push(await tcpCheck('github.com', 22))
  results.push(await tcpCheck('ssh.github.com', 443))
  results.push(await tcpCheck('ssh.github.com', 22))

  console.log('目标'.padEnd(34) + '结果'.padEnd(10) + '耗时/说明')
  console.log('─'.repeat(78))
  for (const r of results) {
    const label = `${r.host}:${r.port}`
    const status = r.ok ? '连通' : '不通'
    const extra = r.ok ? (r.ms ? r.ms + 'ms' : '') : r.note
    console.log(label.padEnd(34) + status.padEnd(10) + extra)
  }

  // 代理
  console.log('\n=== 代理环境变量 ===')
  const proxyVars = ['HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY', 'http_proxy', 'https_proxy', 'all_proxy']
  let anyProxy = false
  for (const v of proxyVars) {
    if (process.env[v]) {
      console.log(`  ${v} = ${process.env[v]}`)
      anyProxy = true
    }
  }
  if (!anyProxy) console.log('  （未设置任何代理环境变量）')

  console.log('\n=== 结论 ===')
  const https = results.find((r) => r.host === 'github.com' && r.port === 443)
  const ssh22 = results.find((r) => r.host === 'github.com' && r.port === 22)
  const ssh443 = results.find((r) => r.host === 'ssh.github.com' && r.port === 443)

  if (https?.ok) {
    console.log('  HTTPS 443 可用 → git 用 https://github.com/... 推送')
  } else if (ssh443?.ok) {
    console.log('  HTTPS 不通，但 ssh.github.com:443 可用 → 用 SSH over 443')
    console.log('  需要在 ~/.ssh/config 里配置 Host github.com / Port 443 / HostName ssh.github.com')
  } else if (ssh22?.ok) {
    console.log('  SSH 22 可用 → git 用 git@github.com:... 推送')
  } else {
    console.log('  所有常用通道都不通：需要配置代理或使用其他网络环境')
  }
}

main().catch((e) => {
  console.error('探测失败:', e)
  process.exit(1)
})
