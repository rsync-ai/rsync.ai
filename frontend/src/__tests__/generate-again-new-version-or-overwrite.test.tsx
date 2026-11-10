import { beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"

// Generating a connector again under a name that exists used to say "overwrite"
// while the generator added a new version, and connections pinned to the old
// version kept running it with nothing on screen to say so. The wizard now asks
// which one the user wants (a new version by default, or the current version
// rewritten in place), sends overwrite only for the second, and lists the pinned
// connections with one button to move them all.

const startDiscovery = vi.fn()
const generateFromSession = vi.fn()
const moveConnectionsToConnectorVersion = vi.fn()
vi.mock("@/lib/api/discovery", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api/discovery")>()),
  startDiscovery: (...a: unknown[]) => startDiscovery(...a),
  generateFromSession: (...a: unknown[]) => generateFromSession(...a),
  moveConnectionsToConnectorVersion: (...a: unknown[]) => moveConnectionsToConnectorVersion(...a),
}))
vi.mock("@/lib/api/mcp-connectors", () => ({
  // the connector is already installed
  fetchMCPConnector: vi.fn().mockResolvedValue({ name: "acme-crm" }),
}))
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn().mockResolvedValue(
    new Response(JSON.stringify({ valid: true, can_generate: true }), { status: 200 }),
  ),
}))
vi.mock("next/navigation", () => ({ useSearchParams: () => new URLSearchParams("") }))
vi.mock("@/components/connectors/discovery/VendorPicker", () => ({
  VendorPicker: ({ apiName, onApiNameChange }: { apiName: string; onApiNameChange: (v: string) => void }) => (
    <input data-testid="api-name" value={apiName} onChange={(e) => onApiNameChange(e.target.value)} />
  ),
}))
vi.mock("@/components/connectors/discovery/ContractPreviewCard", () => ({
  ContractPreviewCard: () => <div data-testid="contract" />,
}))

import { DiscoveryFlow } from "@/components/connectors/discovery/DiscoveryFlow"

const CONTRACT = {
  session_id: "s1",
  api_name: "acme-crm",
  created_at: "",
  updated_at: "",
  facts: {},
  questions: [],
  can_generate: true,
  refusal_reason: null,
  metadata: {},
}

async function discovered() {
  render(<DiscoveryFlow />)
  fireEvent.change(screen.getByTestId("api-name"), { target: { value: "acme-crm" } })
  fireEvent.click(screen.getByRole("button", { name: /Discover/ }))
  await screen.findByTestId("contract")
  // the existence check ticks "Generate again" for an installed connector
  await waitFor(() => expect((screen.getByTestId("force-regenerate-toggle") as HTMLInputElement).checked).toBe(true))
}

const generate = () => fireEvent.click(screen.getByRole("button", { name: /Generate connector/ }))
const lastOptions = () => generateFromSession.mock.calls.at(-1)![2]

beforeEach(() => {
  vi.clearAllMocks()
  startDiscovery.mockResolvedValue({ contract: CONTRACT })
  generateFromSession.mockResolvedValue({ success: true, status: "completed", connector_name: "acme-crm", version: "v1.0.1" })
})

describe("generating an installed connector again", () => {
  it("saves a new version unless the user picks overwrite", async () => {
    await discovered()
    expect((screen.getByTestId("regenerate-new-version") as HTMLInputElement).checked).toBe(true)
    expect(screen.queryByText(/Force regenerate/)).toBeNull()

    generate()
    await waitFor(() => expect(generateFromSession).toHaveBeenCalledTimes(1))
    expect(lastOptions()).toMatchObject({ forceRegenerate: true, overwrite: false })

    fireEvent.click(screen.getByTestId("regenerate-overwrite"))
    generate()
    await waitFor(() => expect(generateFromSession).toHaveBeenCalledTimes(2))
    expect(lastOptions()).toMatchObject({ forceRegenerate: true, overwrite: true })
  })

  it("never sends overwrite once Generate again is unticked", async () => {
    await discovered()
    fireEvent.click(screen.getByTestId("regenerate-overwrite"))
    fireEvent.click(screen.getByTestId("force-regenerate-toggle"))
    expect(screen.queryByTestId("regenerate-overwrite")).toBeNull()
    generate()
    await waitFor(() => expect(generateFromSession).toHaveBeenCalledTimes(1))
    expect(lastOptions()).toMatchObject({ forceRegenerate: false, overwrite: false })
  })

  it("shows an overwrite, a container that did not restart, and moves pinned connections together", async () => {
    generateFromSession.mockResolvedValue({
      success: true,
      status: "completed",
      connector_name: "acme-crm",
      version: "v1.0.0",
      overwritten: true,
      restart_error: "v1.0.0 was overwritten, but its running container could not be stopped.",
      pinned_connections: [
        { id: "c1", name: "Prod CRM", connector_version: "v0.9.0" },
        { id: "c2", name: "", connector_version: "v0.9.1" },
      ],
    })
    moveConnectionsToConnectorVersion.mockResolvedValue({
      success: true,
      connector_version: "v1.0.0",
      message: "2 connection(s) now use v1.0.0 from their pipelines' next manual run or CDC restart.",
    })
    await discovered()
    fireEvent.click(screen.getByTestId("regenerate-overwrite"))
    generate()

    expect((await screen.findByTestId("generation-version")).textContent).toMatch(/v1\.0\.0.*overwritten in place/)
    expect(screen.getByTestId("generation-restart-error").textContent).toContain("could not be stopped")
    const pinned = screen.getByTestId("pinned-connections")
    expect(pinned.textContent).toContain("Prod CRM")
    expect(pinned.textContent).toContain("c2") // no name: the id stands in

    fireEvent.click(screen.getByTestId("pinned-connections-move"))
    expect((await screen.findByTestId("pinned-connections-moved")).textContent).toContain("2 connection(s) now use v1.0.0")
    expect(moveConnectionsToConnectorVersion).toHaveBeenCalledWith("acme-crm", "v1.0.0", ["c1", "c2"])
    expect(screen.queryByTestId("pinned-connections-move")).toBeNull()
  })

  it("keeps the list and shows why when the move is refused", async () => {
    generateFromSession.mockResolvedValue({
      success: true,
      status: "completed",
      connector_name: "acme-crm",
      version: "v1.0.1",
      pinned_connections: [{ id: "c1", name: "Prod CRM", connector_version: "v1.0.0" }],
    })
    moveConnectionsToConnectorVersion.mockRejectedValue(
      new Error("One or more connections were not found in this workspace for this connector. Nothing was changed."),
    )
    await discovered()
    generate()
    fireEvent.click(await screen.findByTestId("pinned-connections-move"))
    expect((await screen.findByTestId("pinned-connections-error")).textContent).toContain("Nothing was changed")
    expect(screen.getByTestId("pinned-connections-move")).toBeTruthy()
  })

  it("shows no prompt when nothing is pinned elsewhere", async () => {
    generateFromSession.mockResolvedValue({
      success: true, status: "completed", connector_name: "acme-crm", version: "v1.0.1", pinned_connections: [],
    })
    await discovered()
    generate()
    expect((await screen.findByTestId("generation-version")).textContent).not.toContain("overwritten")
    expect(screen.queryByTestId("pinned-connections")).toBeNull()
  })
})
