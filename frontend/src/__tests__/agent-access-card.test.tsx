import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { AgentAccessCard } from "@/components/settings/AgentAccessCard"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { readFileSync } from "node:fs"
import { resolve } from "node:path"

import { AGENT_SCOPE_CHOICES, agentAppSnippets } from "@/lib/api/agent-tokens"
import { featureFlagsManager, fetchFeatureFlags } from "@/config/features"

// Settings → AI apps: the app picker and Agent tokens. The server (handlers/agent_tokens.go) owns every
// rule — who sees which token, the 1-365 day bound, the 20-token cap — so what
// is pinned here is what only the card can get wrong:
//   - the token is shown ONCE, with setup commands that carry the real MCP URL,
//     and is gone after Done (nothing can fetch it again);
//   - the expiry the person picked is the one sent;
//   - revoke asks first, sends DELETE for that token, and refreshes the list;
//   - a failed list read shows Retry, never "no tokens";
//   - the card appears only when /api/v1/features says agent_gateway.
//   - the AI app picker above the tokens never holds a token: VS Code asks for
//     it (a password input), Gemini CLI reads it from an environment variable.
//   - only an admin or owner is offered "Can generate connectors", and the
//     scope is sent only when it is ticked.
//   - a member is offered "Can propose pipelines" (a plan creates nothing);
//     a viewer, whose token the server refuses on every plan, is not.
//   - every scope the server lets a token carry (services.TokenScopes) is a
//     box, offered from the role the server lets grant it (services.AdminScope):
//     a scope left off the card can only be granted through the API.

// null = no workspace role known (outside a provider, the hook fails closed).
let mockRole: string | null = null
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: mockRole,
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => false,
    meets: (min: string) => (mockRole ? realMeetsRole(mockRole as never, min as never) : false),
  }),
}))
import { meetsRole as realMeetsRole } from "@/lib/workspace/roles"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const TOKEN = "rsat_" + "ab".repeat(32)
const future = new Date(Date.now() + 30 * 86_400_000).toISOString()
const past = new Date(Date.now() - 86_400_000).toISOString()

const mine = {
  id: "t-1",
  name: "Claude Code on my laptop",
  token_prefix: "rsat_abababa",
  scopes: ["metadata:read"],
  expires_at: future,
  last_used_at: null,
  created_at: past,
  owner_email: "me@example.com",
  mine: true,
}
const theirs = { ...mine, id: "t-2", name: "CI agent", owner_email: "ana@example.com", mine: false }
const revoked = { ...mine, id: "t-3", name: "old laptop", revoked_at: past }
const expired = { ...mine, id: "t-4", name: "trial", expires_at: past }

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

