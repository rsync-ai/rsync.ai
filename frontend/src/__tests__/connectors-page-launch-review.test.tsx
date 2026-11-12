import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ConnectorsPage from "@/app/(dashboard)/connectors/page"
import { fetchMCPConnectors } from "@/lib/api/mcp-connectors"

// ---------------------------------------------------------------------------
// Launch review, PR5: the connector catalog.
//
// The grid took about two seconds to appear (every card waited index * 0.05s
// after its section's own fade, and `transition-all` eased the same opacity),
// and two rows of filter buttons split one question. These render the real
// page against the catalog's shape and read what a user sees: one row of
// category chips with counts, a status toggle of the states connectors are
// actually in, a pill per card, and one short fade on the first load only.
// ---------------------------------------------------------------------------

vi.mock("@/lib/api/mcp-connectors", () => ({ fetchMCPConnectors: vi.fn(), saveConnection: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
const router = vi.hoisted(() => ({ push: vi.fn(), refresh: vi.fn(), replace: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => router }))
vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspaceRole: () => ({ role: "admin" }) }))
vi.mock("@/components/connectors/UnifiedConnectorModal", () => ({ UnifiedConnectorModal: () => null }))
vi.mock("@/components/connectors/ConnectionLogo", () => ({ ConnectionLogo: () => <span data-testid="logo" /> }))
vi.mock("@/components/connectors/ConnectionTablesCheck", () => ({ notifyTableCheck: vi.fn() }))
// A motion element that says what it was asked to do, so the fade's timing can
// be read without waiting on frames.
vi.mock("framer-motion", async () => {
  const { createElement } = await import("react")
  const made = new Map<string, unknown>()
  const motion = new Proxy(
    {},
    {
      get(_target, tag: string) {
        if (!made.has(tag)) {
          made.set(tag, function Motion({ initial, animate: _a, transition, exit: _e, ...rest }: Record<string, unknown>) {
            return createElement(tag, {
              ...rest,
              "data-motion": tag,
              "data-initial": JSON.stringify(initial),
              "data-transition": JSON.stringify(transition),
            })
          })
        }
        return made.get(tag)
      },
    },
  )
  return {
    motion,
    MotionConfig: ({ reducedMotion, children }: { reducedMotion?: string; children: React.ReactNode }) =>
      createElement("div", { "data-reduced-motion": reducedMotion }, children),
    AnimatePresence: ({ children }: { children: React.ReactNode }) => children,
  }
})

const mockFetch = fetchMCPConnectors as unknown as Mock

type Run = "running" | "stopped" | "ready"

function connector(name: string, display_name: string, category: string, run: Run) {
  return {
    name,
    display_name,
    version: "1.0.0",
    description: `${display_name} connector`,
    category,
    icon: "",
    color: "",
    capabilities: [],
    configuration_schema: {},
    supports_source: true,
    supports_destination: false,
    supports_cdc: false,
    status: "active",
    lifecycle: "ga",
    has_dockerfile: true,
    docker_deployed: run !== "ready",
    docker_status: run === "ready" ? "auto_deploys_on_use" : run,
  }
}

const CATALOG = [
  connector("postgresql", "PostgreSQL", "relational_db", "running"),
  connector("mysql", "MySQL", "relational_db", "running"),
  connector("bigquery", "BigQuery", "data_warehouse", "running"),
  connector("mongodb", "MongoDB", "document_db", "ready"),
  connector("gcs", "Cloud Storage", "cloud_storage", "running"),
  connector("stripe", "Stripe", "api_saas", "ready"),
  connector("hubspot", "HubSpot", "api_saas", "stopped"),
  connector("sample-data", "Sample Data", "sample", "running"),
]

async function open(catalog: unknown[] = CATALOG) {
  mockFetch.mockResolvedValue({ connectors: catalog })
  await act(async () => {
    render(<ConnectorsPage />)
  })
  await screen.findAllByRole("heading", { level: 3 })
}

/** Each chip's text in a filter group, label and count run together ("All8"). */
const chips = (group: string) =>
  within(screen.getByRole("group", { name: group }))
    .getAllByRole("button")
    .map((b) => b.textContent)

const chip = (group: string, name: RegExp) => within(screen.getByRole("group", { name: group })).getByRole("button", { name })

const card = (name: string) => screen.getByRole("heading", { level: 3, name }).closest("[data-motion]") as HTMLElement

const cardNames = () => screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)

// Braced: a hook that returns a function has it run as teardown, and the mock
// would hand back a pending promise for the hook to wait on.
beforeEach(() => {
  mockFetch.mockReset()
})
afterEach(() => cleanup())

