import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import axe from "axe-core"

import SettingsPage from "@/app/(dashboard)/settings/page"
import ConnectionsPage from "@/app/(dashboard)/connections/page"
import { PipelinesPageClient } from "@/app/(dashboard)/pipelines/PipelinesPageClient"
import HomePage from "@/app/(dashboard)/page"
import { Sidebar } from "@/components/layout/Sidebar"
import { AgenticPipelineHome } from "@/components/chat/AgenticPipelineHome"
import { ChatInputBar } from "@/components/chat/ChatInputBar"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { featureFlagsManager } from "@/config/features"

// KI-AXE-VIOLATIONS-ON-AUTHENTICATED-PAGES (was KI-2). The Playwright a11y suite
// (`npm run test:a11y`) measures these routes end to end in Chromium, signed in to a
// stub api-gateway (e2e/fixtures/a11y-api-stub.mjs) — but only nightly and on manual
// dispatch, not on PRs, and only on an empty workspace.
//
// This file is the guard that runs on every PR: `src/__tests__/**` is in the
// vitest `include` list, and ci.yml runs `npm test` in the `frontend` job. It renders
// the same components under jsdom and applies the SAME blocking bar as the Playwright
// spec (`e2e/a11y/authenticated_pages.spec.ts:17` — zero serious/critical violations
// under wcag2a/wcag2aa/wcag21a/wcag21aa), so the two gates agree on what "blocking" means.
//
// SCOPE — this is a name/role/label gate, NOT a contrast gate. `color-contrast` is
// explicitly disabled because jsdom has no layout and no paint: the rule needs real
// computed colours (and `HTMLCanvasElement.getContext`, which jsdom does not implement),
// so it can only ever return `incomplete`, never `violations`. Contrast is the
// Playwright suite's job. What jsdom CAN see is the class behind the contrast failures
// that suite reported on /pipelines and the old /dashboard (now Home): see
// faintLightModeText below.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn(), replace: vi.fn() }),
  usePathname: () => "/settings",
  useSearchParams: () => new URLSearchParams(),
}))
// Home (/) is an async server component: it forwards the request cookies and
// fetches the gateway itself for the first-run checklist, so both are stubbed.
vi.mock("next/headers", () => ({ cookies: async () => ({ getAll: () => [] }) }))
vi.mock("next-themes", () => ({
  useTheme: () => ({ theme: "light", resolvedTheme: "light", setTheme: vi.fn() }),
}))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: "admin",
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => true,
    meets: () => true,
  }),
}))
// Collapsed rail AND open mobile drawer, so both icon-only Sidebar buttons —
// the collapse toggle and the drawer's X — are in the measured DOM at once.
vi.mock("@/lib/store/useUIStore", () => ({
  useUIStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      sidebarCollapsed: true,
      sidebarOpen: true,
      setSidebarOpen: () => {},
      toggleSidebarCollapsed: () => {},
    }),
}))

const mockFetch = authFetch as unknown as Mock

function res(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response
}

type Impact = "minor" | "moderate" | "serious" | "critical"
const BLOCKING: Impact[] = ["serious", "critical"]

/**
 * Runs axe over `container` and returns one string per blocking violation, e.g.
 * `button-name (critical) x3`. An empty array is the pass condition.
 */
async function blockingViolations(container: HTMLElement): Promise<string[]> {
  const results = await axe.run(container, {
    runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] },
    // See the SCOPE note at the top of this file: jsdom cannot evaluate contrast.
    rules: { "color-contrast": { enabled: false } },
  })
  return results.violations
    .filter((v) => BLOCKING.includes(v.impact as Impact))
    .map((v) => `${v.id} (${v.impact}) x${v.nodes.length}`)
}

/**
 * Text drawn in light-mode `text-zinc-400` (#a1a1aa): 2.6:1 on white, under both the
 * 4.5:1 AA bar and the 3:1 large-text one. `[class~=…]` matches the bare token only,
 * so `dark:text-zinc-400` (on a dark background) passes. Icons are not text.
 */
function faintLightModeText(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll<HTMLElement>("[class~='text-zinc-400']"))
    .filter((el) => !(el instanceof SVGElement) && el.textContent?.trim())
    .map((el) => el.textContent!.trim().slice(0, 40))
}

beforeAll(() => {
  // Radix (Switch, DropdownMenu, AlertDialog, ScrollArea) observes element size.
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
})