// Routes authFetch by method; `tokens` is read at call time so a test can
// change what the next list returns.
function route(opts: { tokens?: unknown[]; list?: Response; create?: Response; revoke?: Response } = {}) {
  let tokens = opts.tokens ?? []
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (method === "POST") return Promise.resolve(opts.create ?? json(201, {}))
    if (method === "DELETE") {
      tokens = tokens.filter((t) => !url.endsWith(`/${(t as { id: string }).id}`))
      return Promise.resolve(opts.revoke ?? json(200, { revoked: true }))
    }
    return Promise.resolve(opts.list ?? json(200, { tokens }))
  })
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(async () => {
  mockRole = null
  mockFetch.mockReset()
  const { toast } = await import("sonner")
  ;(toast.error as Mock).mockReset()
  ;(toast.success as Mock).mockReset()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe("AgentAccessCard — list", () => {
  it("lists tokens: another member's shows its owner, a dead one has no Revoke", async () => {
    route({ tokens: [mine, theirs, revoked, expired] })
    render(<AgentAccessCard />)

    const list = await screen.findByRole("list", { name: /agent tokens/i })
    const rows = within(list).getAllByRole("listitem")
    expect(rows).toHaveLength(4)

    expect(within(rows[0]).getByText("rsat_abababa…")).toBeInTheDocument()
    expect(within(rows[0]).queryByText("me@example.com", { exact: false })).toBeNull()
    expect(rows[0]).toHaveTextContent(/Last used never/)
    expect(within(rows[1]).getByText("ana@example.com", { exact: false })).toBeInTheDocument()

    expect(within(rows[2]).getByText("Revoked")).toBeInTheDocument()
    expect(within(rows[3]).getByText("Expired")).toBeInTheDocument()
    expect(within(rows[0]).getByRole("button", { name: /revoke/i })).toBeInTheDocument()
    expect(within(rows[2]).queryByRole("button", { name: /revoke/i })).toBeNull()
    expect(within(rows[3]).queryByRole("button", { name: /revoke/i })).toBeNull()
  })

  it("a failed read shows Retry, not an empty list, and Retry reads again", async () => {
    route({ list: json(500, { error: "Could not list agent tokens" }) })
    render(<AgentAccessCard />)
    await screen.findByText(/could not load agent tokens/i)
    expect(screen.queryByText(/no agent tokens/i)).toBeNull()

    route({ tokens: [mine] })
    fireEvent.click(screen.getByRole("button", { name: /retry/i }))
    await screen.findByText("Claude Code on my laptop")
  })

  it("a 200 that is not the list fails like an error", async () => {
    route({ list: json(200, { hello: "proxy page" }) })
    render(<AgentAccessCard />)
    await screen.findByText(/could not load agent tokens/i)
  })
})

describe("AgentAccessCard — create", () => {
  it("sends the chosen expiry, shows the token once with setup commands, and forgets it on Done", async () => {
    route({
      create: json(201, {
        id: "t-9",
        name: "cursor",
        token: TOKEN,
        token_prefix: TOKEN.slice(0, 12),
        scopes: ["metadata:read"],
        expires_at: future,
        workspace_id: "ws-1",
      }),
    })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)

    const create = screen.getByRole("button", { name: /create token/i })
    expect(create).toBeDisabled()
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "  cursor  " } })
    fireEvent.click(screen.getByRole("button", { name: "1 year" }))
    expect(screen.getByRole("button", { name: "1 year" })).toHaveAttribute("aria-pressed", "true")
    fireEvent.click(create)

    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    expect(post[0]).toBe(API_ENDPOINTS.AGENT_TOKENS.LIST)
    expect(JSON.parse((post[1] as RequestInit).body as string)).toEqual({ name: "cursor", expires_in_days: 365 })

    const url = API_ENDPOINTS.AGENT_TOKENS.MCP
    expect(url).toMatch(/\/api\/agent\/v1\/mcp$/)
    expect(screen.getByText(TOKEN)).toBeInTheDocument()
    expect(
      screen.getByText(`claude mcp add --transport http rsync-ai ${url} --header "Authorization: Bearer ${TOKEN}"`),
    ).toBeInTheDocument()
    // The Cursor file carries the token; the VS Code tab of the picker has the same url line but no token.
    expect(
      screen.getByText(new RegExp(`"url": "${url.replace(/[/.]/g, "\\$&")}"[\\s\\S]*Bearer ${TOKEN}`)),
    ).toBeInTheDocument()

    fireEvent.click(screen.getByRole("button", { name: /done/i }))
    expect(screen.queryByText(TOKEN, { exact: false })).toBeNull()
    expect(screen.getByLabelText(/token name/i)).toHaveValue("")
  })

  it("surfaces the server's refusal (the 20-token cap) and shows no token", async () => {
    const { toast } = await import("sonner")
    route({ create: json(409, { error: "You have 20 live agent tokens in this workspace. Revoke one first." }) })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "one more" } })
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))
    await waitFor(() =>
      expect(toast.error as Mock).toHaveBeenCalledWith(
        "You have 20 live agent tokens in this workspace. Revoke one first.",
      ),
    )
    expect(screen.queryByText(/it will not be shown again/i)).toBeNull()
  })
})

