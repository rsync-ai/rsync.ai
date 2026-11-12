import { describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen } from "@testing-library/react"
import { useState } from "react"

import { useSuggestionTab } from "@/hooks/useSuggestionTab"

// Tab writes an AI box's suggestion in, only into an empty box and only on a
// plain Tab. Every other case must fall through to the browser, so the keyboard
// can always leave the box. `fireEvent.keyDown` returns false when the handler
// called preventDefault: that is "Tab was taken".

function Box({ suggestion, initial = "" }: { suggestion: string | null; initial?: string }) {
  const [value, setValue] = useState(initial)
  const tab = useSuggestionTab({ value, suggestion, onAccept: setValue })
  return (
    <>
      <textarea aria-label="prompt" value={value} onChange={(e) => setValue(e.target.value)} onKeyDown={tab.onKeyDown} />
      {tab.showHint && <span>hint</span>}
    </>
  )
}

const box = () => screen.getByLabelText("prompt") as HTMLTextAreaElement
/** True when the hook took the key. */
const press = (init: Parameters<typeof fireEvent.keyDown>[1]) => !fireEvent.keyDown(box(), init)

describe("useSuggestionTab", () => {
  it("takes a plain Tab in an empty box, writes the trimmed suggestion and puts the cursor at the end", () => {
    const raf = vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => {
      cb(0)
      return 0
    })
    render(<Box suggestion="  Sync Postgres to BigQuery " />)
    box().focus()
    expect(screen.getByText("hint")).toBeTruthy()

    expect(press({ key: "Tab" })).toBe(true)
    expect(box().value).toBe("Sync Postgres to BigQuery")
    expect(box().selectionStart).toBe("Sync Postgres to BigQuery".length)
    expect(screen.queryByText("hint")).toBeNull()

    // The box has text now: a second Tab leaves it.
    expect(press({ key: "Tab" })).toBe(false)
    raf.mockRestore()
  })

  it("treats a box holding only spaces as empty", () => {
    render(<Box suggestion="Sync" initial="   " />)
    expect(press({ key: "Tab" })).toBe(true)
    expect(box().value).toBe("Sync")
  })

  it("leaves Tab alone once the user has typed", () => {
    render(<Box suggestion="Sync" initial="my own words" />)
    expect(screen.queryByText("hint")).toBeNull()
    expect(press({ key: "Tab" })).toBe(false)
    expect(box().value).toBe("my own words")
  })

  it("leaves Tab alone with no suggestion, or a blank one", () => {
    const { unmount } = render(<Box suggestion={null} />)
    expect(press({ key: "Tab" })).toBe(false)
    expect(screen.queryByText("hint")).toBeNull()
    unmount()

    render(<Box suggestion="   " />)
    expect(press({ key: "Tab" })).toBe(false)
    expect(screen.queryByText("hint")).toBeNull()
  })

  it.each([
    ["Shift", { shiftKey: true }],
    ["Alt", { altKey: true }],
    ["Ctrl", { ctrlKey: true }],
    ["Meta", { metaKey: true }],
    ["an input method composing", { isComposing: true }],
  ])("leaves Tab alone with %s", (_name, extra) => {
    render(<Box suggestion="Sync" />)
    expect(press({ key: "Tab", ...extra })).toBe(false)
    expect(box().value).toBe("")
  })

  it("ignores keys other than Tab", () => {
    render(<Box suggestion="Sync" />)
    expect(press({ key: "Enter" })).toBe(false)
    expect(press({ key: "ArrowRight" })).toBe(false)
    expect(box().value).toBe("")
  })

  it("leaves a Tab that a capture-phase handler already took, as the dialog's focus trap does", () => {
    render(<Box suggestion="Sync" />)
    const trap = (e: KeyboardEvent) => {
      if (e.key === "Tab") e.preventDefault()
    }
    document.addEventListener("keydown", trap, true)
    try {
      fireEvent.keyDown(box(), { key: "Tab" })
      expect(box().value).toBe("")
    } finally {
      document.removeEventListener("keydown", trap, true)
    }
  })
})
