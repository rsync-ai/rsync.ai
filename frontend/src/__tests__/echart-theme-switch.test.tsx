import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { useTheme } from "next-themes"

import { EChart } from "@/components/charts/EChart"
import { ThemeProvider } from "@/components/providers/ThemeProvider"
import { PIPELINE_OUTCOMES, countChart, countRows } from "@/components/runs/run-chart-specs"

// A chart follows a theme switch, end to end and with no mocks: the app's
// ThemeProvider (next-themes), a toggle as the header has one, the tokens as
// globals.css scopes them (`:root` light, `.dark` dark) and ECharts drawing.
//
// next-themes sets `.dark` on <html> in its provider's effect, and React runs a
// child's effects before its parent's. A chart that redraws when next-themes'
// state changes therefore reads the tokens while <html> still has the old class
// and keeps the previous theme's colours until a reload, both ways (found on
// prod after #1406). The redraw has to follow the class itself.

const LIGHT = { succeeded: "142 76% 36%", failed: "0 72% 51%", cancelled: "240 5% 65%", waiting: "38 92% 50%", running: "221 83% 53%" }
const DARK = { succeeded: "142 69% 58%", failed: "0 91% 71%", cancelled: "240 5% 46%", waiting: "43 96% 56%", running: "213 94% 68%" }

const vars = (tokens: Record<string, string>) =>
  Object.entries(tokens)
    .map(([k, v]) => `--chart-${k}: ${v};`)
    .join(" ")

const LIGHT_SUCCEEDED = "hsl(142, 76%, 36%)"
const DARK_SUCCEEDED = "hsl(142, 69%, 58%)"

const SETTINGS = countChart("Pipeline runs per day, by outcome", ["2026-10-03", "2026-10-04"], PIPELINE_OUTCOMES)
const ROWS = countRows(
  [
    { day: "2026-10-03", status: "succeeded", count: 3 },
    { day: "2026-10-04", status: "failed", count: 1 },
  ],
  PIPELINE_OUTCOMES,
)

function ThemeToggle() {
  const { resolvedTheme, setTheme } = useTheme()
  return (
    <button type="button" onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")}>
      Toggle theme
    </button>
  )
}

let sheet: HTMLStyleElement | undefined

beforeEach(() => {
  // jsdom lays nothing out and has no canvas: see echart-draws.test.tsx.
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get: () => 400 })
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null)
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  )
  // jsdom has no matchMedia, and next-themes listens for the system's scheme.
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
})

afterEach(() => {
  delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  sheet?.remove()
  sheet = undefined
  window.localStorage.clear()
  document.documentElement.removeAttribute("class")
  document.documentElement.removeAttribute("style")
})

describe("a chart when the theme is switched", () => {
  it("redraws in the new theme's tokens, light to dark and back", async () => {
    sheet = document.createElement("style")
    sheet.textContent = `:root { ${vars(LIGHT)} } .dark { ${vars(DARK)} }`
    document.head.appendChild(sheet)

    const { container } = render(
      <ThemeProvider>
        <ThemeToggle />
        <EChart settings={SETTINGS} rows={ROWS} />
      </ThemeProvider>,
    )
    const fills = () => [...container.querySelectorAll("svg path")].map((p) => p.getAttribute("fill"))
    const toggle = screen.getByRole("button", { name: "Toggle theme" })

    await waitFor(() => expect(fills()).toContain(LIGHT_SUCCEEDED))
    expect(document.documentElement.classList.contains("dark")).toBe(false)

    await act(async () => fireEvent.click(toggle))
    expect(document.documentElement.classList.contains("dark")).toBe(true)
    await waitFor(() => expect(fills()).toContain(DARK_SUCCEEDED))
    expect(fills()).not.toContain(LIGHT_SUCCEEDED)

    await act(async () => fireEvent.click(toggle))
    expect(document.documentElement.classList.contains("dark")).toBe(false)
    await waitFor(() => expect(fills()).toContain(LIGHT_SUCCEEDED))
    expect(fills()).not.toContain(DARK_SUCCEEDED)
  })
})

// A box that scrolls both ways has a corner where its two bars meet. The bars were styled
// and the corner not, so the browser painted it its own white: a white square on a table
// chart in the dark theme (found on prod after #1540).
describe("the page's scrollbars", () => {
  it("style the corner where two bars meet as the track, so it follows the theme", () => {
    const css = readFileSync(resolve(process.cwd(), "src/app/globals.css"), "utf8")
    const rule = (part: string) => new RegExp(`::-webkit-scrollbar-${part}\\s*\\{([^}]*)\\}`).exec(css)?.[1].trim()
    expect(rule("track")).toBeDefined()
    expect(rule("corner")).toBe(rule("track"))
  })
})