describe("AgentAccessCard — connectors:generate", () => {
  const created = json(201, {
    id: "t-9",
    name: "builder",
    token: TOKEN,
    token_prefix: TOKEN.slice(0, 12),
    scopes: ["metadata:read", "connectors:generate"],
    expires_at: future,
    workspace_id: "ws-1",
  })

  it("an admin may tick it, and only then is the scope sent", async () => {
    mockRole = "admin"
    route({ create: created })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)

    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "builder" } })
    fireEvent.click(screen.getByRole("checkbox", { name: /can generate connectors/i }))
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))

    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    expect(JSON.parse((post[1] as RequestInit).body as string)).toEqual({
      name: "builder",
      expires_in_days: 30,
      scopes: ["connectors:generate"],
    })
    fireEvent.click(screen.getByRole("button", { name: /done/i }))
    expect(screen.getByRole("checkbox", { name: /can generate connectors/i })).not.toBeChecked()
  })

  it("an admin who leaves it unticked sends no scopes", async () => {
    mockRole = "owner"
    route({ create: created })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "reader" } })
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))
    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    expect(JSON.parse((post[1] as RequestInit).body as string)).not.toHaveProperty("scopes")
  })

  it("a member is not offered it", async () => {
    mockRole = "member"
    route()
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    expect(screen.queryByRole("checkbox", { name: /can generate connectors/i })).toBeNull()
  })

  it("a token that has it says so in the list", async () => {
    route({ tokens: [{ ...mine, scopes: ["metadata:read", "connectors:generate"] }, theirs] })
    render(<AgentAccessCard />)
    const list = await screen.findByRole("list", { name: /agent tokens/i })
    const rows = within(list).getAllByRole("listitem")
    expect(within(rows[0]).getByText("Generates connectors")).toBeInTheDocument()
    expect(within(rows[1]).queryByText("Generates connectors")).toBeNull()
  })
})

describe("AgentAccessCard — models:write", () => {
  const created = json(201, {
    id: "t-10",
    name: "modeller",
    token: TOKEN,
    token_prefix: TOKEN.slice(0, 12),
    scopes: ["metadata:read", "models:write"],
    expires_at: future,
    workspace_id: "ws-1",
  })

  async function createWith(boxes: RegExp[]) {
    route({ create: created })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "modeller" } })
    for (const name of boxes) fireEvent.click(screen.getByRole("checkbox", { name }))
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))
    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    return JSON.parse((post[1] as RequestInit).body as string)
  }

  it("an admin may tick it, and only then is the scope sent", async () => {
    mockRole = "admin"
    expect(await createWith([/can build models/i])).toEqual({
      name: "modeller",
      expires_in_days: 30,
      scopes: ["models:write"],
    })
    fireEvent.click(screen.getByRole("button", { name: /done/i }))
    expect(screen.getByRole("checkbox", { name: /can build models/i })).not.toBeChecked()
  })

  it("both boxes send both scopes", async () => {
    mockRole = "owner"
    expect((await createWith([/can generate connectors/i, /can build models/i])).scopes).toEqual([
      "connectors:generate",
      "models:write",
    ])
  })

  it("a member is not offered it", async () => {
    mockRole = "member"
    route()
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    expect(screen.queryByRole("checkbox", { name: /can build models/i })).toBeNull()
  })

  it("a token that has it says so in the list", async () => {
    route({ tokens: [{ ...mine, scopes: ["metadata:read", "models:write"] }, theirs] })
    render(<AgentAccessCard />)
    const list = await screen.findByRole("list", { name: /agent tokens/i })
    const rows = within(list).getAllByRole("listitem")
    expect(within(rows[0]).getByText("Builds models")).toBeInTheDocument()
    expect(within(rows[0]).queryByText("Generates connectors")).toBeNull()
    expect(within(rows[1]).queryByText("Builds models")).toBeNull()
  })
})