describe("authenticated pages — no serious/critical axe violations", () => {
  it("/settings, every tab", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url === API_ENDPOINTS.NOTIFICATIONS.PREFERENCES) {
        return Promise.resolve(
          res({
            email_enabled: true,
            email_categories: { data_loss: true, run_status: false },
            categories: [
              { id: "data_loss", label: "Data loss & integrity", description: "Rows may be missing." },
              { id: "run_status", label: "Run failed or stopped", description: "A run ended with an error." },
            ],
            channels: { email: false, slack: true },
          }),
        )
      }
      if (url === API_ENDPOINTS.AGENT_TOKENS.LIST) return Promise.resolve(res({ tokens: [] }))
      return Promise.resolve(res({ name: "Ada Lovelace", email: "ada@example.com" }))
    })
    act(() => featureFlagsManager.updateFlags({ agentGateway: true }))

    try {
      const { container } = render(<SettingsPage />)
      // An inactive tab is not in the DOM, so each tab is opened and measured in
      // turn, once its data has landed (the real form, switches and token list).
      const measured: Record<string, string[]> = {}
      await waitFor(() => expect(screen.getByLabelText("Email")).toHaveValue("ada@example.com"))
      measured.Account = await blockingViolations(container)

      await userEvent.click(screen.getByRole("tab", { name: "Email alerts" }))
      await screen.findByRole("switch", { name: "Email: Run failed or stopped" })
      measured["Email alerts"] = await blockingViolations(container)

      await userEvent.click(screen.getByRole("tab", { name: "AI apps" }))
      await screen.findByText(/no agent tokens/i)
      measured["AI apps"] = await blockingViolations(container)

      await userEvent.click(screen.getByRole("tab", { name: "Appearance" }))
      await screen.findByRole("button", { name: "System" })
      measured.Appearance = await blockingViolations(container)

      expect(measured).toEqual({ Account: [], "Email alerts": [], "AI apps": [], Appearance: [] })
    } finally {
      act(() => featureFlagsManager.resetFlags())
    }
  })

  it("/connections", async () => {
    // A zero-row render would make this case vacuously green: the row-actions
    // button only exists once per connection. Assert the row is present first.
    mockFetch.mockImplementation((url: string) => {
      if (url === API_ENDPOINTS.OAUTH.TOKENS) return Promise.resolve(res({ tokens: [] }))
      if (url === API_ENDPOINTS.CONNECTIONS.LIST) {
        return Promise.resolve(
          res({
            connections: [
              {
                id: "conn-1",
                name: "Analytics Postgres",
                description: "Primary warehouse",
                type: "source",
                connector_type: "postgresql",
                is_connected: true,
                config: {},
                created_at: "2026-01-02T03:04:05Z",
                updated_at: "2026-01-02T03:04:05Z",
              },
            ],
          }),
        )
      }
      return Promise.resolve(res({}))
    })

    const { container } = render(<ConnectionsPage />)
    await waitFor(() => expect(screen.getByText("Analytics Postgres")).toBeInTheDocument())

    expect(await blockingViolations(container)).toEqual([])
  })

  it("/pipelines", async () => {
    // As for /connections: the row's actions only exist once a pipeline is listed.
    mockFetch.mockImplementation((url: string) => {
      if (url.startsWith(`${API_ENDPOINTS.PIPELINES.LIST}?`)) {
        return Promise.resolve(
          res({
            pipelines: [
              {
                id: "pipe-1",
                name: "Orders to warehouse",
                pipeline_status: "active",
                derived_status: "completed",
                sync_mode: "batch",
                created_at: "2026-01-02T03:04:05Z",
                updated_at: "2026-01-02T03:04:05Z",
                source_connection: { id: "c1", name: "Orders PG", connector_type: "postgresql" },
                destination_connection: { id: "c2", name: "Lake", connector_type: "gcs" },
                last_execution: { id: "e1", status: "completed" },
              },
            ],
            pagination: { page: 1, per_page: 25, total: 1, total_pages: 1, has_next: false, has_prev: false },
          }),
        )
      }
      return Promise.resolve(res({}))
    })

    const { container } = render(<PipelinesPageClient />)
    await waitFor(() => expect(screen.getByText("Orders to warehouse")).toBeInTheDocument())

    expect(await blockingViolations(container)).toEqual([])
    expect(faintLightModeText(container)).toEqual([])
  })

  it("/ (Home), with and without a model", async () => {
    // A workspace with something in every card and one run of each outcome, so
    // the attention list, the status badges, the run links and the run charts are
    // all measured: an empty Home would pass vacuously. The SSR checklist read sees no
    // connections, so the first-run checklist renders too.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ total: 0, pipelines: [], connections: [] }) })),
    )
    const now = new Date().toISOString()
    mockFetch.mockImplementation((url: string) => {
      const path = url.replace(/^https?:\/\/[^/]+/, "")
      if (path.startsWith("/api/v1/pipelines?status=failed")) {
        return Promise.resolve(res({ total: 1, pipelines: [{ id: "p1", name: "Orders to warehouse" }] }))
      }
      if (path.startsWith("/api/v1/pipelines?status=")) return Promise.resolve(res({ total: 0, pipelines: [] }))
      if (path.startsWith("/api/v1/pipelines")) return Promise.resolve(res({ total: 3, pipelines: [] }))
      if (path.startsWith("/api/v1/explorer/schedules")) {
        return Promise.resolve(res({ schedules: [{ schedule_id: "s1", saved_query_id: "q1", name: "Daily revenue", status: "active" }] }))
      }
      if (path.startsWith("/api/v1/explorer/freshness")) return Promise.resolve(res({ breaches: [] }))
      if (path.startsWith("/api/v1/workflows")) {
        return Promise.resolve(res({ workflows: [{ id: "w1", name: "Weekly report", status: "active" }] }))
      }
      if (path.startsWith("/api/v1/activity/metrics")) {
        const day = now.slice(0, 10)
        return Promise.resolve(
          res({
            days: [day],
            time_zone: "UTC",
            pipeline_runs: [{ day, status: "failed", count: 1 }],
            rows_written: [{ day, rows: 10 }],
          }),
        )
      }
      if (path.startsWith("/api/v1/activity")) {
        const run = (id: string, kind: string, name: string, status: string) => ({
          kind, id, subject_id: `${id}-subject`, subject_name: name, status,
          source_status: status, trigger: "schedule", started_at: now,
        })
        return Promise.resolve(
          res({
            runs: [
              run("e1", "pipeline", "Orders to warehouse", "failed"),
              run("e2", "scheduled_query", "Daily revenue", "succeeded"),
              run("e3", "workflow", "Weekly report", "running"),
            ],
          }),
        )
      }
      return Promise.resolve(res({}))
    })

    try {
      // No model: the box is the pipeline builder, with the Tab hint and the note.
      act(() => {
        featureFlagsManager.updateFlags({ assistant: true, llmConfigured: false })
        featureFlagsManager.markResolved()
      })
      const { container } = render(await HomePage())
      // Both landed: the failed pipeline is in the attention list AND the runs.
      await waitFor(() => expect(screen.getAllByRole("link", { name: /Orders to warehouse/ }).length).toBe(2))
      // And the Activity card drew its six run charts.
      await waitFor(() => expect(within(screen.getByRole("region", { name: "Activity" })).getAllByRole("figure")).toHaveLength(6))
      expect(screen.getByRole("heading", { name: "Create a pipeline" })).toBeInTheDocument()
      expect(screen.getByText("to use the suggestion")).toBeInTheDocument()
      const measured: Record<string, string[]> = {}
      measured.builder = [...(await blockingViolations(container)), ...faintLightModeText(container)]

      // With a model: the box is Ask rsync.ai.
      act(() => featureFlagsManager.updateFlags({ llmConfigured: true }))
      await screen.findByRole("heading", { name: "Ask rsync.ai" })
      measured.assistant = [...(await blockingViolations(container)), ...faintLightModeText(container)]

      expect(measured).toEqual({ builder: [], assistant: [] })
    } finally {
      act(() => featureFlagsManager.resetFlags())
      vi.unstubAllGlobals()
    }
  })

  it("/chat landing (AgenticPipelineHome)", async () => {
    mockFetch.mockImplementation(() => Promise.resolve(res({ connections: [] })))

    // The landing over the chat's box, the page's only box, with an intent typed so
    // its icon-only send button is enabled: the state a user reaches it in (#47
    // prod retest: the landing's own box, since removed, had a send arrow with no
    // accessible name).
    const { container } = render(
      <>
        <AgenticPipelineHome onSubmit={() => {}} onPrefill={() => {}} onCustom={() => {}} />
        <ChatInputBar
          input="sync pg to gcs"
          onInputChange={() => {}}
          isSendingMessage={false}
          pipelineId={undefined}
          isPipelineDone={false}
          executionFailed={false}
          onStartNew={() => {}}
          onViewPipeline={() => {}}
          onRetry={() => {}}
        />
      </>,
    )
    await screen.findByText("Example pipelines")
    expect(screen.getByRole("button", { name: "Send message" })).toBeEnabled()

    expect(await blockingViolations(container)).toEqual([])
  })

  it("sidebar, collapsed rail + mobile drawer", async () => {
    // `sidebarCollapsed` is persisted to localStorage (useUIStore partialize), so any
    // user who collapses the rail carries this state onto every authenticated page —
    // the collapsed branch is the only one where the toggle has no visible text.
    const { container } = render(<Sidebar role="user" />)
    await waitFor(() => expect(screen.getAllByRole("link").length).toBeGreaterThan(0))

    expect(await blockingViolations(container)).toEqual([])
  })
})
