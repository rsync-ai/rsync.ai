// A stand-in api-gateway for the axe suite (playwright.a11y.config.ts).
//
// The suite measures what a signed-in user sees, so it needs a backend that logs
// in and says who the user is -- without one, auth.setup.ts had nothing to log in
// to and the authenticated pages were never visited. This answers just enough for
// the pages to render their real chrome: a session, the user, and empty lists.
// Data-dependent screens are out of scope; an empty workspace is the state every
// new user sees first.
//
// No dependencies; `node e2e/fixtures/a11y-api-stub.mjs`. Port from A11Y_STUB_PORT.
// A request it has no answer for gets a 404 and a line on stderr naming it.

import http from 'node:http'

const PORT = Number(process.env.A11Y_STUB_PORT || 5199)
const TOKEN = 'a11y-stub-session'

const USER = {
  user_id: '00000000-0000-4000-8000-0000000a11e0',
  email: 'a11y@rsync-ai.local',
  name: 'A11y Check',
  role: 'owner',
  status: 'active',
  email_verified: true,
  plan: 'pro',
  trial_ends_at: null,
  pipelines_used: 0,
  pipelines_limit: null,
}

const WORKSPACE_ID = '00000000-0000-4000-8000-0000000a11e1'
const CREATED = '2026-01-01T00:00:00Z'

// `days` is every UTC date in the window, oldest first, ending today
// (activity_metrics.go); the charts take their x-axis from it.
function windowDays(n) {
  const today = new Date()
  return Array.from({ length: n }, (_, i) =>
    new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() - (n - 1 - i))).toISOString().slice(0, 10))
}

// Each body is what api-gateway answers for a new workspace -- one owner, nothing
// created yet, billing enforced, plan pro. Lists are [] (never null), as the
// handlers write them, so pages render their empty states, not their error ones.
const ROUTES = {
  'GET /health': () => ({ status: 'ok' }),
  'GET /api/v1/auth/me': () => USER,
  'GET /api/v1/features': () => ({
    monitoring_overview: true, monitoring_infra: false, monitoring_traces: true,
    usage_panel: true, agent_gateway: false, assistant: false, llm_configured: false,
  }),
  'GET /api/v1/workspaces': () => ({
    workspaces: [{
      id: WORKSPACE_ID, name: 'My Workspace', slug: 'my-workspace', owner_id: USER.user_id, plan: 'pro',
      role: 'owner', is_personal: true, created_at: CREATED, updated_at: CREATED,
    }],
  }),
  'GET /api/v1/usage': () => ({
    workspace_id: WORKSPACE_ID, plan: 'pro', pipelines_used: 0, pipeline_limit: null, can_run: true, blocked: false,
    queries_used: 0, queries_limit: 10000,
    transfer: { rows_read: 0, rows_written: 0, records_processed: 0, cdc_events_applied: 0, bytes: 0, gb: 0, gb_metered: true, gb_limit: 500 },
    pipelines: [], retention_enabled: false, retention_days: 90, plan_expires_at: null,
  }),
  'GET /api/v1/usage/plan': () => ({
    workspace_id: WORKSPACE_ID, plan: 'pro', pipelines_used: 0, pipelines_limit: null, trial_ends_at: null, blocked: false,
  }),
  'GET /api/v1/notifications/unread-count': () => ({ unread_count: 0 }),
  'GET /api/v1/pipelines': (q) => {
    const perPage = Number(q.get('limit') || q.get('per_page') || 50)
    return {
      pipelines: [],
      pagination: { page: 1, per_page: perPage, total: 0, total_pages: 0, has_next: false, has_prev: false },
      total: 0, limit: perPage, offset: 0,
    }
  },
  'GET /api/v1/connections': () => ({ connections: [], total: 0 }),
  'GET /api/v1/activity': () => ({ runs: [] }),
  'GET /api/v1/activity/metrics': (q) => ({
    days: windowDays(Number(q.get('days') || 7)), time_zone: 'UTC',
    pipeline_runs: [], pipeline_run_time: [], rows_written: [], model_runs: [], model_run_time: [], checks: [],
  }),
  'GET /api/v1/demo/status': () => ({ available: false, reason: 'not_configured' }),
  'GET /api/v1/explorer/schedules': () => ({ schedules: [], count: 0 }),
  'GET /api/v1/explorer/freshness': () => ({ breaches: [], count: 0 }),
  'GET /api/v1/workflows': () => ({ workflows: [], can_manage: true }),
  'GET /api/v1/oauth/tokens': () => ({ tokens: [], count: 0 }),
}

function send(req, res, status, body, extraHeaders = {}) {
  const origin = req.headers.origin
  res.writeHead(status, {
    'Content-Type': 'application/json',
    ...(origin ? { 'Access-Control-Allow-Origin': origin, 'Access-Control-Allow-Credentials': 'true', Vary: 'Origin' } : {}),
    ...extraHeaders,
  })
  res.end(body === undefined ? '' : JSON.stringify(body))
}

const server = http.createServer((req, res) => {
  const { pathname, searchParams } = new URL(req.url, `http://localhost:${PORT}`)
  if (req.method === 'OPTIONS') {
    return send(req, res, 204, undefined, {
      'Access-Control-Allow-Methods': 'GET, POST, PUT, PATCH, DELETE, OPTIONS',
      'Access-Control-Allow-Headers': req.headers['access-control-request-headers'] || '*',
    })
  }
  if (req.method === 'POST' && pathname === '/api/v1/auth/login') {
    return send(req, res, 200, { token: TOKEN, user: USER }, {
      'Set-Cookie': `auth_token=${TOKEN}; Path=/; HttpOnly; SameSite=Lax`,
    })
  }
  const route = ROUTES[`${req.method} ${pathname}`]
  if (route) return send(req, res, 200, route(searchParams))
  process.stderr.write(`a11y-api-stub: no answer for ${req.method} ${pathname}\n`)
  return send(req, res, 404, { error: 'not stubbed', path: pathname })
})

// The app opens a websocket for live updates; refuse it so the client falls back
// to its disconnected state instead of hanging the page's network-idle wait.
server.on('upgrade', (_req, socket) => socket.destroy())

server.listen(PORT, '127.0.0.1', () => {
  process.stdout.write(`a11y-api-stub listening on http://localhost:${PORT}\n`)
})