describe("AgentAccessCard — pipelines:plan", () => {
  const created = json(201, {
    id: "t-11",
    name: "planner",
    token: TOKEN,
    token_prefix: TOKEN.slice(0, 12),
    scopes: ["metadata:read", "pipelines:plan"],
    expires_at: future,
    workspace_id: "ws-1",
  })

  async function createWith(boxes: RegExp[]) {
    route({ create: created })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "planner" } })
    for (const name of boxes) fireEvent.click(screen.getByRole("checkbox", { name }))
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))
    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    return JSON.parse((post[1] as RequestInit).body as string)
  }

  // Unlike the admin scopes, a member may grant it: a plan creates nothing.
  it("a member may tick it, and only then is the scope sent", async () => {
    mockRole = "member"
    expect(await createWith([/can propose pipelines/i])).toEqual({
      name: "planner",
      expires_in_days: 30,
      scopes: ["pipelines:plan"],
    })
    fireEvent.click(screen.getByRole("button", { name: /done/i }))
    expect(screen.getByRole("checkbox", { name: /can propose pipelines/i })).not.toBeChecked()
  })

  it("a member who leaves it unticked sends no scopes", async () => {
    mockRole = "member"
    expect(await createWith([])).not.toHaveProperty("scopes")
  })

  it("an owner's ticks are all sent", async () => {
    mockRole = "owner"
    expect(
      (await createWith([/can propose pipelines/i, /can generate connectors/i, /can build models/i])).scopes,
    ).toEqual(["pipelines:plan", "connectors:generate", "models:write"])
  })

  it("a viewer is not offered it", async () => {
    mockRole = "viewer"
    route()
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    expect(screen.queryByRole("checkbox", { name: /can propose pipelines/i })).toBeNull()
  })

  it("a token that has it says so in the list", async () => {
    route({ tokens: [{ ...mine, scopes: ["metadata:read", "pipelines:plan"] }, theirs] })
    render(<AgentAccessCard />)
    const list = await screen.findByRole("list", { name: /agent tokens/i })
    const rows = within(list).getAllByRole("listitem")
    expect(within(rows[0]).getByText("Proposes pipelines")).toBeInTheDocument()
    expect(within(rows[1]).queryByText("Proposes pipelines")).toBeNull()
  })
})