describe("connectors — filters", () => {
  it("one row of category chips with counts, and a status toggle of the states connectors are in", async () => {
    await open()
    expect(chips("Category")).toEqual(["All8", "Database4", "Storage1", "API2", "Other1"])
    // Nothing is restarting and every connector has an image: no empty chips.
    expect(chips("Status")).toEqual(["All8", "Running5", "Stopped1", "Ready2"])
    expect(screen.queryByRole("button", { name: /All Categories|Available|Unavailable/ })).toBeNull()
  })

  it("a status narrows the grid and recounts the categories; a category recounts the statuses", async () => {
    const user = userEvent.setup()
    await open()

    await user.click(chip("Status", /^Ready/))
    expect(cardNames()).toEqual(["MongoDB", "Stripe"])
    expect(chips("Category")).toEqual(["All2", "Database1", "Storage0", "API1", "Other0"])

    await user.click(chip("Category", /^API/))
    expect(cardNames()).toEqual(["Stripe"])
    expect(chips("Status")).toEqual(["All2", "Running0", "Stopped1", "Ready1"])
  })

  it("a search that matches nothing says so, and Clear filters brings every connector back", async () => {
    const user = userEvent.setup()
    await open()

    await user.type(screen.getByRole("textbox", { name: "Search connectors" }), "zzz")
    expect(screen.getByText("No connectors found")).toBeTruthy()
    expect(screen.getByText("Nothing matches this category, status and search.")).toBeTruthy()

    await user.click(screen.getByRole("button", { name: "Clear filters" }))
    expect(cardNames()).toHaveLength(8)
    expect((screen.getByRole("textbox", { name: "Search connectors" }) as HTMLInputElement).value).toBe("")
  })

  // "All 0" beside the loading blocks or an error reads as an empty catalog.
  it("leaves the counts off the chips until the catalog loads", async () => {
    let answer: (value: unknown) => void = () => {}
    mockFetch.mockReturnValue(new Promise((resolve) => (answer = resolve)))
    await act(async () => {
      render(<ConnectorsPage />)
    })
    expect(chips("Category")).toEqual(["All"])
    expect(chips("Status")).toEqual(["All"])

    await act(async () => answer({ connectors: CATALOG }))
    expect(chips("Category")).toEqual(["All8", "Database4", "Storage1", "API2", "Other1"])
    expect(chips("Status")).toEqual(["All8", "Running5", "Stopped1", "Ready2"])
  })

  it("leaves the counts off the chips when the catalog did not load", async () => {
    mockFetch.mockRejectedValue(new Error("catalog unreachable"))
    await act(async () => {
      render(<ConnectorsPage />)
    })
    expect(screen.queryByRole("status")).toBeNull()
    expect(chips("Category")).toEqual(["All"])
    expect(chips("Status")).toEqual(["All"])
  })
})

describe("connectors — cards", () => {
  it("each card says where its container is, as a status pill with the reason", async () => {
    await open()
    const pill = (name: string, label: string) => within(card(name)).getByText(label)
    expect(pill("PostgreSQL", "Running").className).toContain("emerald")
    expect(pill("HubSpot", "Stopped").className).toContain("red")
    expect(pill("MongoDB", "Ready").getAttribute("title")).toBe("Starts the first time a connection uses it")
  })

  it("loads as card-shaped blocks, not a spinner", async () => {
    let answer: (value: unknown) => void = () => {}
    mockFetch.mockReturnValue(new Promise((resolve) => (answer = resolve)))
    await act(async () => {
      render(<ConnectorsPage />)
    })
    expect(screen.getByRole("status").textContent).toContain("Loading connectors…")
    expect(document.querySelector(".animate-spin")).toBeNull()
    await act(async () => answer({ connectors: CATALOG }))
    expect(screen.queryByRole("status")).toBeNull()
  })
})

describe("connectors — motion", () => {
  const many = Array.from({ length: 10 }, (_, i) => connector(`db${i}`, `Database ${i}`, "relational_db", "running"))

  it("one 150ms fade, the first six cards 30ms apart and the rest with the sixth", async () => {
    await open(many)
    const transitions = cardNames().map((n) => JSON.parse(card(n as string).dataset.transition as string))
    expect(transitions.every((t) => t.duration === 0.15)).toBe(true)
    const delays = transitions.map((t) => Math.round(t.delay * 100) / 100)
    expect(delays).toEqual([0, 0.03, 0.06, 0.09, 0.12, 0.15, 0.18, 0.18, 0.18, 0.18])
    expect(JSON.parse(card("Database 0").dataset.initial as string)).toEqual({ opacity: 0 })
    // The section around them does not fade on its own.
    expect(screen.getByRole("region", { name: "Database" }).closest("[data-motion]")).toBeNull()
  })

  it("fades on the first load only: cards a filter brings back appear at once", async () => {
    const user = userEvent.setup()
    await open()
    expect(card("Cloud Storage").dataset.initial).toBe(JSON.stringify({ opacity: 0 }))

    await user.click(chip("Category", /^Database/))
    await user.click(chip("Category", /^Storage/))
    expect(card("Cloud Storage").dataset.initial).toBe("false")
  })

  it("honours reduced motion, and the hover eases only the border and shadow", async () => {
    await open()
    expect(document.querySelector("[data-reduced-motion]")?.getAttribute("data-reduced-motion")).toBe("user")
    const classes = card("PostgreSQL").className.split(/\s+/)
    expect(classes).toContain("transition-[border-color,box-shadow]")
    expect(classes).not.toContain("transition-all")
  })
})
