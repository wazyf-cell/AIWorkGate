/**
 * One delegated listener gives every control the same press ripple, instead of
 * adding a directive to a few hundred buttons. Ripple is decoration only: it
 * never changes layout, never blocks the click, and is skipped entirely for
 * users who asked for reduced motion.
 */
const TARGETS = '.btn, .tab, .seg-btn, .nav-item, .preset-card, .legend-toggle'
const DURATION_MS = 560

/** index.html resolves OS preference + the 动效 override into this attribute. */
function prefersReducedMotion(): boolean {
  return document.documentElement.dataset.motion === 'reduced'
}

export function installRipple(): void {
  document.addEventListener(
    'pointerdown',
    (event: PointerEvent) => {
      if (event.button !== 0 || prefersReducedMotion()) return
      const host = (event.target as Element | null)?.closest<HTMLElement>(TARGETS)
      if (!host || host.dataset.noRipple !== undefined || host.hasAttribute('disabled')) return
      const rect = host.getBoundingClientRect()
      if (rect.width === 0 || rect.height === 0) return
      const size = Math.max(rect.width, rect.height) * 1.1
      const dot = document.createElement('span')
      dot.className = 'ripple'
      dot.style.width = `${size}px`
      dot.style.height = `${size}px`
      dot.style.left = `${event.clientX - rect.left - size / 2}px`
      dot.style.top = `${event.clientY - rect.top - size / 2}px`
      host.appendChild(dot)
      dot.addEventListener('animationend', () => dot.remove(), { once: true })
      // animationend does not fire when the tab is backgrounded mid-press.
      window.setTimeout(() => dot.remove(), DURATION_MS + 200)
    },
    { passive: true },
  )
}
