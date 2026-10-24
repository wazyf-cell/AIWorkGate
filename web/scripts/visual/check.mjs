/**
 * Visual regression gate: capture the 12 admin pages in three skins (light,
 * dark, narrow), plus a handful of interaction states (drawer open, scheduler
 * explanation, raw log line) and two forced-failure pages, then diff against
 * baselines. Stateful captures name their skin list; overlays capture the
 * viewport instead of the whole page.
 *
 * Run modes
 *   node scripts/visual/check.mjs                 # compare, exit 1 on diff
 *   node scripts/visual/check.mjs --update         # rewrite baselines
 *   node scripts/visual/check.mjs --only logs      # one page, all three skins
 *   node scripts/visual/check.mjs --report-dir D   # where diffs land
 *
 * What makes it trustworthy rather than merely green:
 *
 *  - It runs the SAME bundle the host embeds (internal/webui/dist), so passing
 *    here is evidence about what ships, not about a second copy.
 *  - Time is frozen and Math.random seeded per page, so two runs of the same
 *    build are expected to be byte-identical. A diff therefore means the CSS,
 *    the template or the fixtures changed — never "the clock moved".
 *  - The API is served from synthetic fixtures, so a busy host or a fresh one
 *    produces the same pixels. This is the property that keeps the gate quiet
 *    enough to stay switched on.
 *  - An unknown /api endpoint answers 404 instead of a fake ok, so a view that
 *    starts calling something new fails here rather than silently baselining an
 *    empty state.
 */

import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join, basename, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { decodePng, encodePng } from './png.mjs'
import { findBrowser, launchBrowser } from './cdp.mjs'
import { startFixtureServer } from './serve.mjs'
import { FIXTURE_NOW_MS } from './fixtures.mjs'

const HERE = resolve(fileURLToPath(new URL('.', import.meta.url)))
const REPO = resolve(HERE, '..', '..', '..')

const VIEWPORT = { width: 1440, height: 900 }

/**
 * Every page is captured in three skins. Light/1440 was the only baseline for
 * a long time, which meant a dark-mode regression or a narrow-window overflow
 * could ship green. `media` drives prefers-color-scheme (the shell has no
 * stored theme in a fresh profile, so this is what flips it).
 */
const VARIANTS = [
  { key: 'light', suffix: '', media: 'light', viewport: VIEWPORT },
  { key: 'dark', suffix: '-dark', media: 'dark', viewport: VIEWPORT },
  { key: 'narrow', suffix: '-narrow', media: 'light', viewport: { width: 1024, height: 900 } },
]

/**
 * The 11 pages. `ready` is a predicate evaluated in the page: it must be true
 * only once the first render has settled, so the screenshot never catches a
 * skeleton or a half-filled table.
 *
 * Keep `ready` narrow — a `document.querySelectorAll('tbody tr').length > 0`
 * that also matches an unrelated table lower on the page would fire too early.
 */
