// Throwaway feasibility probe: headless Chrome over CDP with Node's built-in
// WebSocket, no npm packages. Deleted once the real harness exists.
import { spawn } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'
const profile = mkdtempSync(join(tmpdir(), 'awg-cdp-probe-'))
const port = 9333

const child = spawn(CHROME, [
  '--headless=new',
  `--remote-debugging-port=${port}`,
  `--user-data-dir=${profile}`,
  '--no-first-run',
  '--no-default-browser-check',
  '--disable-gpu',
  '--hide-scrollbars',
  '--force-device-scale-factor=1',
  '--force-color-profile=srgb',
  '--window-size=1440,900',
  'about:blank',
], { stdio: 'ignore' })

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function findTarget() {
  for (let i = 0; i < 100; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()
      const page = list.find((t) => t.type === 'page')
      if (page?.webSocketDebuggerUrl) return page.webSocketDebuggerUrl
    } catch {
      /* not up yet */
    }
    await sleep(100)
  }
  throw new Error('CDP endpoint never came up')
}

class Cdp {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)
        this.pending.delete(msg.id)
        msg.error ? reject(new Error(`${msg.error.message} (${msg.error.code})`)) : resolve(msg.result)
      }
    })
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
    })
  }
}

const url = await findTarget()
const ws = new WebSocket(url)
await new Promise((res, rej) => {
  ws.addEventListener('open', res, { once: true })
  ws.addEventListener('error', rej, { once: true })
})
const cdp = new Cdp(ws)

await cdp.send('Page.enable')
await cdp.send('Network.enable')
await cdp.send('Emulation.setDeviceMetricsOverride', {
  width: 1440, height: 900, deviceScaleFactor: 1, mobile: false,
})

// Give the page the admin cookie the tray would have minted.
await cdp.send('Network.setCookie', {
  name: 'dsh_admin_session',
  value: process.argv[2] || '',
  domain: '127.0.0.1',
  path: '/',
  httpOnly: true,
})

await cdp.send('Page.navigate', { url: 'http://127.0.0.1:7865/logs' })
await sleep(2500)

const shot = await cdp.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false })
const out = join(process.cwd(), '_probe-logs.png')
writeFileSync(out, Buffer.from(shot.data, 'base64'))
const title = await cdp.send('Runtime.evaluate', { expression: 'document.title', returnByValue: true })
console.log(JSON.stringify({ ok: true, bytes: Buffer.from(shot.data, 'base64').length, title: title.result.value, out }))

ws.close()
child.kill()
await sleep(300)
try { rmSync(profile, { recursive: true, force: true }) } catch { /* chrome may hold a lock briefly */ }
