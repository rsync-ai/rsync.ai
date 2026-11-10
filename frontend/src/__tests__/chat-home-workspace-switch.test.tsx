import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, render, screen, waitFor } from "@testing-library/react"
import type { Mock } from "vitest"

import { AgenticPipelineHome } from "@/components/chat/AgenticPipelineHome"
import { shortConnectorName } from "@/components/chat/home/suggestions"
import { authFetch } from "@/lib/api/auth-fetch"
import { ACTIVE_WORKSPACE_EVENT, ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// Found on prod: after switching demo → personal, /chat kept rendering demo's
// connections (Demo Mysql / Demo Postgresql) and demo's pipelines — each with a
// live Run button — under the personal workspace's name in the header. It
// persisted indefinitely, because the hero fetched once on mount with an empty
// dependency array and never subscribed to the workspace-change signal. Ten list
// surfaces were converted in #712; this one was missed.
//
// The page no longer lists connections or pipelines by name (Pipelines and Runs
// do): what it shows from the connections is the Quick Pipelines cards, one per
// connected source → destination type pair. So each fixture's types make a pair
// that is not a curated template, and the tests watch that pair's card.
//
// These pin both halves of the contract every workspace-scoped list owes:
//   1. refetch when the active workspace changes (same tab AND cross tab), and
//      clear first so the previous tenant's rows don't linger during the refetch
//   2. drop a response that a switch overtook mid-flight
//
// Blast radius was display-only — creating from another tenant's pair would fail
// server-side — but the cards are still another tenant's setup on screen.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/chat",
}))
vi.mock("framer-motion", () => ({
  motion: new Proxy({} as Record<string, unknown>, {
    get: () => (props: Record<string, unknown>) => {
      const { children, ...rest } = props as { children?: React.ReactNode }
      void rest
      return <div>{children}</div>
    },
  }),
}))
vi.mock("@/components/connectors/ConnectionLogo", () => ({
  ConnectionLogo: () => <span />,
}))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

const DEMO_WS = "0df2f291-0000-0000-0000-000000000001"
const PERSONAL_WS = "f71b4b51-0000-0000-0000-000000000002"

type Conn = { name: string; connector_type: string; type: "source" | "destination" }
type Fixture = { connections: Conn[] }

// Neither pair is a curated template, so each card exists only because of that
// workspace's connections.
const DEMO: Fixture = {
  connections: [
    { name: "Demo Mysql", connector_type: "mysql", type: "source" },
    { name: "Demo BigQuery", connector_type: "bigquery", type: "destination" },
  ],
}
const PERSONAL: Fixture = {
  connections: [
    { name: "Personal Postgres", connector_type: "postgresql", type: "source" },
    { name: "Personal Bucket", connector_type: "gcs", type: "destination" },
  ],
}
const DEMO_CARD = `${shortConnectorName("mysql")} → ${shortConnectorName("bigquery")}`
const PERSONAL_CARD = `${shortConnectorName("postgresql")} → ${shortConnectorName("gcs")}`

function conn(c: Conn, i: number) {
  return { id: `conn-${c.name}-${i}`, ...c, status: "active" }
}

/** Serves whichever fixture matches the workspace currently in localStorage. */
function serveByActiveWorkspace(byWorkspace: Record<string, Fixture>) {
  ;(authFetch as Mock).mockImplementation(async (url: string) => {
    const active = window.localStorage.getItem(ACTIVE_WORKSPACE_KEY) ?? ""
    const fx = byWorkspace[active] ?? { connections: [] }
    if (String(url).includes("/connections")) {
      return {
        ok: true,
        status: 200,
        json: async () => ({ connections: fx.connections.map(conn) }),
      }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
}

/** Switches the active workspace the way the header does: write, then announce. */
function switchWorkspace(id: string) {
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, id)
  window.dispatchEvent(new Event(ACTIVE_WORKSPACE_EVENT))
}

beforeEach(() => {
  window.localStorage.clear()
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, DEMO_WS)
  vi.clearAllMocks()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe("AgenticPipelineHome — active workspace", () => {
  it("replaces the previous workspace's suggestions on switch", async () => {
    serveByActiveWorkspace({ [DEMO_WS]: DEMO, [PERSONAL_WS]: PERSONAL })
    render(<AgenticPipelineHome onSubmit={() => {}} onPrefill={() => {}} onCustom={() => {}} />)

    await screen.findByText(DEMO_CARD)

    await act(async () => {
      switchWorkspace(PERSONAL_WS)
    })

    await screen.findByText(PERSONAL_CARD)
    // The exact prod symptom: demo's setup still on screen under personal.
    expect(screen.queryByText(DEMO_CARD)).toBeNull()
  })

  it("refetches when the workspace changes in another tab", async () => {
    serveByActiveWorkspace({ [DEMO_WS]: DEMO, [PERSONAL_WS]: PERSONAL })
    render(<AgenticPipelineHome onSubmit={() => {}} onPrefill={() => {}} onCustom={() => {}} />)
    await screen.findByText(DEMO_CARD)

    // Cross-tab: the other tab writes localStorage, this tab only sees `storage`.
    await act(async () => {
      window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, PERSONAL_WS)
      window.dispatchEvent(new StorageEvent("storage", { key: ACTIVE_WORKSPACE_KEY }))
    })

    await screen.findByText(PERSONAL_CARD)
    expect(screen.queryByText(DEMO_CARD)).toBeNull()
  })

  it("drops a response that a switch overtook mid-flight", async () => {
    // demo's fetch is held open, then released AFTER the switch to personal. Its
    // connections must never reach the screen: authFetch stamped X-Workspace-ID at call
    // time, so this response is demo's data arriving under personal.
    let releaseDemo: (() => void) | undefined
    const demoInFlight = new Promise<void>((resolve) => {
      releaseDemo = resolve
    })

    ;(authFetch as Mock).mockImplementation(async (url: string) => {
      const active = window.localStorage.getItem(ACTIVE_WORKSPACE_KEY) ?? ""
      if (active === DEMO_WS) await demoInFlight
      const fx = active === DEMO_WS ? DEMO : PERSONAL
      if (String(url).includes("/connections")) {
        return { ok: true, status: 200, json: async () => ({ connections: fx.connections.map(conn) }) }
      }
      return { ok: true, status: 200, json: async () => ({}) }
    })

    render(<AgenticPipelineHome onSubmit={() => {}} onPrefill={() => {}} onCustom={() => {}} />)

    await act(async () => {
      switchWorkspace(PERSONAL_WS)
    })
    await screen.findByText(PERSONAL_CARD)

    await act(async () => {
      releaseDemo?.()
      await Promise.resolve()
    })

    await waitFor(() => {
      expect(screen.getByText(PERSONAL_CARD)).toBeTruthy()
    })
    expect(screen.queryByText(DEMO_CARD)).toBeNull()
  })
})