export const PAGES = [
  { slug: 'dashboard', path: '/', title: '概览', ready: () => !!document.querySelector('.main') && !document.querySelector('.loading') },
  { slug: 'plugins', path: '/plugins', title: '插件', ready: () => document.querySelectorAll('.main *').length > 40 },
  { slug: 'accounts', path: '/accounts', title: '账号', ready: () => document.querySelectorAll('[class*="account"]').length > 0 },
  { slug: 'checkin', path: '/checkin', title: '签到', ready: () => !/\u2014/.test(document.querySelector('.content')?.textContent ?? '') || document.querySelectorAll('[class*="calendar"], [class*="checkin"]').length > 0 },
  { slug: 'usage', path: '/usage', title: '用量', ready: () => document.querySelectorAll('.main *').length > 40 },
  // Overlays and failure states: the list above only captures what a page looks
  // like on arrival, so a drawer, an explanation result or an error banner could
  // regress unseen. These cost one interaction each and are pinned in both skins.
  {
    slug: 'accounts-drawer',
    path: '/accounts',
    title: '账号 · 登录态抽屉',
    ready: () => document.querySelectorAll('[class*="account"]').length > 0,
    prepare: [
      `(() => {
        const button = [...document.querySelectorAll('button')].find((el) => el.textContent.trim() === '登录态');
        if (!button) throw new Error('登录态 button missing');
        button.click();
      })()`,
    ],
    settled: () => !!document.querySelector('.drawer'),
    fullPage: false,
    variants: ['light', 'dark'],
  },
  {
    slug: 'scheduler-explain',
    path: '/scheduler',
    title: '调度 · 解释结果',
    ready: () => document.querySelectorAll('.main *').length > 40,
    prepare: [
      `(() => {
        const input = document.querySelector('#explain-model');
        if (!input) throw new Error('explain input missing');
        input.value = 'example/logical-one';
        input.dispatchEvent(new Event('input', { bubbles: true }));
      })()`,
      `(() => {
        const form = document.querySelector('.explain-form');
        if (!form) throw new Error('explain form missing');
        form.requestSubmit();
      })()`,
    ],
    settled: () => !!document.querySelector('.decision-card'),
    variants: ['light', 'dark'],
  },
  {
    slug: 'logs-raw',
    path: '/logs',
    title: '日志 · 原始行',
    ready: () => document.querySelectorAll('table').length > 0,
    prepare: [
      `(() => {
        const button = [...document.querySelectorAll('button')].find((el) => el.textContent.trim() === '原始行');
        if (!button) throw new Error('原始行 button missing');
        button.click();
      })()`,
    ],
    settled: () => !!document.querySelector('.raw-view'),
    variants: ['light', 'dark'],
  },
  {
    slug: 'logs-page2',
    path: '/logs',
    title: '日志 · 第二页',
    ready: () => document.querySelectorAll('.logs-table tbody tr').length > 0,
    // The request table appends the next page when the scroller nears the end.
    // Load every page (120 fixture rows) so the end state — and therefore the
    // screenshot — does not depend on how many scroll events landed before the
    // shot. Jumping to the bottom leaves rows from the last page on screen.
    prepare: [
      `(async () => {
        const scroller = document.querySelector('.logs-scroll');
        if (!scroller) throw new Error('logs scroller missing');
        const rows = () => scroller.querySelectorAll('tbody tr').length;
        const total = Number((document.querySelector('.toolbar-hint')?.textContent ?? '').match(/([\\d,]+) 条/)?.[1].replace(/,/g, '') ?? 0);
        if (!total) throw new Error('row total missing');
        const deadline = performance.now() + 8000;
        while (rows() < total && performance.now() < deadline) {
          scroller.scrollTop = scroller.scrollHeight;
          await new Promise((resolve) => setTimeout(resolve, 100));
        }
        if (rows() < total) throw new Error('pagination stopped at ' + rows() + ' of ' + total);
        scroller.scrollTop = scroller.scrollHeight;
      })()`,
    ],
    settled: () =>
      document.querySelectorAll('.logs-table tbody tr').length >= 120 &&
      !(document.querySelector('.logs-footer')?.textContent ?? '').includes('加载中'),
    fullPage: false,
    variants: ['light', 'dark'],
  },
  {
    slug: 'logs-timeline',
    path: '/logs',
    title: '日志 · 事件时间线',
    ready: () => document.querySelectorAll('.logs-table tbody tr').length > 0,
    // The merged timeline is a tab, so the PAGES entry only ever captured the
    // request table. The footer carries the cursor contract ("已加载 N 条"),
    // which had no pixels guarding it at all.
    prepare: [
      `(() => {
        const tab = [...document.querySelectorAll('.seg-btn')].find((el) => el.textContent.trim() === '事件时间线');
        if (!tab) throw new Error('timeline tab missing');
        tab.click();
      })()`,
    ],
    settled: () => document.querySelectorAll('.event').length > 0,
    variants: ['light', 'dark'],
  },
  {
    slug: 'dashboard-error',
    path: '/?fail=status',
    title: '概览 · 接口失败',
    ready: () => !!document.querySelector('.notice.error'),
    variants: ['light', 'dark'],
  },
  {
    slug: 'accounts-error',
    path: '/accounts?fail=accounts',
    title: '账号 · 接口失败',
    // A per-plugin read failure lands in the warn banner, not the page-level one.
    ready: () => (document.querySelector('.notice')?.textContent ?? '').includes('账号读取失败'),
    variants: ['light', 'dark'],
  },
  {
    slug: 'usage-token',
    path: '/usage',
    title: '用量 · Token',
    ready: () => document.querySelectorAll('.main *').length > 40,
    // The token chart lives on a tab, so it had no baseline and its layout
    // defects (columns capped at 72px, half the card empty) went unseen.
    prepare: [
      `(() => {
        const tab = [...document.querySelectorAll('.tabs .tab')].find((el) => el.textContent.trim() === 'Token');
        if (!tab) throw new Error('Token tab not found');
        tab.click();
      })()`,
    ],
  },
  { slug: 'scheduler', path: '/scheduler', title: '调度', ready: () => document.querySelectorAll('.main *').length > 40 },
  { slug: 'pools', path: '/pools', title: '资源', ready: () => document.querySelectorAll('.main *').length > 40 },
  // Pool detail carries the per-pool settings form, including model_sync_time.
  { slug: 'pool-detail', path: '/pools/officeace', title: '资源 · 详情设置', ready: () => document.querySelectorAll('.main *').length > 40, variants: ['light', 'dark'] },
  { slug: 'rules', path: '/rules', title: '规则', ready: () => document.querySelectorAll('.main *').length > 40 },
  { slug: 'proxies', path: '/proxies', title: '代理', ready: () => document.querySelectorAll('.main *').length > 40 },
  { slug: 'logs', path: '/logs', title: '日志', ready: () => document.querySelectorAll('table').length > 0 },
  { slug: 'settings', path: '/settings', title: '设置', ready: () => document.querySelectorAll('.main *').length > 40 },
  // Keep this last: it flips the fixture's debug_body to true, and the server
  // keeps that write for the rest of the run, so earlier captures must not see it.
  {
    slug: 'logs-debug',
    path: '/logs',
    title: '日志 · Debug 正文提醒',
    ready: () => document.querySelectorAll('.logs-table tbody tr').length > 0,
    prepare: [
      `(() => {
        const button = [...document.querySelectorAll('button')].find((el) => el.textContent.trim().startsWith('Debug'));
        if (!button) throw new Error('Debug button missing');
        button.click();
      })()`,
    ],
    settled: () => !!document.querySelector('.debug-body-warning'),
    fullPage: false,
    variants: ['light', 'dark'],
  },
]

