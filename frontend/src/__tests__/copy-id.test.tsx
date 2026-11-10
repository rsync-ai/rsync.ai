/**
 * The id chip in a run's and a pipeline's header (components/kit/CopyId.tsx).
 * A self-host on plain HTTP has no navigator.clipboard: the chip must say it
 * could not copy rather than do nothing, and "Copied" must be announced from
 * outside the button, whose accessible name is its aria-label.
 */

import { act, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
vi.mock("sonner", () => ({ toast }))

import { CopyId } from "@/components/kit/CopyId"

const ID = "6f0d3c2a-9b1e-4c7d-8a55-0e2f4b6c8d10"

afterEach(() => {
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe("CopyId", () => {
  it("copies the whole id and announces it outside the button", async () => {
    const writeText = vi.fn(() => Promise.resolve())
    vi.stubGlobal("navigator", { clipboard: { writeText } })
    render(<CopyId value={ID} label="run ID" />)
    const button = screen.getByRole("button", { name: `Copy run ID ${ID}` })
    expect(button).toHaveTextContent(ID.slice(0, 8))

    await act(async () => fireEvent.click(button))
    expect(writeText).toHaveBeenCalledWith(ID)
    const status = screen.getByText("Copied")
    expect(status).toHaveAttribute("aria-live", "polite")
    expect(button.contains(status)).toBe(false)
    expect(toast.error).not.toHaveBeenCalled()
  })

  it("shows the whole id with full", () => {
    render(<CopyId value={ID} label="workspace ID" full />)
    expect(screen.getByRole("button", { name: `Copy workspace ID ${ID}` })).toHaveTextContent(ID)
  })

  it("says it could not copy on an origin with no clipboard", async () => {
    vi.stubGlobal("navigator", {})
    render(<CopyId value={ID} label="run ID" />)
    await act(async () => fireEvent.click(screen.getByRole("button", { name: `Copy run ID ${ID}` })))
    expect(toast.error).toHaveBeenCalledWith("Could not copy the run ID")
    expect(screen.queryByText("Copied")).toBeNull()
  })
})
