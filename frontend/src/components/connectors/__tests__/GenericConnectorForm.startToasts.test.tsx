import { describe, it, expect, vi, afterEach, beforeEach } from "vitest"
import fs from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { act, render, screen, fireEvent, cleanup } from "@testing-library/react"
import "@testing-library/jest-dom"
import { toast } from "sonner"
import type { MCPConnector } from "@/lib/types/mcp-connector"
import { testMCPConnection } from "@/lib/api/mcp-connectors"
import { GenericConnectorForm } from "../GenericConnectorForm"

// ---------------------------------------------------------------------------
// Prod 2026-10-01, connector form at ~470 px: "Start & Test Connection" on a
// stopped connector raised "Starting connector..." (never dismissed) and then
// "Connector started successfully!" (the same news the inline result already
// says). Stacked at the bottom of a narrow window they sat on Save Connection.
// Bug class: a toast that repeats inline state, or outlives the step it reports.
// ---------------------------------------------------------------------------

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}) })),
}))
vi.mock("@/components/oauth/OAuthConnectButton", () => ({
  OAuthConnectButton: () => <button type="button">Connect</button>,
}))
vi.mock("sonner", () => ({
  toast: { info: vi.fn(() => "start-toast-id"), success: vi.fn(), error: vi.fn(), dismiss: vi.fn() },
}))
vi.mock("@/lib/api/mcp-connectors", () => ({ testMCPConnection: vi.fn() }))

const CONNECTORS_ROOT = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../../../shared/mcp-connectors/public",
)

function loadShippedConnector(rel: string, dockerStatus: string): MCPConnector {
  const dir = path.join(CONNECTORS_ROOT, rel)
  const cv = JSON.parse(fs.readFileSync(path.join(dir, "latest.json"), "utf8")).current_version
  const m = JSON.parse(fs.readFileSync(path.join(dir, "versions", cv, "metadata.json"), "utf8"))
  return {
    ...m,
    name: m.id,
    configuration_schema: m.config_schema,
    docker_status: dockerStatus,
  } as unknown as MCPConnector
}

async function startAndTest(dockerStatus: string) {
  render(
    <GenericConnectorForm
      connector={loadShippedConnector("postgresql", dockerStatus)}
      onSave={vi.fn()}
      onCancel={vi.fn()}
      initialData={{ connectionName: "pg" }}
    />,
  )
  fireEvent.change(screen.getByTestId("credential-user"), { target: { value: "rsync" } })
  fireEvent.change(screen.getByTestId("credential-password"), { target: { value: "x" } })
  fireEvent.click(screen.getByRole("button", { name: /Test Connection/i }))
  // The form waits 1.5 s for a stopped container before testing.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2000)
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe("GenericConnectorForm — Start & Test toasts", () => {
  it("dismisses its 'Starting connector...' toast and raises no success toast", async () => {
    vi.mocked(testMCPConnection).mockResolvedValue({ success: true, message: "Connected" })
    await startAndTest("stopped")

    expect(toast.info).toHaveBeenCalledWith("Starting connector...", expect.anything())
    expect(toast.dismiss).toHaveBeenCalledWith("start-toast-id")
    expect(toast.success).not.toHaveBeenCalled()
    // The inline result is where success is said.
    expect(screen.getByText("Connected")).toBeInTheDocument()
  })

  it("a failed test dismisses the start toast too", async () => {
    vi.mocked(testMCPConnection).mockRejectedValue(new Error("boom"))
    await startAndTest("not_deployed")

    expect(toast.dismiss).toHaveBeenCalledWith("start-toast-id")
  })

  it("control: a running connector raises no start toast at all", async () => {
    vi.mocked(testMCPConnection).mockResolvedValue({ success: true, message: "Connected" })
    await startAndTest("running")

    expect(toast.info).not.toHaveBeenCalled()
    expect(toast.success).not.toHaveBeenCalled()
  })
})
