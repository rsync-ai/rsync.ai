/**
 * The pipeline chat's first screen: quick pipelines come from the user's
 * connections, the first load shows a skeleton instead of an empty page, and
 * existing pipelines are not read here (Pipelines lists them, Runs their runs).
 * It has no box: its examples, connection chips and Custom pipeline go to the
 * chat's box below, and
 * it hands the chat its first example for Tab (chat-input-tab.test.tsx).
 */
import { afterEach, describe, expect, it, vi, type Mock } from "vitest"
import { render, screen, waitFor, fireEvent } from "@testing-library/react"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/chat",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/components/connectors/ConnectionLogo", () => ({
  ConnectionLogo: ({ connectorType }: { connectorType: string }) => <span data-testid="logo">{connectorType}</span>,
}))

import { authFetch } from "@/lib/api/auth-fetch"
import { AgenticPipelineHome } from "@/components/chat/AgenticPipelineHome"
import { buildExamplePrompts } from "@/components/chat/home/suggestions"

const mockFetch = authFetch as unknown as Mock
const res = (status: number, body: unknown) =>
  ({ ok: status >= 200 && status < 300, status, json: async () => body }) as Response

const CONNECTIONS = [
  { id: "c1", name: "Postgresql Source", connector_type: "postgresql", type: "source", status: "active" },
  { id: "c2", name: "MongoDB Source", connector_type: "mongodb", type: "source", status: "active" },
  { id: "c3", name: "MongoDB Dest", connector_type: "mongodb", type: "destination", status: "active" },
  { id: "c4", name: "GCS Dest", connector_type: "gcs", type: "destination", status: "active" },
]

function serve(connections: unknown[]) {
  mockFetch.mockImplementation(async (url: string) =>
    String(url).includes("/connections") ? res(200, { connections }) : res(404, {}),
  )
}

afterEach(() => mockFetch.mockReset())

const props = (over: { onSubmit?: (p: string) => void } = {}) => ({
  onSubmit: vi.fn(),
  onPrefill: vi.fn(),
  onCustom: vi.fn(),
  onSuggestion: vi.fn(),
  ...over,
})

describe("AgenticPipelineHome", () => {
  it("shows a skeleton until the first load lands", async () => {
    let release!: () => void
    mockFetch.mockImplementation(
      () => new Promise<Response>((r) => { release = () => r(res(200, { connections: [] })) }),
    )
    render(<AgenticPipelineHome {...props()} />)
    expect(screen.getByTestId("home-skeleton")).toBeInTheDocument()
    release()
    await waitFor(() => expect(screen.queryByTestId("home-skeleton")).not.toBeInTheDocument())
  })

  it("reads only the connections, not the pipeline list", async () => {
    serve(CONNECTIONS)
    render(<AgenticPipelineHome {...props()} />)

    await screen.findByText("Postgres → GCS")
    const urls = mockFetch.mock.calls.map(([url]) => String(url))
    expect(urls.some((u) => u.includes("/connections"))).toBe(true)
    expect(urls.some((u) => u.includes("/pipelines"))).toBe(false)
  })

  it("builds quick pipelines from the connected source → destination pairs", async () => {
    serve(CONNECTIONS)
    const onSubmit = vi.fn()
    render(<AgenticPipelineHome {...props({ onSubmit })} />)

    fireEvent.click(await screen.findByText("Postgres → GCS"))
    expect(onSubmit).toHaveBeenCalledWith("sync postgresql to gcs")
    expect(screen.getByText("MongoDB → GCS")).toBeInTheDocument()
    // Templates that need connectors the user lacks are not offered as ready.
    expect(screen.queryByText("MySQL → S3")).not.toBeInTheDocument()
  })

  it("marks the generic templates as needing a connection when nothing matches", async () => {
    serve([])
    render(<AgenticPipelineHome {...props()} />)

    await screen.findByText("Example pipelines")
    expect(screen.getByText("MySQL → S3")).toBeInTheDocument()
    expect(screen.getAllByText("Needs connection").length).toBeGreaterThan(0)
    // Launch review: 75% opacity read as disabled; "Needs connection" already says it.
    expect(screen.getByText("MySQL → S3").closest("button")?.className).not.toMatch(/opacity-/)
  })

  it("has no box of its own: the chat's box below is the only one", async () => {
    serve(CONNECTIONS)
    render(<AgenticPipelineHome {...props()} />)
    await screen.findByText("Postgres → GCS")
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument()
  })

  it("an example chip prefills the chat's box without sending it", async () => {
    serve(CONNECTIONS)
    const p = props()
    render(<AgenticPipelineHome {...p} />)

    await screen.findByText("Postgres → GCS")
    const chip = screen.getByRole("button", { name: /stream postgres changes to/i })
    fireEvent.click(chip)
    expect(p.onPrefill).toHaveBeenCalledWith(chip.textContent)
    expect(p.onSubmit).not.toHaveBeenCalled()
  })

  it("hands the chat the first example, for Tab", async () => {
    serve(CONNECTIONS)
    const p = props()
    render(<AgenticPipelineHome {...p} />)
    await screen.findByText("Postgres → GCS")

    const first = screen.getAllByRole("button", { name: /^sync |^stream |^copy /i })[0].textContent
    // The chips commit before the effect that hands one over runs: wait for it.
    await waitFor(() => expect(p.onSuggestion).toHaveBeenLastCalledWith(first))
  })

  it("Custom pipeline sends the user to the chat's box", async () => {
    serve(CONNECTIONS)
    const p = props()
    render(<AgenticPipelineHome {...p} />)

    fireEvent.click(await screen.findByText("Custom pipeline"))
    expect(p.onCustom).toHaveBeenCalledTimes(1)
    expect(p.onSubmit).not.toHaveBeenCalled()
  })
})