/** Injected before any app script runs: pins the clock and the RNG. */
function freezeScript(nowMs) {
  return `(() => {
  const FIXED = ${nowMs};
  const RealDate = Date;
  const FakeDate = function (...args) {
    if (!(this instanceof FakeDate)) return new RealDate(FIXED).toString();
    return args.length === 0 ? new RealDate(FIXED) : new RealDate(...args);
  };
  FakeDate.prototype = RealDate.prototype;
  FakeDate.now = () => FIXED;
  FakeDate.parse = RealDate.parse;
  FakeDate.UTC = RealDate.UTC;
  Object.setPrototypeOf(FakeDate, RealDate);
  globalThis.Date = FakeDate;

  // Seeded LCG so any Math.random-derived layout (jitter, ids) repeats exactly.
  let seed = 0x2f6f1c3d;
  Math.random = () => {
    seed = (seed * 1664525 + 1013904223) >>> 0;
    return seed / 4294967296;
  };

  // A capture can force one API family to fail (/?fail=accounts), which is how
  // the error states get a baseline at all — the fixture server always answers.
  const fail = new URLSearchParams(location.search).get('fail');
  const blocked = fail && {
    status: '/api/status',
    accounts: '/api/accounts',
    plugins: '/api/plugins',
    usage: '/api/usage',
    scheduler: '/api/scheduler',
    logs: '/api/logs',
  }[fail];
  if (blocked) {
    const realFetch = window.fetch.bind(window);
    window.fetch = (input, init) => {
      const url = typeof input === 'string' ? input : (input && input.url) || '';
      if (url.includes(blocked)) return Promise.reject(new Error('fixture: simulated network failure'));
      return realFetch(input, init);
    };
  }

  // Disable smooth scrolling/animation so a screenshot cannot land mid-transition.
  const style = document.createElement('style');
  style.textContent = '*,*::before,*::after{animation-duration:0s!important;animation-delay:0s!important;transition-duration:0s!important;transition-delay:0s!important;caret-color:transparent!important}';
  document.addEventListener('DOMContentLoaded', () => document.head.appendChild(style));
})();`
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const args = process.argv.slice(2)
const flag = (name) => args.includes(`--${name}`)
const option = (name, fallback) => {
  const index = args.indexOf(`--${name}`)
  return index >= 0 && args[index + 1] ? args[index + 1] : fallback
}

const UPDATE = flag('update')
const ONLY = option('only', null)
const REPORT_DIR = resolve(REPO, option('report-dir', 'web/visual-report'))
const BASELINE_DIR = resolve(REPO, option('baseline-dir', 'web/visual-baseline'))
const MAX_DIFF_CHANGED_PIXELS = Number(option('threshold', '0'))
const PAGE_TIMEOUT_MS = Number(option('timeout', '20000'))
const SETTLE_TIMEOUT_MS = Number(option('settle-timeout', '8000'))

/**
 * Baselines are only comparable within one browser engine.
 *
 * Measured on this repo: Chrome and Edge render the same bundle with 0.12–0.46%
 * of pixels differing across the whole text area — different hinting and glyph
 * rasterisation, not a layout change. Accepting that would mean a tolerance so
 * wide it would also swallow real regressions, so instead each baseline set
 * records which engine produced it and a mismatch is refused outright. That
 * turns a confusing "mysterious tiny diff" into an explicit instruction.
 */
const ENGINE_FILE = join(BASELINE_DIR, '.engine.json')

/**
 * Per-channel tolerance. Anti-aliasing over a long text run can differ by one
 * level on a machine whose font stack resolves slightly differently, and that
 * must not fail the build. Genuine style changes move whole regions by far more.
 */
const CHANNEL_TOLERANCE = 6

/**
 * Hard ceiling on how tall a full-page screenshot may be. This is a real guard,
 * not a clamp: if a page grows past it the run FAILS rather than capturing a
 * truncated image, because a truncated baseline would silently stop guarding
 * the bottom of the page — the exact class of blind spot this gate exists to
 * remove. The tallest fixture-backed page is ~4.2k px (accounts, 60 rows), so
 * the ceiling leaves headroom while still catching a runaway fixture.
 */
const MAX_CAPTURE_HEIGHT = 6000

function pixelDiff(a, b) {
  if (a.width !== b.width || a.height !== b.height) {
    return { sizeMismatch: true, changed: Infinity, total: a.width * a.height, boxes: [] }
  }
  const { width, height } = a
  let changed = 0
  let minX = width
  let minY = height
  let maxX = -1
  let maxY = -1
  for (let i = 0; i < width * height; i++) {
    const offset = i * 4
    const dr = Math.abs(a.data[offset] - b.data[offset])
    const dg = Math.abs(a.data[offset + 1] - b.data[offset + 1])
    const db = Math.abs(a.data[offset + 2] - b.data[offset + 2])
    if (dr > CHANNEL_TOLERANCE || dg > CHANNEL_TOLERANCE || db > CHANNEL_TOLERANCE) {
      changed++
      const x = i % width
      const y = (i - x) / width
      if (x < minX) minX = x
      if (x > maxX) maxX = x
      if (y < minY) minY = y
      if (y > maxY) maxY = y
    }
  }
  return {
    sizeMismatch: false,
    changed,
    total: width * height,
    // Bounding box of the change is the single most useful hint in a report.
    boxes: maxX >= 0 ? [{ x: minX, y: minY, w: maxX - minX + 1, h: maxY - minY + 1 }] : [],
  }
}

/** Writes a diff image: baseline in grey, changed pixels in magenta. */
function writeDiffImage(path, base, actual) {
  const { width, height } = base
  const out = Buffer.alloc(width * height * 4)
  for (let i = 0; i < width * height; i++) {
    const offset = i * 4
    const dr = Math.abs(base.data[offset] - actual.data[offset])
    const dg = Math.abs(base.data[offset + 1] - actual.data[offset + 1])
    const db = Math.abs(base.data[offset + 2] - actual.data[offset + 2])
    if (dr > CHANNEL_TOLERANCE || dg > CHANNEL_TOLERANCE || db > CHANNEL_TOLERANCE) {
      out[offset] = 255
      out[offset + 1] = 0
      out[offset + 2] = 255
    } else {
      // Faded greyscale of the baseline keeps the surrounding layout readable.
      const grey = (base.data[offset] * 0.299 + base.data[offset + 1] * 0.587 + base.data[offset + 2] * 0.114) * 0.25 + 190
      out[offset] = out[offset + 1] = out[offset + 2] = grey
    }
    out[offset + 3] = 255
  }
  writeFileSync(path, encodePng(width, height, out))
}

async function main() {
  const pages = PAGES.flatMap((page) => {
    const skins = VARIANTS.filter((variant) => !page.variants || page.variants.includes(variant.key))
    return skins.map((variant) => ({
      ...page,
      slug: page.slug + variant.suffix,
      media: variant.media,
      viewport: variant.viewport,
    }))
  })
  const selected = ONLY
    ? pages.filter((page) => page.slug === ONLY || page.slug.startsWith(ONLY + '-'))
    : pages
  if (ONLY && selected.length === 0) {
    throw new Error(`--only ${ONLY} matched no page. Known: ${PAGES.map((p) => p.slug).join(', ')}`)
  }

  mkdirSync(BASELINE_DIR, { recursive: true })
  rmSync(REPORT_DIR, { recursive: true, force: true })
  mkdirSync(REPORT_DIR, { recursive: true })

  // Refuse to compare a baseline set against a different engine than made it.
  const engineName = basename(findBrowser())
  if (existsSync(ENGINE_FILE)) {
    const recorded = JSON.parse(readFileSync(ENGINE_FILE, 'utf8')).engine
    if (recorded !== engineName) {
      throw new Error(
        `baselines were produced by ${recorded}, this run uses ${engineName}. ` +
          `Cross-engine glyph rendering differs by ~0.1-0.5% of pixels, so a comparison would be meaningless. ` +
          `Either set AWG_BROWSER=${recorded} or re-run with --update to adopt ${engineName} baselines.`,
      )
    }
  } else if (!UPDATE) {
    throw new Error(
      `no ${ENGINE_FILE} — baselines predate engine tracking. Re-run with --update once to record the engine.`,
    )
  }

  const server = await startFixtureServer({ port: 7899 })
  let browser
  const failures = []
  const results = []
  let browserCrashes = 0

  try {
    browser = await launchBrowser({ width: VIEWPORT.width, height: VIEWPORT.height })
    const { cdp } = browser

    if (UPDATE) {
      writeFileSync(ENGINE_FILE, JSON.stringify({ engine: engineName, viewport: VIEWPORT }, null, 2) + '\n')
    }

    await cdp.send('Page.enable')
    await cdp.send('Runtime.enable')
    await cdp.send('Emulation.setDeviceMetricsOverride', {
      width: VIEWPORT.width,
      height: VIEWPORT.height,
      deviceScaleFactor: 1,
      mobile: false,
    })
    // The shell picks its theme from localStorage (absent in this profile) or
    // prefers-color-scheme, so the emulated media value is what selects a skin.
    // Registered once: registering inside the loop stacked one copy per page.
    await cdp.send('Page.addScriptToEvaluateOnNewDocument', { source: freezeScript(FIXTURE_NOW_MS) })

    for (const page of selected) {
      const url = `${server.origin}${page.path}`
      const started = Date.now()

      await cdp.send('Emulation.setDeviceMetricsOverride', {
        width: page.viewport.width,
        height: page.viewport.height,
        deviceScaleFactor: 1,
        mobile: false,
      })
      await cdp.send('Emulation.setEmulatedMedia', {
        features: [{ name: 'prefers-color-scheme', value: page.media }],
      })
      await cdp.send('Page.navigate', { url })

      // Poll the page-side predicate until it reports ready, with a hard ceiling.
      const predicate = `(${page.ready.toString()})()`
      let ready = false
      let lastError = null
      while (Date.now() - started < PAGE_TIMEOUT_MS) {
        try {
          ready = await cdp.evaluate(predicate)
        } catch (error) {
          lastError = error.message
        }
        if (ready) break
        await sleep(120)
      }
      if (!ready) {
        failures.push({ slug: page.slug, reason: `page never became ready in ${PAGE_TIMEOUT_MS}ms${lastError ? ` (${lastError})` : ''}` })
        results.push({ slug: page.slug, ok: false })
        continue
      }

      // Tab switches and other in-page state changes a page needs before it
      // shows the surface under test.
      for (const step of page.prepare ?? []) {
        await cdp.evaluate(step)
      }

      // An interaction-driven surface (drawer, explanation result) needs its own
      // wait: baselining before it renders would bake in the closed state.
      if (page.settled) {
        const settlePredicate = `(${page.settled.toString()})()`
        const deadline = Date.now() + SETTLE_TIMEOUT_MS
        let settled = false
        while (Date.now() < deadline) {
          try {
            if (await cdp.evaluate(settlePredicate)) { settled = true; break }
          } catch {
            /* keep polling */
          }
          await sleep(100)
        }
        if (!settled) {
          failures.push({ slug: page.slug, reason: `interaction never settled in ${SETTLE_TIMEOUT_MS}ms` })
          results.push({ slug: page.slug, ok: false })
          continue
        }
      }

      // Two rAF turns plus a short settle: covers late paint and lazy chunks.
      await cdp.evaluate('new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(r, 260))))')
      await sleep(120)

      // Capture the WHOLE page, not just the viewport. Several pages are taller
      // than 900px (the accounts table, the plugins list, the log table), and a
      // viewport-only shot silently stops guarding everything below the fold —
      // a regression down there would keep the gate green. The fixtures pin the
      // content height, so the full-page height is stable between runs.
      const metrics = await cdp.evaluate(`(() => {
        const doc = document.documentElement;
        return { width: doc.scrollWidth, height: doc.scrollHeight };
      })()`)
      if (page.fullPage === false) {
        // A drawer or a dialog is positioned against the viewport; capturing the
        // whole (very tall) page would make the overlay a few pixels of a huge
        // image and hide every regression that matters.
        metrics.height = page.viewport.height
        metrics.width = page.viewport.width
      }
      if (metrics.height > MAX_CAPTURE_HEIGHT) {
        // Fail loudly instead of clamping: a clamped baseline would stop
        // guarding the bottom of the page without anyone noticing.
        failures.push({
          slug: page.slug,
          reason: `page is ${metrics.height}px tall, over the ${MAX_CAPTURE_HEIGHT}px capture ceiling — raise MAX_CAPTURE_HEIGHT deliberately`,
        })
        results.push({ slug: page.slug, ok: false })
        continue
      }
      const clipHeight = metrics.height
      // A narrow window that legitimately overflows horizontally must still be
      // captured in full, otherwise the baseline silently loses the right edge.
      const clipWidth = Math.max(page.viewport.width, metrics.width)

      const shot = await cdp.send('Page.captureScreenshot', {
        format: 'png',
        captureBeyondViewport: true,
        fromSurface: true,
        clip: {
          x: 0,
          y: 0,
          width: clipWidth,
          height: clipHeight,
          scale: 1,
        },
      })
      const png = Buffer.from(shot.data, 'base64')
      const actualPath = join(REPORT_DIR, `${page.slug}.actual.png`)
      writeFileSync(actualPath, png)
      const actual = decodePng(png)

      const baselinePath = join(BASELINE_DIR, `${page.slug}.png`)
      if (UPDATE || !existsSync(baselinePath)) {
        writeFileSync(baselinePath, png)
        results.push({ slug: page.slug, ok: true, created: !existsSync(baselinePath) || UPDATE, bytes: png.length })
        continue
      }

      const baseline = decodePng(readFileSync(baselinePath))
      const diff = pixelDiff(baseline, actual)
      const overThreshold = diff.changed > MAX_DIFF_CHANGED_PIXELS
      if (overThreshold) {
        writeDiffImage(join(REPORT_DIR, `${page.slug}.diff.png`), baseline, actual)
        failures.push({
          slug: page.slug,
          reason: diff.sizeMismatch
            ? `size changed ${baseline.width}x${baseline.height} -> ${actual.width}x${actual.height}`
            : `${diff.changed} of ${diff.total} pixels differ (${((diff.changed / diff.total) * 100).toFixed(3)}%) in ${JSON.stringify(diff.boxes)}`,
        })
      }
      results.push({ slug: page.slug, ok: !overThreshold, changed: diff.changed, total: diff.total })
    }
  } catch (error) {
    browserCrashes++
    throw error
  } finally {
    if (browser) await browser.kill()
    await server.close()
  }

  writeFileSync(join(REPORT_DIR, 'result.json'), JSON.stringify({ update: UPDATE, results, failures }, null, 2))

  if (failures.length === 0) {
    const created = results.filter((r) => r.created).length
    console.log(`visual: ${results.length - failures.length}/${results.length} pages match${UPDATE ? ' (baselines rewritten)' : ''}${created ? `, ${created} created` : ''}`)
    return 0
  }

  console.error(`visual: ${failures.length} of ${results.length} pages differ`)
  for (const failure of failures) console.error(`  - ${failure.slug}: ${failure.reason}`)
  console.error(`\nactual:   ${join(REPORT_DIR, '<page>.actual.png')}`)
  console.error(`diff:     ${join(REPORT_DIR, '<page>.diff.png')}`)
  console.error(`baseline: ${BASELINE_DIR}`)
  console.error('\nIf the change is intended, re-run with --update and commit the new baselines.')
  return 1
}

main().then(
  (code) => process.exit(code),
  (error) => {
    console.error(`visual: ${error.message}`)
    process.exit(2)
  },
)
