/**
 * The home page's first-run checklist, overview and recent runs must follow the
 * active workspace, not just the one that was active when they mounted.
 *
 * Found on a fresh v0.1.7 self-host install: a browser that had used an earlier
 * install kept a `rsync_active_workspace_id` the new database had never seen.
 * authFetch stamps that id on every request, the gateway answers each with 404
 * "workspace not found", and WorkspaceContext/Header then re-point the selection
 * at a real workspace (setActiveWorkspaceId, which fires ACTIVE_WORKSPACE_EVENT).
 * FirstRunOnboarding and the old stat cards fetched once on mount and never
 * listened, so they kept the 404: no "Try it with sample data" card and zeroed
 * stats until a manual reload. A manual switch hit the same gap -- router.refresh
 * hands both new server props, and both ignore them after mount. The overview
 * and recent runs that replaced the stat cards load in the browser only, so
 * they are pinned here the same way.
 *
 * Same class as #712, which moved ten list pages onto onActiveWorkspaceChange and
 * captureWorkspace; these two were not in it. The idiom here is the one
 * chat-home-workspace-switch.test.tsx pins for useHomeData.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi, type Mock } from "vitest"
import { act, render, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

import { authFetch } from "@/lib/api/auth-fetch"
import {
  ACTIVE_WORKSPACE_EVENT,
  ACTIVE_WORKSPACE_KEY,
  setActiveWorkspaceId,
} from "@/lib/workspace/active-workspace"
import { FirstRunOnboarding } from "@/components/onboarding/FirstRunOnboarding"
import { HomeOverview } from "@/components/home/HomeOverview"
import { RecentActivity } from "@/components/home/RecentActivity"

const mockFetch = authFetch as unknown as Mock

const STALE_WS = "ws-from-a-previous-install"
const WS_A = "ws-a"
const WS_B = "ws-b"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

const demoStatus = {
  available: true,
  source_connector: "sample-data",
  source_name: "Sample data (demo)",
  destination_connector: "postgresql",
  destination_name: "Demo warehouse",
  destination_database: "demo",
}

interface WorkspaceData {
  sources: number
  destinations: number
  pipelines: number
  schedules?: number
  workflows?: number
  /** Names of the newest runs, newest first. */
  runs?: string[]
}

// Answers the way the gateway does: by the X-Workspace-ID authFetch would stamp,
// i.e. whatever is active at call time. An id with no entry is a 404.
function serveByActiveWorkspace(byWs: Record<string, WorkspaceData>) {
  mockFetch.mockImplementation(async (path: string) => respond(path, byWs[activeId()]))
}

function activeId() {
  return window.localStorage.getItem(ACTIVE_WORKSPACE_KEY) ?? ""
}

function respond(url: string, ws: WorkspaceData | undefined) {
  if (!ws) return res(404, { error: "workspace not found" })
  // Some endpoints carry the gateway origin (API_ENDPOINTS), some are relative.
  const path = url.replace(/^https?:\/\/[^/]+/, "")
  if (path.startsWith("/api/v1/demo/status")) return res(200, demoStatus)
  if (path.startsWith("/api/v1/connections?type=source")) return res(200, { total: ws.sources, connections: [] })
  if (path.startsWith("/api/v1/connections?type=destination"))
    return res(200, { total: ws.destinations, connections: [] })
  // A status-filtered read is "how many are failing": none, in these workspaces.
  if (path.startsWith("/api/v1/pipelines?status=")) return res(200, { total: 0, pipelines: [] })
  if (path.startsWith("/api/v1/pipelines")) return res(200, { total: ws.pipelines, pipelines: [] })
  if (path.startsWith("/api/v1/usage")) return res(200, { queries_used: 0 })
  if (path.startsWith("/api/v1/explorer/schedules"))
    return res(200, { schedules: Array.from({ length: ws.schedules ?? 0 }, (_, i) => schedule(i)) })
  if (path.startsWith("/api/v1/explorer/freshness")) return res(200, { breaches: [] })
  if (path.startsWith("/api/v1/workflows"))
    return res(200, { workflows: Array.from({ length: ws.workflows ?? 0 }, (_, i) => workflow(i)), can_manage: true })
  if (path.startsWith("/api/v1/activity")) return res(200, { runs: (ws.runs ?? []).map(run) })
  return res(404, {})
}

const schedule = (i: number) => ({ schedule_id: `s${i}`, saved_query_id: `q${i}`, name: `Query ${i}`, status: "active" })
const workflow = (i: number) => ({ id: `w${i}`, name: `Workflow ${i}`, status: "active" })
const run = (name: string, i: number) => ({
  kind: "pipeline",
  id: `e${i}`,
  subject_id: `p${i}`,
  subject_name: name,
  status: "succeeded",
  source_status: "completed",
  trigger: "manual",
  started_at: new Date().toISOString(),
})

const emptyWs: WorkspaceData = { sources: 0, destinations: 0, pipelines: 0 }
const noCounts = { sourceCount: 0, destinationCount: 0, pipelineCount: 0, queryCount: 0 }