// Launch review bug 3: the chips lowercased every name ("sync mongodb to bigquery").
// Bug class: a product name passed through toLowerCase on its way to the screen.
describe("example chips spell product names the way their makers do", () => {
  it("names a ready pair in its own capitals", () => {
    expect(buildExamplePrompts([{ source: "mongodb", destination: "bigquery", label: "", description: "" }])).toEqual([
      "Sync MongoDB to BigQuery",
      "Stream MongoDB changes to BigQuery",
      "Copy MongoDB to BigQuery every night",
    ])
  })

  it("keeps the short names too", () => {
    expect(buildExamplePrompts([{ source: "postgresql", destination: "gcs", label: "", description: "" }])[0]).toBe(
      "Sync Postgres to GCS",
    )
  })

  it("falls back to generic examples that are capitalised the same way", () => {
    expect(buildExamplePrompts([])).toEqual([
      "Sync MySQL to S3",
      "Back up Postgres daily",
      "Stream MongoDB changes to BigQuery",
    ])
  })
})

// Launch review (Chat): the page offered generic templates but never the user's
// own connections, and an expired one only showed up on Connections.
describe("your connections strip", () => {
  it("lists each connection, and a chip starts a prompt from it", async () => {
    serve(CONNECTIONS)
    const p = props()
    render(<AgenticPipelineHome {...p} />)

    const strip = await screen.findByRole("region", { name: "Your connections" })
    expect(strip).toHaveTextContent("Pick one to start from it")
    fireEvent.click(screen.getByRole("button", { name: /Postgresql Source/ }))
    expect(p.onPrefill).toHaveBeenCalledWith("Sync Postgresql Source (Postgres) to ")
    fireEvent.click(screen.getByRole("button", { name: /GCS Dest/ }))
    expect(p.onPrefill).toHaveBeenLastCalledWith("Load into GCS Dest (GCS) from ")
    expect(p.onSubmit).not.toHaveBeenCalled()
  })

  it("an expired connection links to its page to reconnect instead", async () => {
    serve([...CONNECTIONS, { id: "c5", name: "Sheets Source", connector_type: "google-sheets", type: "source", status: "active", is_expired: true }])
    const p = props()
    render(<AgenticPipelineHome {...p} />)

    const link = await screen.findByRole("link", { name: /Sheets Source.*Reconnect/ })
    expect(link).toHaveAttribute("href", "/connections/c5?type=source")
    expect(screen.queryByRole("button", { name: /Sheets Source/ })).not.toBeInTheDocument()
  })

  it("a token that expired before a passing test is not offered for reconnect", async () => {
    serve([{ ...CONNECTIONS[0], is_expired: true, is_connected: true, token_expires_at: "2026-10-01T00:00:00Z", last_tested_at: "2026-10-02T00:00:00Z" }])
    render(<AgenticPipelineHome {...props()} />)
    await screen.findByRole("button", { name: /Postgresql Source/ })
    expect(screen.queryByText("Reconnect")).not.toBeInTheDocument()
  })

  it("shows eight chips and links the rest", async () => {
    const many = Array.from({ length: 11 }, (_, i) => ({ id: `m${i}`, name: `Conn ${i}`, connector_type: "postgresql", type: "source", status: "active" }))
    serve(many)
    render(<AgenticPipelineHome {...props()} />)
    const strip = await screen.findByRole("region", { name: "Your connections" })
    expect(strip.querySelectorAll("li button")).toHaveLength(8)
    expect(screen.getByRole("link", { name: "+3 more" })).toHaveAttribute("href", "/connections")
  })

  // Sources went first and the cut came after, so eight sources hid every
  // destination. Each kind keeps half the chips; a kind with fewer gives the
  // rest to the other.
  it("keeps destinations in view when there are many sources", async () => {
    const conn = (i: number, type: string) => ({ id: `${type}${i}`, name: `${type} ${i}`, connector_type: "postgresql", type, status: "active" })
    serve([...Array.from({ length: 10 }, (_, i) => conn(i, "source")), ...Array.from({ length: 2 }, (_, i) => conn(i, "destination"))])
    const { unmount } = render(<AgenticPipelineHome {...props()} />)
    let strip = await screen.findByRole("region", { name: "Your connections" })
    let chips = Array.from(strip.querySelectorAll("li button span.truncate")).map((b) => b.textContent)
    expect(chips).toHaveLength(8)
    expect(chips.filter((t) => t?.startsWith("destination"))).toEqual(["destination 0", "destination 1"])
    expect(screen.getByRole("link", { name: "+4 more" })).toBeInTheDocument()
    unmount()

    serve([...Array.from({ length: 10 }, (_, i) => conn(i, "source")), ...Array.from({ length: 10 }, (_, i) => conn(i, "destination"))])
    render(<AgenticPipelineHome {...props()} />)
    strip = await screen.findByRole("region", { name: "Your connections" })
    chips = Array.from(strip.querySelectorAll("li button span.truncate")).map((b) => b.textContent)
    expect(chips.filter((t) => t?.startsWith("source"))).toHaveLength(4)
    expect(chips.filter((t) => t?.startsWith("destination"))).toHaveLength(4)
    expect(screen.getByRole("link", { name: "+12 more" })).toBeInTheDocument()
  })

  it("is not shown when there are no connections", async () => {
    serve([])
    render(<AgenticPipelineHome {...props()} />)
    await screen.findByText("Example pipelines")
    expect(screen.queryByRole("region", { name: "Your connections" })).not.toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Create connection" })).toHaveAttribute("href", "/connections/new")
  })
})

// Launch review (Chat): "Try:" was gray-400 on white, about 2.5:1.
it("the Try: label uses the readable muted colour", async () => {
  serve(CONNECTIONS)
  render(<AgenticPipelineHome {...props()} />)
  await screen.findByText("Postgres → GCS")
  // Light-mode class only: "dark:text-gray-400" is right on a dark background.
  expect(screen.getByText("Try:").className).toMatch(/(^|\s)text-gray-500\b/)
  expect(screen.getByText("Try:").className).not.toMatch(/(^|\s)text-gray-400\b/)
})
