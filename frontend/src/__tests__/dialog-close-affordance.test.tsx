/**
 * THE MODAL HAD NO VISIBLE WAY OUT.
 *
 * `components/ui/dialog.tsx` is a hand-rolled primitive, not Radix. It imported
 * `X` from lucide-react on line 4 and never rendered it, and
 * `ExecutionDetailDialog` reserved `pr-8` on its title for a button that did not
 * exist — two independent traces of a close affordance that was intended and
 * lost. Escape did work, so every dialog was *closable*; none was
 * *discoverably* closable, which is the defect a user reports as "there is no
 * X". (A click outside never closed one either: `dialog-outside-click.test.tsx`.)
 *
 * The affordance is pinned on the shared primitive rather than on one dialog,
 * because all 23 call sites inherit the gap from this single component. For the
 * same reason the header's right gutter lives on `DialogHeader`: the button
 * reaches 24px into the content box, and only 3 of 20 dialog files reserved any
 * gutter at all — each on the title, which is the header line least likely to
 * collide. (Those three `pr-8`s were removed; the one on `ExecutionDetailDialog`
 * named above is history, not current code.)
 */

import { describe, it, expect, vi, beforeAll } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"

function open(onOpenChange = vi.fn()) {
  render(
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Failed run of Chat Pipeline</DialogTitle>
        </DialogHeader>
        <button type="button">Copy ID</button>
      </DialogContent>
    </Dialog>,
  )
  return onOpenChange
}

describe("every dialog offers a visible way out", () => {
  it("renders a close control with an accessible name", () => {
    open()
    expect(screen.getByRole("button", { name: /close/i })).toBeInTheDocument()
  })

  it("closes the dialog when the close control is activated", async () => {
    const onOpenChange = open()

    await userEvent.click(screen.getByRole("button", { name: /close/i }))

    expect(onOpenChange).toHaveBeenCalledTimes(1)
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it("renders exactly one close control", () => {
    // A second X — from the primitive and a hand-rolled one in the same header —
    // is the regression this fix can most plausibly introduce.
    open()
    expect(screen.getAllByRole("button", { name: /close/i })).toHaveLength(1)
  })

  it("puts the close control ahead of the dialog body, not buried under it", () => {
    // The execution dialog scrolls for 85vh. A close control that a keyboard or
    // screen-reader user only reaches after the whole run report is not an exit.
    open()
    const closeButton = screen.getByRole("button", { name: /close/i })
    const buttons = screen.getAllByRole("button")

    expect(buttons[0]).toBe(closeButton)
    expect(closeButton.compareDocumentPosition(screen.getByText("Failed run of Chat Pipeline")))
      .toBe(Node.DOCUMENT_POSITION_FOLLOWING)
  })

  it("reserves a right gutter on the header so title and description clear the button", () => {
    // Measured in a browser at 1280x800 on a default `max-w-lg` panel: the close
    // button spans x=848..880 while the content box runs to x=872, so header text
    // reaches 24px *into* the button's column. With `admin/users`' real string
    // — "Change role for {email}" with a long address — the description wraps and
    // its first line's glyphs land 4px past the button's left edge, under it.
    //
    // jsdom applies no Tailwind, so this asserts the declaration rather than the
    // geometry; the 32px-clears-24px arithmetic is what the browser measurement
    // above proves. This test is the regression guard, not the proof.
    open()
    expect(screen.getByText("Failed run of Chat Pipeline").parentElement).toHaveClass("pr-8")
  })

  it("keeps the gutter in one place — the header, never the title", () => {
    // Both would compound to a 64px inset and re-introduce the ragged headers the
    // gutter exists to prevent. Three dialogs used to reserve `pr-8` on the title.
    open()
    expect(screen.getByText("Failed run of Chat Pipeline")).not.toHaveClass("pr-8")
  })

  it("renders nothing at all while closed", () => {
    render(
      <Dialog open={false} onOpenChange={vi.fn()}>
        <DialogContent>
          <DialogTitle>Failed run of Chat Pipeline</DialogTitle>
        </DialogContent>
      </Dialog>,
    )
    expect(screen.queryByRole("button", { name: /close/i })).not.toBeInTheDocument()
  })
})

// KI-TRANSFORM-STEP-CLOSE-DISCARDS-RULES: the dialog closed on every Escape on the
// page, including one an open dropdown inside it had already handled, and one
// meant for a second dialog opened over it. Closing a dialog can be an action
// (the pipeline Transforms step's close is Skip), so an Escape aimed at a layer
// on top must close that layer only.
describe("Escape closes only the layer it was aimed at", () => {
  beforeAll(() => {
    window.ResizeObserver ||= class {
      observe() {}
      unobserve() {}
      disconnect() {}
    } as unknown as typeof ResizeObserver
  })

  it("closes the dialog when nothing is open on top of it", async () => {
    const onOpenChange = open()

    await userEvent.keyboard("{Escape}")

    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it("leaves the dialog open when the Escape closed a dropdown inside it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    const onOpenChange = vi.fn()
    render(
      <Dialog open onOpenChange={onOpenChange}>
        <DialogContent>
          <DialogTitle>Add a transform</DialogTitle>
          <Select>
            <SelectTrigger aria-label="Column">
              <SelectValue placeholder="Pick a column" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="amount">amount</SelectItem>
            </SelectContent>
          </Select>
        </DialogContent>
      </Dialog>,
    )
    await user.click(screen.getByRole("combobox", { name: "Column" }))
    await screen.findByRole("listbox")

    await user.keyboard("{Escape}")

    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull())
    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it("closes only the dialog opened last when two are open", async () => {
    const under = vi.fn()
    const over = vi.fn()
    // An inline handler, as most call sites pass: a new function every render.
    const tree = (overOpen: boolean) => (
      <>
        <Dialog open onOpenChange={(o) => under(o)}>
          <DialogContent>
            <DialogTitle>Transforms</DialogTitle>
          </DialogContent>
        </Dialog>
        <Dialog open={overOpen} onOpenChange={over}>
          <DialogContent>
            <DialogTitle>Preview</DialogTitle>
          </DialogContent>
        </Dialog>
      </>
    )
    const { rerender } = render(tree(false))
    rerender(tree(true))
    // The dialog underneath re-renders while the other is open (the step's state
    // changes under a preview); that must not lift it back on top.
    rerender(tree(true))

    await userEvent.keyboard("{Escape}")

    expect(over).toHaveBeenCalledWith(false)
    expect(under).not.toHaveBeenCalled()

    // Once the one on top is gone, Escape belongs to the one under it again.
    rerender(tree(false))
    await userEvent.keyboard("{Escape}")
    expect(under).toHaveBeenCalledWith(false)
  })
})
