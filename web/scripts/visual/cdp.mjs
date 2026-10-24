/**
 * Chrome DevTools Protocol client with zero npm dependencies.
 *
 * The visual gate must run on a machine that already has Chrome or Edge — this
 * repo's host is a Windows tray app, and pulling a second browser just for
 * screenshots is not acceptable. Node 24 ships a global WebSocket, and that is
 * the entire transport: no playwright, no puppeteer, no chromedriver.
 *
 * A private --user-data-dir keeps the run off the user's real profile (Chrome
 * refuses remote debugging on the default profile anyway) and makes every run
 * start from the same empty localStorage.
 */

import { spawn } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const CANDIDATES = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  process.env.LOCALAPPDATA && join(process.env.LOCALAPPDATA, 'Google\\Chrome\\Application\\chrome.exe'),
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  process.env.LOCALAPPDATA && join(process.env.LOCALAPPDATA, 'Microsoft\\Edge\\Application\\msedge.exe'),
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].filter(Boolean)

export function findBrowser() {
  if (process.env.AWG_BROWSER) {
    if (!existsSync(process.env.AWG_BROWSER)) {
      throw new Error(`AWG_BROWSER points at a missing file: ${process.env.AWG_BROWSER}`)
    }
    return process.env.AWG_BROWSER
  }
  const found = CANDIDATES.find((path) => existsSync(path))
  if (!found) {
    throw new Error('no Chrome/Edge found. Install one, or set AWG_BROWSER to the executable path.')
  }
  return found
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

export class CdpConnection {
  constructor(ws) {
    this.ws = ws
    this.nextId = 0
    this.pending = new Map()
    this.listeners = new Map()
    ws.addEventListener('message', (event) => {
      const message = JSON.parse(event.data)
      if (message.id !== undefined) {
        const entry = this.pending.get(message.id)
        if (!entry) return
        this.pending.delete(message.id)
        if (message.error) {
          entry.reject(new Error(`${entry.method}: ${message.error.message} (code ${message.error.code})`))
        } else {
          entry.resolve(message.result)
        }
        return
      }
      const handlers = this.listeners.get(message.method)
      if (handlers) for (const handler of handlers) handler(message.params)
    })
    ws.addEventListener('close', () => {
      for (const entry of this.pending.values()) {
        entry.reject(new Error(`connection closed while waiting for ${entry.method}`))
      }
      this.pending.clear()
    })
  }

  on(method, handler) {
    if (!this.listeners.has(method)) this.listeners.set(method, new Set())
    this.listeners.get(method).add(handler)
    return () => this.listeners.get(method)?.delete(handler)
  }

  send(method, params = {}) {
    if (this.ws.readyState !== 1) {
      return Promise.reject(new Error(`cannot send ${method}: socket is not open`))
    }
    const id = ++this.nextId
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, method })
      this.ws.send(JSON.stringify({ id, method, params }))
    })
  }

  /** Runtime.evaluate by value; throws with the page's own error text on failure. */
  async evaluate(expression) {
    const result = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true,
    })
    if (result.exceptionDetails) {
      const detail = result.exceptionDetails.exception?.description ?? result.exceptionDetails.text
      throw new Error(`page evaluation failed: ${detail}`)
    }
    return result.result.value
  }

  close() {
    try {
      this.ws.close()
    } catch {
      /* already gone */
    }
  }
}

/**
 * Launches a headless browser and opens one page target.
 *
 * Every flag here exists to make the pixels reproducible: fixed window and
 * device scale, sRGB colour, no LCD subpixel antialiasing (that varies with the
 * monitor's subpixel layout and differs between machines), no scrollbars, no
 * first-run UI.
 */
export async function launchBrowser({ width, height, port } = {}) {
  const browser = findBrowser()
  const profile = mkdtempSync(join(tmpdir(), 'awg-visual-'))
  const debugPort = port ?? 9400 + (process.pid % 400)

  const child = spawn(
    browser,
    [
      '--headless=new',
      `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${profile}`,
      '--no-first-run',
      '--no-default-browser-check',
      '--no-sandbox',
      '--disable-gpu',
      '--disable-extensions',
      '--disable-background-networking',
      '--disable-backgrounding-occluded-windows',
      '--disable-renderer-backgrounding',
      '--hide-scrollbars',
      '--force-color-profile=srgb',
      '--force-device-scale-factor=1',
      '--font-render-hinting=none',
      '--disable-lcd-text',
      '--disable-font-subpixel-positioning',
      '--disable-partial-raster',
      '--disable-skia-runtime-opts',
      `--window-size=${width},${height}`,
      'about:blank',
    ],
    { stdio: 'ignore', windowsHide: true },
  )

  let exitNote = null
  child.on('exit', (code, signal) => {
    exitNote = `browser exited early (code ${code}, signal ${signal})`
  })

  // Wait for the HTTP debug endpoint, then create a dedicated page target and
  // connect to it directly: a page-level socket takes page-scoped commands
  // without the session forwarding a browser-level socket would need.
  //
  // A parent process exiting is NOT by itself fatal: Edge (and Chrome's
  // launcher on some builds) hands off to a child and the spawned process
  // returns 0 immediately while the browser keeps running. So keep polling and
  // only treat the exit as the cause of failure if no endpoint ever appears.
  let target = null
  for (let attempt = 0; attempt < 200; attempt++) {
    try {
      const created = await fetch(`http://127.0.0.1:${debugPort}/json/new?about:blank`, { method: 'PUT' })
      if (created.ok) {
        const info = await created.json()
        if (info.webSocketDebuggerUrl) {
          target = info
          break
        }
      }
    } catch {
      /* endpoint not listening yet */
    }
    await sleep(100)
  }
  if (!target) {
    child.kill()
    throw new Error(
      `browser never exposed a CDP page target on port ${debugPort}${exitNote ? ` (${exitNote})` : ''}`,
    )
  }

  const ws = new WebSocket(target.webSocketDebuggerUrl)
  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve, { once: true })
    ws.addEventListener('error', () => reject(new Error('CDP websocket failed to open')), { once: true })
  })
  const cdp = new CdpConnection(ws)

  const kill = async () => {
    cdp.close()
    child.kill()
    await sleep(250)
    try {
      rmSync(profile, { recursive: true, force: true })
    } catch {
      /* Chrome may still hold a lock briefly; a leaked temp dir is harmless */
    }
  }

  return { cdp, browser, profile, targetId: target.id, kill }
}
