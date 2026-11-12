import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react"

import WorkspaceSettingsPage from "@/app/(dashboard)/workspace/settings/page"
import { WorkspaceGeneralSettings } from "@/components/workspace/WorkspaceGeneralSettings"
import { useWorkspace } from "@/contexts/WorkspaceContext"
import { authFetch } from "@/lib/api/auth-fetch"

// ---------------------------------------------------------------------------
// Launch review, PR5: workspace settings.
//
// The caller's role took a banner row of its own, six tabs ran off a laptop
// screen, Save looked broken before any edit, and the plan was a page away.
// These render the page and the real General tab and read what a user sees:
// the role beside the title, wrapping tabs, a Save that is outlined until
// there is something to save, and label-left rows with a Plan row.
// ---------------------------------------------------------------------------

vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspace: vi.fn() }))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
const router = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => router, useSearchParams: () => new URLSearchParams() }))
const panel = vi.hoisted(() => ({ state: "on" as "loading" | "on" | "off" }))
vi.mock("@/config/features", () => ({ useUsagePanelState: () => panel.state }))
// The page's other tabs are tested on their own.
vi.mock("@/components/workspace/WorkspaceMembers", () => ({ WorkspaceMembers: () => null }))
vi.mock("@/components/workspace/WorkspaceRolesMatrix", () => ({ WorkspaceRolesMatrix: () => null }))

const mockUseWorkspace = useWorkspace as unknown as Mock
const mockFetch = authFetch as unknown as Mock

const usage = vi.hoisted(() => ({ fail: false, body: {} as Record<string, unknown> }))

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body } as unknown as Response
}

function serve() {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.endsWith("/auth/me")) return res({ user_id: "u-self" })
    if (url === "/api/v1/usage") return usage.fail ? res({}, 500) : res(usage.body)
    return res({})
  })
}

function ctx(role: string) {
  return {
    workspaces: [],
    activeWorkspace: { id: "ws-1", name: "Analytics", slug: "analytics", role, is_personal: false },
    role,
    isLoading: false,
    error: false,
    refresh: vi.fn(),
    can: () => true,
    meets: () => true,
  }
}

async function openPage(role = "owner") {
  mockUseWorkspace.mockReturnValue(ctx(role))
  await act(async () => {
    render(<WorkspaceSettingsPage />)
  })
}

async function openGeneral(currentRole = "owner") {
  mockUseWorkspace.mockReturnValue(ctx(currentRole))
  await act(async () => {
    render(<WorkspaceGeneralSettings workspaceId="ws-1" workspaceName="Analytics" slug="analytics" currentRole={currentRole} />)
  })
}

const classes = (el: Element) => el.className.split(/\s+/)

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})
beforeEach(() => {
  mockFetch.mockReset()
  mockUseWorkspace.mockReset()
  panel.state = "on"
  usage.fail = false
  usage.body = { plan: "pro", pipeline_limit: null, pipelines_used: 14 }
  serve()
})
afterEach(() => cleanup())

describe("workspace settings — header and tabs", () => {
  it("the role is a badge beside the title, not a banner row", async () => {
    await openPage("owner")
    const title = screen.getByRole("heading", { level: 1, name: "Workspace settings" })
    const badge = within(title.parentElement as HTMLElement).getByTitle("Your role in this workspace")
    expect(badge.textContent).toBe("Your role: Owner")
    expect(screen.queryByText(/Your role in this workspace:/)).toBeNull()
    expect(screen.queryByText(/read-only/)).toBeNull()
  })

  it("a viewer still reads that their access is read-only", async () => {
    await openPage("viewer")
    expect(screen.getByTitle("Your role in this workspace").textContent).toBe("Your role: Viewer")
    expect(screen.getByText(/Your access is read-only; ask an admin for more\./)).toBeTruthy()
  })

  it("the six tabs wrap rather than run off a laptop screen", async () => {
    await openPage()
    const tabs = screen.getByRole("tablist", { name: "Workspace settings" })
    expect(within(tabs).getAllByRole("tab")).toHaveLength(6)
    expect(classes(tabs)).toEqual(expect.arrayContaining(["h-auto", "flex-wrap", "justify-start"]))
  })
})

describe("workspace settings — General", () => {
  it("Save is outlined until the name changes, solid while it differs, outlined again once it matches", async () => {
    await openGeneral()
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    expect(classes(save)).toContain("bg-white")
    expect(classes(save)).not.toContain("bg-zinc-900")

    const name = screen.getByLabelText("Workspace name")
    fireEvent.change(name, { target: { value: "Analytics EU" } })
    expect(save.disabled).toBe(false)
    expect(classes(save)).toContain("bg-zinc-900")

    fireEvent.change(name, { target: { value: "Analytics" } })
    expect(classes(save)).toContain("bg-white")
  })

  it("each setting is a row: its name and what it does on the left, the control on the right", async () => {
    await openGeneral()
    const input = screen.getByLabelText("Workspace name")
    const hint = screen.getByText("Shown in the workspace switcher at the top of every page.")
    expect(input.getAttribute("aria-describedby")).toBe(hint.id)

    const row = (label: string) => screen.getByText(label, { selector: "label, p" }).closest(".grid") as HTMLElement
    const nameRow = row("Workspace name")
    expect(classes(nameRow)).toContain("@2xl:grid-cols-[minmax(0,15rem)_minmax(0,1fr)]")
    const [left, right] = Array.from(nameRow.children)
    expect(left.contains(hint)).toBe(true)
    expect(right.contains(input)).toBe(true)

    const leave = row("Leave workspace")
    expect(within(leave).getByText(/^You lose access to its pipelines and dashboards\./)).toBeTruthy()
    expect(within(leave).getByRole("button", { name: "Leave workspace" })).toBeTruthy()
  })

  it("a Plan row says the plan and its pipelines, and links to Usage", async () => {
    await openGeneral()
    expect(await screen.findByText("Pro · 14 pipelines")).toBeTruthy()
    expect(screen.getByText("What this workspace can use.")).toBeTruthy()
    expect(screen.getByRole("link", { name: "View usage" }).getAttribute("href")).toBe("/usage")
  })

  it("on a plan with a limit, the row counts against it", async () => {
    usage.body = { plan: "free", pipeline_limit: 2, pipelines_used: 1 }
    await openGeneral()
    expect(await screen.findByText("Free · 1 of 2 pipelines")).toBeTruthy()
  })

  it("says the plan did not load rather than leaving the row blank", async () => {
    usage.fail = true
    await openGeneral()
    expect(await screen.findByText("The plan did not load.")).toBeTruthy()
    expect(screen.getByRole("link", { name: "View usage" })).toBeTruthy()
  })

  it.each(["off", "loading"] as const)("where the usage panel is %s, there is no Plan row and no usage read", async (state) => {
    panel.state = state
    await openGeneral()
    expect(screen.queryByText("Plan")).toBeNull()
    expect(screen.queryByRole("link", { name: "View usage" })).toBeNull()
    expect(mockFetch.mock.calls.some(([url]) => url === "/api/v1/usage")).toBe(false)
  })
})
