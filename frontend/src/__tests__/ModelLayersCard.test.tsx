/**
 * The "Model layers" card on /workspace/settings (GET/POST/PATCH/DELETE
 * /api/v1/model-layers, handlers/workspace_layers.go).
 *
 * This file lives in src/__tests__ rather than beside the component because
 * vitest.config.ts's `include` globs do not cover components/workspace/**.
 *
 * What must hold: the list reads in the workspace's order; each edit sends exactly
 * the change it names (a move sends only the new position); a delete never happens
 * on one click and says how many models it left in no layer; the server's own
 * refusal is shown, not a generic one; and a member sees the layers with no control
 * that would only be refused.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, render, renderHook, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
}))
const workspace = { id: "ws-1" }
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspace: () => ({ activeWorkspace: { id: workspace.id } }),
}))

import { ModelLayersCard } from "@/components/workspace/ModelLayersCard"
import { useWorkspaceLayers } from "@/lib/hooks/useWorkspaceLayers"

type Layer = { id: string; name: string; description: string; position: number; model_count: number }

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as unknown as Response
}

let layers: Layer[]
let canManage: boolean
let refuse: { status: number; error: string } | null
let listStatus: number
const writes: Array<{ method: string; url: string; body: unknown }> = []

/** A small in-memory stand-in for workspace_layers.go: enough to see each call land. */
function serve() {
  authFetch.mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (method === "GET") {
      if (listStatus !== 200) return json(listStatus, { error: "failed to list layers" })
      return json(200, { layers: [...layers].reverse(), can_manage: canManage })
    }
    const body = init?.body ? JSON.parse(String(init.body)) : null
    writes.push({ method, url, body })
    if (refuse) return json(refuse.status, { error: refuse.error })
    const id = decodeURIComponent(url.split("/model-layers/")[1] ?? "")
    if (method === "POST") {
      layers.push({ id: "l-new", name: body.name, description: body.description, position: layers.length, model_count: 0 })
      return json(201, { id: "l-new" })
    }
    if (method === "PATCH") {
      const layer = layers.find((l) => l.id === id)!
      if (typeof body.position === "number") {
        const other = layers.find((l) => l.position === body.position)!
        other.position = layer.position
        layer.position = body.position
      }
      if (typeof body.name === "string") layer.name = body.name
      if (typeof body.description === "string") layer.description = body.description
      return json(200, { ok: true })
    }
    if (method === "DELETE") {
      const gone = layers.find((l) => l.id === id)!
      layers = layers.filter((l) => l.id !== id).map((l) => (l.position > gone.position ? { ...l, position: l.position - 1 } : l))
      return json(200, { deleted: true, models_unlabelled: gone.model_count })
    }
    return json(404, { error: "not found" })
  })
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  authFetch.mockReset()
  writes.length = 0
  layers = [
    { id: "l-s", name: "Silver", description: "Cleaned from one pipeline table", position: 0, model_count: 1 },
    { id: "l-g", name: "Gold", description: "", position: 1, model_count: 3 },
  ]
  canManage = true
  refuse = null
  listStatus = 200
  workspace.id = "ws-1"
  serve()
})

function rows(): string[] {
  const list = screen.getByRole("list", { name: "Model layers" })
  return within(list)
    .getAllByRole("listitem")
    .map((li) => li.querySelector("[data-layer]")!.getAttribute("data-layer")!)
}

