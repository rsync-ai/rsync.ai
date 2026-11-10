/**
 * A CLICK OUTSIDE A POPUP DID NOTHING; ONLY ESCAPE CLOSED IT.
 *
 * `components/ui/dialog.tsx` drew a dim backdrop with `onClick={close}` and, after
 * it, a full-screen layer that centres the panel — same `fixed inset-0 z-50`, so
 * the later layer paints on top and takes every click on the dim area. The
 * backdrop's handler was unreachable from the day the file was written, in every
 * dialog of the app.
 *
 * jsdom does no hit-testing, so clicking the backdrop element here would pass
 * while a browser never delivers that click. The test clicks the layer a browser
 * would hit: of the overlay's full-screen layers, the highest `z`, and on a tie
 * the last in the document.
 */

import { readdirSync, readFileSync, statSync } from "node:fs"
import { join, relative, resolve } from "node:path"
import { describe, it, expect, vi } from "vitest"
import { fireEvent, render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"

function zOf(el: Element): number {
  const m = el.className.toString().match(/(?:^|\s)z-(?:\[(\d+)\]|(\d+))(?:\s|$)/)
  return m ? Number(m[1] ?? m[2]) : 0
}

/** The full-screen layer on top of the overlay that holds `panel`: what a click on the dim area hits. */
function areaAround(panel: HTMLElement): HTMLElement {
  const overlay = panel.parentElement?.parentElement
  if (!overlay) throw new Error("no overlay around the panel")
  const layers = Array.from(overlay.children).filter(
    (el): el is HTMLElement => el instanceof HTMLElement && el.classList.contains("fixed") && el.classList.contains("inset-0"),
  )
  // Stable sort: on equal z the later layer stays later, and paints on top.
  return layers.sort((a, b) => zOf(a) - zOf(b))[layers.length - 1]
}

function openOne(props: { closeOnOutsideClick?: boolean } = {}) {
  const onOpenChange = vi.fn()
  render(
    <Dialog open onOpenChange={onOpenChange} {...props}>
      <DialogContent>
        <DialogTitle>Loan portfolio</DialogTitle>
        <p>Runs</p>
      </DialogContent>
    </Dialog>,
  )
  return { onOpenChange, panel: screen.getByRole("dialog") }
}

describe("a click outside a dialog closes it", () => {
  it("closes on a click on the dim area around the panel", async () => {
    const { onOpenChange, panel } = openOne()

    await userEvent.click(areaAround(panel))

    expect(onOpenChange).toHaveBeenCalledTimes(1)
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it("stays open on a click inside the panel", async () => {
    const { onOpenChange } = openOne()

    await userEvent.click(screen.getByText("Runs"))

    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it("stays open when a text selection is dragged out of the panel", () => {
    const { onOpenChange, panel } = openOne()

    fireEvent.mouseDown(screen.getByText("Runs"))
    fireEvent.mouseUp(areaAround(panel))
    // The browser fires the click on the nearest common ancestor: the area.
    fireEvent.click(areaAround(panel))

    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it("stays open on an outside click when the dialog opts out, and Escape still closes it", async () => {
    const { onOpenChange, panel } = openOne({ closeOnOutsideClick: false })

    await userEvent.click(areaAround(panel))
    expect(onOpenChange).not.toHaveBeenCalled()

    await userEvent.keyboard("{Escape}")
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it("closes only the dialog on top when one is open over another", async () => {
    const outer = vi.fn()
    const inner = vi.fn()
    render(
      <Dialog open onOpenChange={outer}>
        <DialogContent>
          <DialogTitle>Model</DialogTitle>
          <Dialog open onOpenChange={inner}>
            <DialogContent>
              <DialogTitle>Run</DialogTitle>
            </DialogContent>
          </Dialog>
        </DialogContent>
      </Dialog>,
    )
    const top = screen.getAllByRole("dialog")[1]

    // The inner overlay sits inside the outer panel, so the click bubbles
    // through the outer one too.
    await userEvent.click(areaAround(top))

    expect(inner).toHaveBeenCalledWith(false)
    expect(outer).not.toHaveBeenCalled()
  })
})

/**
 * Where the user types or picks something, a stray click must not throw it away:
 * those dialogs pass `closeOnOutsideClick={false}` and keep Escape and X. This
 * reads every `<Dialog>` in the app, so a new form dialog is held to it too.
 * Controls rendered by a child component the scan cannot see (a schedule editor)
 * are opted out by hand at the call site.
 */
const FORM_CONTROL =
  /<(input|textarea|select|Input|Textarea|SelectTrigger|Checkbox|Switch|RadioGroup|[A-Z]\w*Form)\b/g

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return name === "__tests__" ? [] : sources(path)
    return /\.tsx$/.test(name) && !/\.test\.tsx$/.test(name) ? [path] : []
  })
}

/** The `<Dialog …>` opening tag at `start`: up to the first `>` outside `{}`. */
function openingTag(text: string, start: number): string {
  let depth = 0
  for (let i = start; i < text.length; i++) {
    if (text[i] === "{") depth++
    else if (text[i] === "}") depth--
    else if (text[i] === ">" && depth === 0) return text.slice(start, i + 1)
  }
  return text.slice(start)
}

/** A control the user can type into or pick from; a read-only one (a cell viewer) holds nothing to lose. */
function holdsInput(body: string): boolean {
  return [...body.matchAll(FORM_CONTROL)].some((m) => !/\breadOnly\b/.test(openingTag(body, m.index ?? 0)))
}

describe("form dialogs keep what was typed", () => {
  it("every dialog holding an input opts out of closing on an outside click", () => {
    const src = resolve(__dirname, "..")
    const files = sources(src)
    let scanned = 0
    const open: string[] = []
    for (const path of files) {
      const text = readFileSync(path, "utf8")
      // At the start of a line, so a comment that names `<Dialog>` is not a dialog.
      for (const m of text.matchAll(/^[ \t]*<Dialog(?=[\s>])/gm)) {
        const start = (m.index ?? 0) + m[0].indexOf("<")
        const end = text.indexOf("</Dialog>", start)
        const body = text.slice(start, end === -1 ? undefined : end)
        if (!holdsInput(body)) continue
        scanned++
        if (!/closeOnOutsideClick=\{false\}/.test(openingTag(text, start))) {
          const line = text.slice(0, start).split("\n").length
          open.push(`${relative(src, path)}:${line}`)
        }
      }
    }
    // A scan that matches nothing proves nothing.
    expect(scanned).toBeGreaterThan(10)
    expect(open).toEqual([])
  })
})