// workflows:write and dashboards:write: the server has let an owner or admin
// grant them since #1374 and the dashboards work, but the card offered no box,
// so the 403 that names them ("An owner or admin can create a token that has
// it") pointed at a token nobody could make here.
describe("AgentAccessCard — workflows:write and dashboards:write", () => {
  const created = json(201, {
    id: "t-12",
    name: "drafter",
    token: TOKEN,
    token_prefix: TOKEN.slice(0, 12),
    scopes: ["metadata:read", "workflows:write", "dashboards:write"],
    expires_at: future,
    workspace_id: "ws-1",
  })

  async function createWith(boxes: RegExp[]) {
    route({ create: created })
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    fireEvent.change(screen.getByLabelText(/token name/i), { target: { value: "drafter" } })
    for (const name of boxes) fireEvent.click(screen.getByRole("checkbox", { name }))
    fireEvent.click(screen.getByRole("button", { name: /create token/i }))
    await screen.findByText(/it will not be shown again/i)
    const post = mockFetch.mock.calls.find(([, i]) => i?.method === "POST")!
    return JSON.parse((post[1] as RequestInit).body as string)
  }

  it("an admin may tick each, and only the ticked one is sent", async () => {
    mockRole = "admin"
    expect((await createWith([/can draft workflows/i])).scopes).toEqual(["workflows:write"])
  })

  it("an admin's two ticks send both, after the scopes above them", async () => {
    mockRole = "admin"
    expect((await createWith([/can draft dashboards/i, /can draft workflows/i, /can build models/i])).scopes).toEqual(
      ["models:write", "workflows:write", "dashboards:write"],
    )
    fireEvent.click(screen.getByRole("button", { name: /done/i }))
    expect(screen.getByRole("checkbox", { name: /can draft workflows/i })).not.toBeChecked()
    expect(screen.getByRole("checkbox", { name: /can draft dashboards/i })).not.toBeChecked()
  })

  it("a member is offered neither", async () => {
    mockRole = "member"
    route()
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    // Control: the member's own box is there, so the card has rendered its boxes.
    expect(screen.getByRole("checkbox", { name: /can propose pipelines/i })).toBeInTheDocument()
    expect(screen.queryByRole("checkbox", { name: /can draft workflows/i })).toBeNull()
    expect(screen.queryByRole("checkbox", { name: /can draft dashboards/i })).toBeNull()
  })

  it("a token that has them says so in the list", async () => {
    route({ tokens: [{ ...mine, scopes: ["metadata:read", "workflows:write", "dashboards:write"] }, theirs] })
    render(<AgentAccessCard />)
    const list = await screen.findByRole("list", { name: /agent tokens/i })
    const rows = within(list).getAllByRole("listitem")
    expect(within(rows[0]).getByText("Drafts workflows")).toBeInTheDocument()
    expect(within(rows[0]).getByText("Drafts dashboards")).toBeInTheDocument()
    expect(within(rows[1]).queryByText("Drafts workflows")).toBeNull()
  })

  // The intro said a token "reads metadata — never your data", which a token
  // with "Can build models" outgrows: a model's SQL builds tables from the data.
  it("the intro does not promise more than the boxes keep", async () => {
    route()
    render(<AgentAccessCard />)
    await screen.findByText(/no agent tokens/i)
    expect(screen.queryByText(/never your data/i)).toBeNull()
    expect(screen.getByText(/build tables from your data/i)).toBeInTheDocument()
  })
})

// Bug class: a scope the server accepts that the card cannot grant. The card's
// list is checked against the Go source, so a new services.Scope* that joins
// TokenScopes fails here until it has a box, and its admin-ness must match
// services.AdminScope.
describe("every server scope is a box", () => {
  const go = readFileSync(resolve(process.cwd(), "../api-gateway/internal/services/pipeline_policy.go"), "utf8")
  const consts = new Map([...go.matchAll(/^\s*(Scope\w+)\s*=\s*"([^"]+)"/gm)].map((m) => [m[1], m[2]]))
  const value = (ident: string) => {
    const v = consts.get(ident)
    if (!v) throw new Error(`${ident} is not a Scope constant in pipeline_policy.go`)
    return v
  }
  const tokenScopes = (go.match(/var TokenScopes = \[\]string\{([^}]*)\}/)?.[1] ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .map(value)
  const adminBody = go.match(/func AdminScope\(scope string\) bool \{([\s\S]*?)\n\}/)?.[1] ?? ""
  const adminScopes = [...adminBody.matchAll(/scope == (Scope\w+)/g)].map((m) => value(m[1]))

  it("parses the Go source (census)", () => {
    expect(consts.size).toBeGreaterThanOrEqual(6)
    expect(tokenScopes.length).toBeGreaterThanOrEqual(6)
    expect(tokenScopes).toContain("metadata:read")
    expect(adminScopes.length).toBeGreaterThanOrEqual(4)
  })

  it("offers every scope a token may carry beyond metadata:read, once", () => {
    const offered = AGENT_SCOPE_CHOICES.map((c) => c.scope)
    expect(new Set(offered).size).toBe(offered.length)
    expect([...offered].sort()).toEqual(tokenScopes.filter((s) => s !== "metadata:read").sort())
  })

  it("offers each from the role the server lets grant it", () => {
    for (const c of AGENT_SCOPE_CHOICES) {
      expect({ scope: c.scope, minRole: c.minRole }).toEqual({
        scope: c.scope,
        minRole: adminScopes.includes(c.scope) ? "admin" : "member",
      })
    }
  })
})

describe("AgentAccessCard — revoke", () => {
  it("asks first, DELETEs that token, then refreshes the list", async () => {
    route({ tokens: [mine, theirs] })
    render(<AgentAccessCard />)
    fireEvent.click(await screen.findByRole("button", { name: "Revoke CI agent" }))

    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText(/belongs to ana@example.com/i)).toBeInTheDocument()
    expect(mockFetch.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false)

    fireEvent.click(within(dialog).getByRole("button", { name: /revoke token/i }))
    await waitFor(() => expect(screen.queryByText("CI agent")).toBeNull())
    const del = mockFetch.mock.calls.find(([, i]) => i?.method === "DELETE")!
    expect(del[0]).toBe(API_ENDPOINTS.AGENT_TOKENS.BY_ID("t-2"))
    expect(screen.getByText("Claude Code on my laptop")).toBeInTheDocument()
  })

  it("Cancel sends nothing", async () => {
    route({ tokens: [mine] })
    render(<AgentAccessCard />)
    fireEvent.click(await screen.findByRole("button", { name: "Revoke Claude Code on my laptop" }))
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: /cancel/i }))
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull())
    expect(mockFetch.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false)
  })
})

