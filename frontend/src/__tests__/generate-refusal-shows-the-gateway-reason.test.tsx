import { beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"

// Found 2026-10-06 on app.rsync.ai: overwriting github-rest from the generate
// page was refused by the gateway with "This is a built-in, maintained connector
// and cannot be generated or overwritten." (a 403), and the page said
// "Connector generation requires power user or admin access" -- every 403 was
// shown as a role problem, so a workspace owner was told to ask an admin.
// The page now shows the gateway's own sentence; only the role gate's 403 gets
// the access message, naming the gate that runs (workspace owner or admin).

const startDiscovery = vi.fn()
const generateFromSession = vi.fn()
vi.mock("@/lib/api/discovery", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api/discovery")>()),
  startDiscovery: (...a: unknown[]) => startDiscovery(...a),
  generateFromSession: (...a: unknown[]) => generateFromSession(...a),
}))
vi.mock("@/lib/api/mcp-connectors", () => ({
  fetchMCPConnector: vi.fn().mockResolvedValue({ name: "github-rest" }),
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

import { DiscoveryError } from "@/lib/api/discovery"
import { DiscoveryFlow } from "@/components/connectors/discovery/DiscoveryFlow"

const CONTRACT = {
  session_id: "s1", api_name: "github-rest", created_at: "", updated_at: "", facts: {},
  questions: [], can_generate: true, refusal_reason: null, metadata: {},
}

async function generateRefusedWith(message: string) {
  generateFromSession.mockRejectedValue(new DiscoveryError(message, 403))
  render(<DiscoveryFlow />)
  fireEvent.change(screen.getByTestId("api-name"), { target: { value: "github-rest" } })
  fireEvent.click(screen.getByRole("button", { name: /Discover/ }))
  await screen.findByTestId("contract")
  fireEvent.click(screen.getByRole("button", { name: /Generate connector/ }))
  await waitFor(() => expect(generateFromSession).toHaveBeenCalledTimes(1))
}

beforeEach(() => {
  vi.clearAllMocks()
  startDiscovery.mockResolvedValue({ contract: CONTRACT })
})

describe("a refused generate says why", () => {
  it("shows the protected-connector refusal, not an access problem", async () => {
    await generateRefusedWith("This is a built-in, maintained connector and cannot be generated or overwritten.")
    expect(await screen.findAllByText(/built-in, maintained connector and cannot be generated or overwritten/)).not.toHaveLength(0)
    expect(screen.queryByText(/access/i)).toBeNull()
  })

  it("names the workspace role gate when that is what refused", async () => {
    await generateRefusedWith("insufficient workspace role")
    expect(await screen.findAllByText(/workspace owner or admin/)).not.toHaveLength(0)
    expect(screen.queryByText(/power user/i)).toBeNull()
  })
})
