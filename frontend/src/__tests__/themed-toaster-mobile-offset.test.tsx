/**
 * At ≤600 px sonner ignores `offset` and places the toaster by `mobileOffset`,
 * whose default bottom is 16 px — the height of a form's bottom action row. On
 * the connector form at ~470 px the Start & Test toasts sat on Save Connection.
 * The 96 px desktop clearance has to hold on a narrow window too.
 *
 * jsdom applies no media queries, so this pins the variable sonner's
 * `@media (max-width: 600px)` rule reads.
 */

import { afterEach, describe, expect, it, vi } from "vitest"
import { act, cleanup, render } from "@testing-library/react"
import { toast } from "sonner"

vi.mock("next-themes", () => ({ useTheme: () => ({ resolvedTheme: "light" }) }))

import { ThemedToaster } from "@/components/providers/ThemedToaster"

afterEach(() => {
  act(() => {
    toast.dismiss()
  })
  cleanup()
})

describe("ThemedToaster", () => {
  it("keeps toasts 96 px off the bottom on a narrow window too", async () => {
    const { findByText } = render(<ThemedToaster />)
    act(() => {
      toast("hello")
    })
    await findByText("hello")
    const list = document.querySelector<HTMLElement>("[data-sonner-toaster]")!
    expect(list.style.getPropertyValue("--mobile-offset-bottom")).toBe("96px")
    // Control: the desktop offset the narrow one now matches.
    expect(list.style.getPropertyValue("--offset-bottom")).toBe("96px")
  })
})
