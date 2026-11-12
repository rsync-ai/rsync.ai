import { afterEach, describe, expect, it, vi } from "vitest"
import { act, render, screen } from "@testing-library/react"

import { DashboardShell, shellLayoutFor } from "@/components/layout/DashboardShell"
import { useAssistantStore } from "@/lib/store/useAssistantStore"

// How the shell frames a page. A tool (Data Explorer, chat, lineage, transforms) is
// a workspace: edge to edge and exactly as tall as the window, so its panes split
// the whole area and scroll on their own. Every other page is a padded column that
// scrolls with the window. Either way the frame is the `@container` page content
// lays itself out against — the box the sidebar and the docked assistant take from.

const nav = vi.hoisted(() => ({ pathname: "/" as string | null }))
vi.mock("next/navigation", () => ({ usePathname: () => nav.pathname }))

afterEach(() => {
  nav.pathname = "/"
  act(() => useAssistantStore.setState({ open: false, expanded: false }))
})

describe("shellLayoutFor", () => {
  it.each([
    ["/explorer", "workspace"],
    ["/explorer/", "workspace"],
    ["/explorer/lineage", "workspace"],
    ["/chat", "workspace"],
    // A redirect now (to a pipeline's Transforms tab), so no workspace frame.
    ["/transforms", "page"],
    // A detail page under a workspace route is an ordinary page.
    ["/models", "page"],
    ["/models/3f2b8c1e-5a6d-4e7f-9a0b-1c2d3e4f5a6b", "page"],
    ["/connectors", "page"],
    ["/pipelines/new", "page"],
    ["/", "page"],
    [null, "page"],
  ] as const)("%s → %s", (path, layout) => {
    expect(shellLayoutFor(path)).toBe(layout)
  })
})

describe("DashboardShell", () => {
  it("frames a tool edge to edge, exactly as tall as the window", () => {
    nav.pathname = "/explorer"
    render(<DashboardShell>tool</DashboardShell>)
    const main = screen.getByRole("main")
    const frame = screen.getByTestId("page-frame")
    expect(main).toHaveAttribute("data-layout", "workspace")
    expect(main.className.split(/\s+/)).toEqual(expect.arrayContaining(["flex", "h-dvh", "flex-col"]))
    expect(frame.className.split(/\s+/)).toEqual(
      expect.arrayContaining(["@container", "flex-1", "min-h-0", "overflow-auto"]),
    )
    expect(frame.className).not.toMatch(/(^|\s)p-\d/)
  })

  it("frames any other page as a padded column that scrolls with the window", () => {
    nav.pathname = "/connectors"
    render(<DashboardShell>page</DashboardShell>)
    const main = screen.getByRole("main")
    const frame = screen.getByTestId("page-frame")
    expect(main).toHaveAttribute("data-layout", "page")
    expect(main.className).not.toContain("h-dvh")
    expect(frame.className.split(/\s+/)).toEqual(expect.arrayContaining(["@container", "p-4"]))
  })

  it("never lets a page scroll the window sideways", () => {
    // Clip, not hidden: a clipped frame is no scroll container, so a sticky
    // header inside it still sticks to the window as it scrolls down.
    nav.pathname = "/connectors"
    render(<DashboardShell>page</DashboardShell>)
    const frame = screen.getByTestId("page-frame")
    expect(frame.className.split(/\s+/)).toContain("overflow-x-clip")
    expect(frame.className).not.toMatch(/overflow-(x-)?hidden/)
  })

  it("puts banners above the frame, outside its padding", () => {
    nav.pathname = "/connectors"
    render(<DashboardShell banners={<div data-testid="banner" />}>page</DashboardShell>)
    const banner = screen.getByTestId("banner")
    const frame = screen.getByTestId("page-frame")
    expect(frame.contains(banner)).toBe(false)
    expect(banner.compareDocumentPosition(frame) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it("keeps the frame the container when the assistant docks", () => {
    nav.pathname = "/explorer"
    render(<DashboardShell>tool</DashboardShell>)
    const main = screen.getByRole("main")
    act(() => useAssistantStore.setState({ open: true, expanded: false }))
    expect(main.className).toContain("xl:pr-(--assistant-dock-width)")
    expect(screen.getByTestId("page-frame").className).toContain("@container")
  })
})