describe("ModelLayersCard", () => {
  it("lists the layers in the workspace's order, with how many models each holds", async () => {
    render(<ModelLayersCard />)
    await screen.findByRole("list", { name: "Model layers" })
    expect(rows()).toEqual(["Silver", "Gold"])
    const silver = within(screen.getByRole("list", { name: "Model layers" })).getAllByRole("listitem")[0]
    expect(within(silver).getByText("1 model")).toBeInTheDocument()
    expect(within(silver).getByText("Cleaned from one pipeline table")).toBeInTheDocument()
    expect(screen.getByText("3 models")).toBeInTheDocument()
    // The first cannot move up, the last cannot move down.
    expect(screen.getByRole("button", { name: "Move Silver up" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Move Gold down" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Move Silver down" })).toBeEnabled()
  })

  it("adds a layer with its name and description, and lists it last", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ModelLayersCard />)
    await user.click(await screen.findByRole("button", { name: "Add layer" }))
    const form = screen.getByRole("form", { name: "Add a layer" })
    await user.type(within(form).getByLabelText("Name"), "Marts")
    await user.type(within(form).getByLabelText("Description (optional)"), "What a team reads")
    await user.click(within(form).getByRole("button", { name: "Add layer" }))

    await waitFor(() => expect(rows()).toEqual(["Silver", "Gold", "Marts"]))
    expect(writes).toEqual([
      { method: "POST", url: expect.stringMatching(/\/api\/v1\/model-layers$/), body: { name: "Marts", description: "What a team reads" } },
    ])
    expect(screen.queryByRole("form", { name: "Add a layer" })).not.toBeInTheDocument()
  })

  it("shows the server's refusal and keeps what was typed", async () => {
    refuse = { status: 409, error: "a layer named Gold already exists in this workspace" }
    const user = userEvent.setup({ delay: null })
    render(<ModelLayersCard />)
    await user.click(await screen.findByRole("button", { name: "Add layer" }))
    const form = screen.getByRole("form", { name: "Add a layer" })
    await user.type(within(form).getByLabelText("Name"), "gold")
    await user.click(within(form).getByRole("button", { name: "Add layer" }))

    expect(await within(form).findByRole("alert")).toHaveTextContent("a layer named Gold already exists in this workspace")
    expect(within(form).getByLabelText("Name")).toHaveValue("gold")
  })

  it("renames a layer in place", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ModelLayersCard />)
    await user.click(await screen.findByRole("button", { name: "Edit Gold" }))
    const form = screen.getByRole("form", { name: "Edit the layer" })
    expect(within(form).getByLabelText("Name")).toHaveValue("Gold")
    await user.clear(within(form).getByLabelText("Name"))
    await user.type(within(form).getByLabelText("Name"), "Marts")
    await user.click(within(form).getByRole("button", { name: "Save" }))

    await waitFor(() => expect(rows()).toEqual(["Silver", "Marts"]))
    expect(writes).toEqual([
      { method: "PATCH", url: expect.stringMatching(/\/api\/v1\/model-layers\/l-g$/), body: { name: "Marts", description: "" } },
    ])
  })

  it("moves a layer by sending only its new place", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ModelLayersCard />)
    await user.click(await screen.findByRole("button", { name: "Move Gold up" }))

    await waitFor(() => expect(rows()).toEqual(["Gold", "Silver"]))
    expect(writes).toEqual([{ method: "PATCH", url: expect.stringMatching(/\/l-g$/), body: { position: 0 } }])
  })

  it("deletes only after a confirmation that says what happens to the models", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ModelLayersCard />)
    await user.click(await screen.findByRole("button", { name: "Delete Gold" }))

    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText("Delete the layer Gold?")).toBeInTheDocument()
    expect(within(dialog).getByText(/its 3 models you can see will be in no layer/i)).toBeInTheDocument()
    expect(writes).toEqual([])

    await user.click(within(dialog).getByRole("button", { name: "Cancel" }))
    await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument())
    expect(writes).toEqual([])

    await user.click(screen.getByRole("button", { name: "Delete Gold" }))
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete layer" }))

    expect(await screen.findByText("Deleted Gold. 3 models now have no layer.")).toBeInTheDocument()
    await waitFor(() => expect(rows()).toEqual(["Silver"]))
    expect(writes).toEqual([{ method: "DELETE", url: expect.stringMatching(/\/l-g$/), body: null }])
  })

  it("shows a member the layers with no control to change them", async () => {
    canManage = false
    render(<ModelLayersCard />)
    await screen.findByRole("list", { name: "Model layers" })
    expect(rows()).toEqual(["Silver", "Gold"])
    expect(screen.getByText("Only workspace owners and admins add, rename, reorder or delete layers.")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /add layer|edit|delete|move/i })).not.toBeInTheDocument()
  })

  it("says a workspace with no layers has every model in none", async () => {
    layers = []
    render(<ModelLayersCard />)
    expect(await screen.findByText("No layers yet. Every model is in no layer.")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Add layer" })).toBeEnabled()
  })

  it("says the list failed to load rather than showing no layers", async () => {
    listStatus = 500
    render(<ModelLayersCard />)
    expect(await screen.findByRole("alert")).toHaveTextContent("failed to list layers")
    expect(screen.queryByText("No layers yet. Every model is in no layer.")).not.toBeInTheDocument()
  })

  it("stops adding at the server's limit of 20 layers", async () => {
    layers = Array.from({ length: 20 }, (_, i) => ({ id: `l-${i}`, name: `L${i}`, description: "", position: i, model_count: 0 }))
    render(<ModelLayersCard />)
    expect(await screen.findByRole("button", { name: "Add layer" })).toBeDisabled()
    expect(screen.getByText("A workspace can have at most 20 layers.")).toBeInTheDocument()
  })
})

describe("useWorkspaceLayers", () => {
  it("never shows one workspace's layers under another, even when the first answer lands last", async () => {
    let answerFirst: (r: Response) => void = () => {}
    authFetch.mockReset()
    authFetch.mockImplementationOnce(() => new Promise<Response>((resolve) => (answerFirst = resolve)))
    authFetch.mockImplementationOnce(async () =>
      json(200, { layers: [{ id: "l-m", name: "Marts", description: "", position: 0, model_count: 0 }], can_manage: false })
    )

    const { result, rerender } = renderHook(() => useWorkspaceLayers())
    expect(result.current.layers).toBeNull()

    workspace.id = "ws-2"
    rerender()
    await waitFor(() => expect(result.current.layers?.map((l) => l.name)).toEqual(["Marts"]))

    // ws-1's answer arrives after the switch: dropped.
    await act(async () => {
      answerFirst(json(200, { layers: [{ id: "l-s", name: "Silver", description: "", position: 0, model_count: 1 }], can_manage: true }))
      await new Promise((r) => setTimeout(r, 0))
    })
    expect(result.current.layers?.map((l) => l.name)).toEqual(["Marts"])
    expect(result.current.canManage).toBe(false)
  })

  it("reads as not loaded, not as the old list, right after the workspace changes", async () => {
    const { result, rerender } = renderHook(() => useWorkspaceLayers())
    await waitFor(() => expect(result.current.layers?.map((l) => l.name)).toEqual(["Silver", "Gold"]))

    authFetch.mockImplementation(() => new Promise<Response>(() => {}))
    workspace.id = "ws-2"
    rerender()
    expect(result.current.layers).toBeNull()
    expect(result.current.canManage).toBe(false)
  })

  it("keeps the list it had when a reload fails, and says so", async () => {
    const { result } = renderHook(() => useWorkspaceLayers())
    await waitFor(() => expect(result.current.layers).toHaveLength(2))
    listStatus = 500
    await result.current.reload()
    await waitFor(() => expect(result.current.error).toBe("failed to list layers"))
    expect(result.current.layers?.map((l) => l.name)).toEqual(["Silver", "Gold"])
  })
})
