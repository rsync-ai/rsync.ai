import { defineConfig, devices } from '@playwright/test'
import path from 'node:path'
import { A11Y_API_URL, A11Y_STUB_PORT, USES_STUB } from './e2e/fixtures/a11y-backend'

/**
 * Playwright config for accessibility (axe-core) tests.
 *
 * Three projects:
 *   - `setup`             — logs in once and persists storageState. Fails when
 *                           the backend cannot be reached: a skipped login used
 *                           to mean the authenticated pages were never checked
 *                           while the run still read green.
 *   - `public-a11y`       — unauthenticated routes (login, signup).
 *   - `authenticated-a11y`— routes behind the middleware auth guard. Depends
 *                           on `setup` and loads its storageState.
 *
 * The backend is a11y-api-stub.mjs, started below, unless E2E_API_URL names a
 * real api-gateway (e2e/fixtures/a11y-backend.ts).
 */

const AUTH_STATE = path.join(__dirname, '.auth', 'user.json')
const API_WS_URL = `${A11Y_API_URL.replace(/^http/, 'ws')}/ws`

const stub = {
  command: 'node e2e/fixtures/a11y-api-stub.mjs',
  url: `${A11Y_API_URL}/health`,
  reuseExistingServer: !process.env.CI,
  timeout: 15_000,
  env: { A11Y_STUB_PORT },
}

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: [['list'], ['html', { outputFolder: 'playwright-a11y-report', open: 'never' }]],
  use: {
    baseURL: process.env.FRONTEND_URL || 'http://localhost:3100',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'setup',
      testMatch: /a11y\/.*\.setup\.ts$|fixtures\/auth\.setup\.ts$/,
    },
    {
      name: 'public-a11y',
      testMatch: /a11y\/public_pages\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'authenticated-a11y',
      testMatch: /a11y\/authenticated_pages\.spec\.ts$/,
      use: {
        ...devices['Desktop Chrome'],
        storageState: AUTH_STATE,
      },
      dependencies: ['setup'],
    },
  ],
  webServer: [
    ...(USES_STUB ? [stub] : []),
    {
      // `next start` against a prebuilt app. CI runs `next build` before this.
      command: 'PORT=3100 npm run start',
      url: 'http://localhost:3100/login',
      reuseExistingServer: !process.env.CI,
      timeout: 120_000,
      env: {
        // The build bakes NEXT_PUBLIC_*; PUBLIC_URL / PUBLIC_WS_URL are read per
        // request (src/lib/config/runtime-env.ts) and outrank the baked value in
        // the browser, so a prebuilt app reaches this run's backend.
        NEXT_PUBLIC_API_URL: A11Y_API_URL,
        NEXT_PUBLIC_WS_URL: API_WS_URL,
        PUBLIC_URL: A11Y_API_URL,
        PUBLIC_WS_URL: API_WS_URL,
        // Server-side fetches in the dashboard layout (getCurrentUser, etc.)
        // default to the docker-compose hostname.
        API_GATEWAY_INTERNAL_URL: A11Y_API_URL,
      },
    },
  ],
  timeout: 30_000,
  expect: { timeout: 5_000 },
})
