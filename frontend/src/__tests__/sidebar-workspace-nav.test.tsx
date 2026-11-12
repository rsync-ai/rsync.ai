import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen } from "@testing-library/react"

import { Sidebar } from "@/components/layout/Sidebar"

// P0 nav: the sidebar gains a "Workspace" entry pointing at the settings hub. The
// "Admin" section stays gated on the PLATFORM role (auth/me role), which is distinct
// from a workspace role even though both can be the string "admin".

let pathname = "/"
vi.mock("next/navigation", () => ({ usePathname: () => pathname }))
vi.mock("@/lib/store/useUIStore", () => ({
  useUIStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({
      sidebarCollapsed: false,
      sidebarOpen: false,
      setSidebarOpen: () => {},
      toggleSidebarCollapsed: () => {},
    }),
}))

beforeEach(() => {
  vi.clearAllMocks()
  pathname = "/"
})
afterEach(() => vi.restoreAllMocks())

describe("Sidebar — workspace nav", () => {
  it("renders a Workspace link to the settings hub", () => {
    render(<Sidebar role="viewer" />)
    const link = screen.getByRole("link", { name: /workspace/i })
    expect(link).toHaveAttribute("href", "/workspace/settings")
  })

  it("hides the Admin section for a non-platform-admin", () => {
    render(<Sidebar role="member" />)
    expect(screen.queryByRole("link", { name: /^admin$/i })).toBeNull()
  })

  it("shows the Admin section for a platform admin", () => {
    render(<Sidebar role="admin" />)
    expect(screen.getByRole("link", { name: /^admin$/i })).toBeInTheDocument()
  })
})

describe("Sidebar — fits above the fold (#56)", () => {
  it("groups the links under four headings instead of eight", () => {
    render(<Sidebar role="admin" />)
    const headings = screen.getAllByRole("heading", { level: 4 }).map((h) => h.textContent)
    expect(headings).toEqual(["Build", "Analyze", "Data", "Manage"])
  })

  it("keeps every destination, Settings and Admin in the same group as Workspace", () => {
    render(<Sidebar role="admin" />)
    const names = screen.getAllByRole("link").map((a) => a.getAttribute("aria-label")).filter(Boolean)
    expect(names).toEqual(
      expect.arrayContaining([
        "Home", "Runs", "Pipelines", "Models", "Workflows", "Explorer",
        "Lineage", "Connections", "Connectors", "Workspace", "Settings", "Admin",
      ]),
    )
    const manage = screen.getByRole("heading", { name: "Manage" }).parentElement as HTMLElement
    const inManage = Array.from(manage.querySelectorAll("a")).map((a) => a.getAttribute("aria-label"))
    expect(inManage).toEqual(expect.arrayContaining(["Workspace", "Settings", "Admin"]))
  })
})

// PII Management shipped with a working /pii page and no way to reach it: the nav
// check above is an `arrayContaining`, so a missing row passes it. These assert the
// row by name, which is the failure the loose check could not produce.
describe("Sidebar — PII Management is reachable from the nav", () => {
  it("links to /pii from the Data group", () => {
    render(<Sidebar role="admin" />)
    expect(screen.getByRole("link", { name: "PII Management" })).toHaveAttribute("href", "/pii")
    const data = screen.getByRole("heading", { name: "Data" }).parentElement as HTMLElement
    const inData = Array.from(data.querySelectorAll("a")).map((a) => a.getAttribute("aria-label"))
    expect(inData).toContain("PII Management")
  })

  it("shows the row to a non-admin, because /pii carries no role gate of its own", () => {
    render(<Sidebar role="viewer" />)
    expect(screen.getByRole("link", { name: "PII Management" })).toBeInTheDocument()
  })
})

// Workflows run several steps (trigger, query, condition, one or more channels),
// so its icon is the multi-node one, not lucide's two-box Workflow.
describe("Sidebar — Workflows", () => {
  it("links to /workflows with the multi-node icon", () => {
    render(<Sidebar role="viewer" />)
    const link = screen.getByRole("link", { name: "Workflows" })
    expect(link).toHaveAttribute("href", "/workflows")
    const icon = link.querySelector("svg")!
    expect(icon).toHaveClass("lucide-workflow-nodes")
    expect(icon.querySelectorAll("rect")).toHaveLength(4)
  })
})

// Home, Pipelines, Executions and "Data Pipeline" showed the same pipelines four
// ways. Build lists the three things that run; Runs lists all of their runs; the
// builder (/chat) and the old Executions list have no row of their own.
describe("Sidebar — Build and Runs", () => {
  const groupLinks = (heading: string) => {
    const group = screen.getByRole("heading", { name: heading }).parentElement as HTMLElement
    return Array.from(group.querySelectorAll("a")).map((a) => a.getAttribute("aria-label"))
  }

  it("puts pipelines, models and workflows under Build, in that order", () => {
    render(<Sidebar role="viewer" />)
    expect(groupLinks("Build")).toEqual(["Pipelines", "Models", "Workflows"])
  })

  // dashboards-design.md § 7.4: Analyze is where data is read, between Build and
  // Data: Dashboards, Charts, then the Explorer.
  it("puts Dashboards, Charts and the Explorer under Analyze, and keeps Lineage under Data", () => {
    render(<Sidebar role="viewer" />)
    expect(groupLinks("Analyze")).toEqual(["Dashboards", "Charts", "Explorer"])
    expect(screen.getByRole("link", { name: "Dashboards" })).toHaveAttribute("href", "/dashboards")
    expect(screen.getByRole("link", { name: "Charts" })).toHaveAttribute("href", "/charts")
    expect(groupLinks("Data")).toEqual(["Lineage", "PII Management", "Connections", "Connectors"])
  })

  it("links Runs to /runs and has no Executions, All Pipelines or Data Pipeline row", () => {
    render(<Sidebar role="viewer" />)
    expect(screen.getByRole("link", { name: "Runs" })).toHaveAttribute("href", "/runs")
    for (const gone of ["Executions", "All Pipelines", "Data Pipeline"]) {
      expect(screen.queryByRole("link", { name: gone })).toBeNull()
    }
    expect(document.querySelector('a[href="/chat"]')).toBeNull()
  })

  it.each([
    ["/executions/abc-123", "Runs"],
    ["/runs", "Runs"],
    ["/chat", "Pipelines"],
    ["/pipelines/p1", "Pipelines"],
    ["/models/q1", "Models"],
    ["/explorer/lineage", "Lineage"],
    ["/explorer", "Explorer"],
  ])("on %s only the %s row is lit", (path, row) => {
    pathname = path
    render(<Sidebar role="viewer" />)
    const lit = screen
      .getAllByRole("link")
      .filter((a) => a.className.includes("bg-zinc-100"))
      .map((a) => a.getAttribute("aria-label"))
    expect(lit).toEqual([row])
  })
})
