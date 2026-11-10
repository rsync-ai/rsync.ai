import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

/**
 * Accessibility checks for authenticated routes.
 *
 * Same blocking impact bar as public_pages.spec.ts (zero serious/critical
 * WCAG 2.1 A/AA violations). The `authenticated-a11y` project loads the
 * storageState produced by `fixtures/auth.setup.ts`; when that login fails,
 * these fail with it rather than skip.
 *
 * Keep this list short and stable: pages that render with no run-specific
 * state. Dashboard list views, settings index, etc.
 */

type Impact = 'minor' | 'moderate' | 'serious' | 'critical'
const BLOCKING: Impact[] = ['serious', 'critical']

const ROUTES = [
  { path: '/', label: 'Home' },
  { path: '/runs', label: 'Runs' },
  { path: '/connections', label: 'Connections' },
  { path: '/pipelines', label: 'Pipelines' },
  { path: '/settings', label: 'Settings' },
]

for (const { path: route, label } of ROUTES) {
  test(`${label} (${route}) has no serious/critical axe violations`, async ({ page }, testInfo) => {
    const response = await page.goto(route)
    // Middleware redirects to /login if storageState is invalid — fail loudly
    // rather than silently asserting against the login page.
    // Compare the path, not a substring: every URL contains "/", /login included.
    expect(new URL(page.url()).pathname, `expected to land on ${route}, got ${page.url()}`).toBe(route)
    expect(response?.ok() ?? false, `non-OK response on ${route}: ${response?.status()}`).toBeTruthy()

    await page.waitForLoadState('networkidle')

    const results = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
      .analyze()

    const blocking = results.violations.filter((v) => BLOCKING.includes(v.impact as Impact))

    await testInfo.attach('axe-report.json', {
      body: JSON.stringify(results, null, 2),
      contentType: 'application/json',
    })

    expect(
      blocking,
      `axe found ${blocking.length} blocking violation(s) on ${route}: ` +
        blocking.map((v) => `${v.id} (${v.impact})`).join(', '),
    ).toEqual([])
  })
}