/** The big number on an overview card, as shown ("—" when unknown). */
function cardTotal(title: string) {
  return screen.getByRole("region", { name: title }).querySelector("p")?.textContent
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  window.localStorage.clear()
  vi.spyOn(console, "warn").mockImplementation(() => {})
  vi.spyOn(console, "error").mockImplementation(() => {})
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe("FirstRunOnboarding follows the active workspace", () => {
  it("shows the sample-data card once a stale workspace id self-heals, without a reload", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, STALE_WS)
    serveByActiveWorkspace({ [WS_A]: emptyWs })
    render(<FirstRunOnboarding initial={noCounts} />)

    // The mount-time read went out under the stale id and was refused.
    await waitFor(() => expect(mockFetch).toHaveBeenCalledWith("/api/v1/demo/status"))
    expect(screen.queryByRole("button", { name: /Start with sample data/ })).not.toBeInTheDocument()

    // WorkspaceContext's self-heal: the real setter, which fires the event.
    act(() => setActiveWorkspaceId(WS_A))

    expect(await screen.findByRole("button", { name: /Start with sample data/ })).toBeInTheDocument()
  })

  it("re-reads the checklist for a workspace switched in another tab", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    serveByActiveWorkspace({ [WS_A]: emptyWs, [WS_B]: { ...emptyWs, sources: 1, destinations: 1 } })
    render(<FirstRunOnboarding initial={noCounts} />)
    await screen.findByText("0 of 4 done")

    act(() => {
      window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_B)
      window.dispatchEvent(new StorageEvent("storage", { key: ACTIVE_WORKSPACE_KEY }))
    })

    expect(await screen.findByText("2 of 4 done")).toBeInTheDocument()
  })

  it("drops counts read for a workspace the user has already left", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    const byWs: Record<string, WorkspaceData> = {
      [WS_A]: { ...emptyWs, sources: 1, destinations: 1, pipelines: 1 },
      [WS_B]: emptyWs,
    }
    // A's reads hang until released; B's answer at once.
    let releaseA: () => void = () => {}
    const aGate = new Promise<void>((r) => (releaseA = r))
    mockFetch.mockImplementation(async (path: string) => {
      const ws = activeId()
      if (ws === WS_A && !path.startsWith("/api/v1/demo/status")) await aGate
      return respond(path, byWs[ws])
    })
    render(<FirstRunOnboarding initial={noCounts} />)

    act(() => {
      window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_B)
      window.dispatchEvent(new Event(ACTIVE_WORKSPACE_EVENT))
    })
    await screen.findByText("0 of 4 done")
    await act(async () => releaseA())

    // A had three steps done; none of that may land under B.
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.getByText("0 of 4 done")).toBeInTheDocument()
    expect(screen.queryByText("3 of 4 done")).not.toBeInTheDocument()
  })
})

describe("HomeOverview follows the active workspace", () => {
  it("replaces the counts with the new workspace's on a switch", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    serveByActiveWorkspace({
      [WS_A]: { ...emptyWs, pipelines: 7, schedules: 7, workflows: 7 },
      [WS_B]: { ...emptyWs, pipelines: 4, schedules: 2, workflows: 3 },
    })
    render(<HomeOverview />)
    await waitFor(() => expect(cardTotal("Pipelines")).toBe("7"))

    act(() => setActiveWorkspaceId(WS_B))

    await waitFor(() => expect(cardTotal("Pipelines")).toBe("4"))
    expect(cardTotal("Scheduled models")).toBe("2")
    expect(cardTotal("Workflows")).toBe("3")
  })

  it("fills the counts once a stale workspace id self-heals", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, STALE_WS)
    serveByActiveWorkspace({ [WS_A]: { ...emptyWs, pipelines: 6, schedules: 1, workflows: 1 } })
    render(<HomeOverview />)
    // Refused under the stale id: unknown, not zero.
    await waitFor(() => expect(cardTotal("Pipelines")).toBe("—"))

    act(() => setActiveWorkspaceId(WS_A))

    await waitFor(() => expect(cardTotal("Pipelines")).toBe("6"))
  })

  it("drops counts read for a workspace the user has already left", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    const byWs: Record<string, WorkspaceData> = {
      [WS_A]: { ...emptyWs, pipelines: 9, schedules: 9, workflows: 9 },
      [WS_B]: emptyWs,
    }
    let releaseA: () => void = () => {}
    const aGate = new Promise<void>((r) => (releaseA = r))
    mockFetch.mockImplementation(async (path: string) => {
      const ws = activeId()
      if (ws === WS_A) await aGate
      return respond(path, byWs[ws])
    })
    render(<HomeOverview />)

    act(() => setActiveWorkspaceId(WS_B))
    await waitFor(() => expect(cardTotal("Pipelines")).toBe("0"))
    await act(async () => releaseA())

    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByText("9")).not.toBeInTheDocument()
    expect(cardTotal("Pipelines")).toBe("0")
  })
})

describe("RecentActivity follows the active workspace", () => {
  it("lists the new workspace's runs on a switch", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    serveByActiveWorkspace({
      [WS_A]: { ...emptyWs, runs: ["orders sync"] },
      [WS_B]: { ...emptyWs, runs: ["billing sync"] },
    })
    render(<RecentActivity />)
    expect(await screen.findByRole("link", { name: "orders sync" })).toBeInTheDocument()

    act(() => setActiveWorkspaceId(WS_B))

    expect(await screen.findByRole("link", { name: "billing sync" })).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "orders sync" })).not.toBeInTheDocument()
  })

  it("drops runs read for a workspace the user has already left", async () => {
    window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
    const byWs: Record<string, WorkspaceData> = {
      [WS_A]: { ...emptyWs, runs: ["orders sync"] },
      [WS_B]: { ...emptyWs, runs: [] },
    }
    let releaseA: () => void = () => {}
    const aGate = new Promise<void>((r) => (releaseA = r))
    mockFetch.mockImplementation(async (path: string) => {
      const ws = activeId()
      if (ws === WS_A) await aGate
      return respond(path, byWs[ws])
    })
    render(<RecentActivity />)

    act(() => setActiveWorkspaceId(WS_B))
    expect(await screen.findByText(/No runs yet/)).toBeInTheDocument()
    await act(async () => releaseA())

    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByRole("link", { name: "orders sync" })).not.toBeInTheDocument()
  })
})
