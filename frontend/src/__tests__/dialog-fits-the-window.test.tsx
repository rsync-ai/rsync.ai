/**
 * A DIALOG TALLER THAN THE WINDOW COULD NOT BE SCROLLED (N-5, prod 2026-10-10).
 *
 * An open dialog locks the page's scroll (`dialog.tsx` sets `overflow: hidden`
 * on the body), and the panel had no height limit of its own, so a dialog
 * taller than the window was cut off at both ends: Add check (568 px) in a
 * 392 px-tall window could not reach its Name field, Test, Cancel or Add check.
 * The panel now stops at the window, less its padding, and scrolls; a dialog
 * that sets its own height or scrolls an inner region keeps doing so.
 */

import { describe, it, expect } from "vitest"
import { render, screen } from "@testing-library/react"
import "@testing-library/jest-dom"

import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"

function open(className?: string) {
  render(
    <Dialog open onOpenChange={() => {}}>
      <DialogContent className={className}>
        <DialogTitle>Add check</DialogTitle>
      </DialogContent>
    </Dialog>,
  )
  return screen.getByRole("dialog")
}

describe("a dialog fits the window", () => {
  it("stops at the window's height and scrolls", () => {
    const panel = open()
    expect(panel).toHaveClass("max-h-[calc(100dvh-2rem)]", "overflow-y-auto")
  })

  it("keeps a call site's own height and inner scrolling", () => {
    const panel = open("max-h-[85vh] flex flex-col overflow-hidden")
    expect(panel).toHaveClass("max-h-[85vh]", "overflow-hidden")
    expect(panel).not.toHaveClass("max-h-[calc(100dvh-2rem)]")
    expect(panel).not.toHaveClass("overflow-y-auto")
  })
})