describe("AI app picker", () => {
  it("sets each app up without a token, and names the apps coming with sign-in", async () => {
    route({ tokens: [mine] })
    const { container } = render(<AgentAccessCard />)
    const picker = await screen.findByRole("tablist", { name: "AI app" })
    const apps = ["Mistral Le Chat", "Copilot in VS Code", "Gemini CLI", "Claude Code", "Cursor"]
    expect(within(picker).getAllByRole("tab").map((t) => t.textContent)).toEqual(apps)
    expect(
      screen.getByText("Coming: connect from Claude (claude.ai), the Gemini app and ChatGPT by signing in to rsync.ai, with no token to copy."),
    ).toBeInTheDocument()

    // Only the token list shows a token's prefix; no app's steps hold any of it.
    await screen.findByText("rsat_abababa…")
    expect(container.textContent).toMatch(/rsat_/) // control: the token list shows the prefix
    for (const name of apps) {
      await userEvent.click(within(picker).getByRole("tab", { name }))
      const panel = screen.getByRole("tabpanel", { name })
      expect(panel).toHaveTextContent(/Agent tokens/) // control: this is the app's steps
      expect(panel.textContent).not.toMatch(/rsat_/)
    }
    // The VS Code tab shows the file that asks for the token.
    await userEvent.click(within(picker).getByRole("tab", { name: "Copilot in VS Code" }))
    expect(screen.getByRole("tabpanel", { name: "Copilot in VS Code" })).toHaveTextContent("${input:rsync-ai-token}")
  })

  it("VS Code prompts for the token as a password, and Gemini CLI names a variable", () => {
    const url = API_ENDPOINTS.AGENT_TOKENS.MCP
    const { vscode, geminiCli } = agentAppSnippets(url)
    const v = JSON.parse(vscode)
    expect(v.inputs[0]).toMatchObject({ type: "promptString", id: "rsync-ai-token", password: true })
    expect(v.servers["rsync-ai"]).toEqual({ type: "http", url, headers: { Authorization: "Bearer ${input:rsync-ai-token}" } })
    const g = JSON.parse(geminiCli)
    expect(g.mcpServers["rsync-ai"]).toEqual({ httpUrl: url, headers: { Authorization: "Bearer ${RSYNC_AGENT_TOKEN}" } })
  })
})

describe("agent_gateway feature flag", () => {
  beforeEach(() => featureFlagsManager.resetFlags())
  afterEach(() => featureFlagsManager.resetFlags())

  it("is off by default and until the API says true", async () => {
    expect(featureFlagsManager.getFlags().agentGateway).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { usage_panel: true }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().agentGateway).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { agent_gateway: "true" }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().agentGateway).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { agent_gateway: true }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().agentGateway).toBe(true)
  })
})
