/**
 * Agent tokens — what lets an AI agent (Claude Code, Cursor, …) call rsync.ai's
 * MCP server as the person who made the token, in the active workspace.
 * handlers/agent_tokens.go
 *   GET    /agent-tokens      -> { tokens: AgentToken[] }  (an admin sees the workspace's)
 *   POST   /agent-tokens       { name, expires_in_days, scopes? } -> NewAgentToken
 *   DELETE /agent-tokens/:id  -> { id, revoked: true }
 *
 * The token itself comes back ONCE, from create. Only its hash is stored, so
 * nothing can show it again; the list carries the first 12 characters.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { extractErrorMessage } from "@/lib/utils/error-handling"

export interface AgentToken {
  id: string
  name: string
  /** The first 12 characters, e.g. "rsat_1a2b3c4". */
  token_prefix: string
  scopes: string[]
  expires_at: string
  revoked_at?: string | null
  last_used_at?: string | null
  created_at: string
  owner_email: string
  /** False on another member's token, which only an admin sees. */
  mine: boolean
}

export interface NewAgentToken {
  id: string
  name: string
  /** The whole token. Shown once; the server keeps only its hash. */
  token: string
  token_prefix: string
  scopes: string[]
  expires_at: string
  workspace_id: string
}

/**
 * The scope that lets an agent generate a connector. Every token reads
 * (metadata:read); only a workspace admin or owner may add this one, and each
 * call still needs the admin role (handlers/agent_tokens.go agentTokenScopes).
 */
export const AGENT_SCOPE_GENERATE = "connectors:generate"

/**
 * The scope that lets an agent create, schedule, run and pause models. Same
 * rules as AGENT_SCOPE_GENERATE: admin or owner to grant, admin on every call.
 * It never lets an agent delete a model or change an existing model's SQL.
 */
export const AGENT_SCOPE_MODELS = "models:write"

/**
 * The scope that lets an agent propose a pipeline. A plan creates nothing: the
 * person opens its link in rsync.ai and clicks Create. Any member may grant it,
 * and each call needs the member role.
 */
export const AGENT_SCOPE_PLAN = "pipelines:plan"

/**
 * The scope that lets an agent draft a workflow (a report or data alert). A
 * draft runs nothing and sends nothing: the person tests it and turns it on in
 * rsync.ai. Admin or owner to grant, admin on every call.
 */
export const AGENT_SCOPE_WORKFLOWS = "workflows:write"

/**
 * The scope that lets an agent draft a dashboard. A draft saves nothing: the
 * person saves it in rsync.ai. Admin or owner to grant, admin on every call.
 */
export const AGENT_SCOPE_DASHBOARDS = "dashboards:write"

/** One scope the card offers as a box, beyond metadata:read, which every token has. */
export interface AgentScopeChoice {
  scope: string
  /** The lowest workspace role the server lets grant it (services.AdminScope). */
  minRole: "member" | "admin"
  /** The box's label: "Can …". */
  label: string
  /** What the box lets the agent do, and what it still cannot. */
  help: string
  /** The badge a token that has it shows in the list. */
  badge: string
}

/**
 * Every scope a token can carry beyond metadata:read, in the order the card
 * offers and sends them. It must name each of the server's scopes
 * (services.TokenScopes): one missing here can only be granted through the API.
 */
export const AGENT_SCOPE_CHOICES: readonly AgentScopeChoice[] = [
  {
    scope: AGENT_SCOPE_PLAN,
    minRole: "member",
    label: "Can propose pipelines",
    help:
      "The agent may propose a pipeline between two of your connections. It gets a link; you open it in " +
      "rsync.ai, check the plan and click Create. The agent cannot create or run a pipeline itself.",
    badge: "Proposes pipelines",
  },
  {
    scope: AGENT_SCOPE_GENERATE,
    minRole: "admin",
    label: "Can generate connectors",
    help:
      "The agent may add a connector to rsync.ai from a public API's OpenAPI document or docs page. " +
      "Every workspace sees new connectors. Credentials are still added only here, in rsync.ai.",
    badge: "Generates connectors",
  },
  {
    scope: AGENT_SCOPE_MODELS,
    minRole: "admin",
    label: "Can build models",
    help:
      "The agent may create silver and gold models, schedule them, run them and pause their schedules. " +
      "It cannot delete a model, change an existing model's SQL or write over a table rsync.ai did not create.",
    badge: "Builds models",
  },
  {
    scope: AGENT_SCOPE_WORKFLOWS,
    minRole: "admin",
    label: "Can draft workflows",
    help:
      "The agent may draft a report or data alert. A draft runs nothing and sends nothing: you test it and " +
      "turn it on in rsync.ai.",
    badge: "Drafts workflows",
  },
  {
    scope: AGENT_SCOPE_DASHBOARDS,
    minRole: "admin",
    label: "Can draft dashboards",
    help: "The agent may draft a dashboard of charts. A draft saves nothing: you save it in rsync.ai.",
    badge: "Drafts dashboards",
  },
]

/** The lifetimes the card offers. The server takes 1-365 days. */
export const AGENT_TOKEN_EXPIRY_DAYS = [30, 90, 365] as const

/** An HTTP failure that keeps its status, so the card can tell 409 from 500. */
export class AgentTokenError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
    this.name = "AgentTokenError"
  }
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: fallback }))
    throw new AgentTokenError(extractErrorMessage(err) || fallback, res.status)
  }
  return res.json() as Promise<T>
}

export async function listAgentTokens(): Promise<AgentToken[]> {
  const res = await authFetch(API_ENDPOINTS.AGENT_TOKENS.LIST)
  const data = await readJSON<{ tokens?: AgentToken[] }>(res, "Could not load agent tokens")
  // A 200 that is not the list (a proxy page, a gateway without the route)
  // must fail, not render "no tokens".
  if (!Array.isArray(data?.tokens)) {
    throw new AgentTokenError("Unexpected agent token response", res.status)
  }
  return data.tokens
}

export async function createAgentToken(input: {
  name: string
  expires_in_days: number
  scopes?: string[]
}): Promise<NewAgentToken> {
  const res = await authFetch(API_ENDPOINTS.AGENT_TOKENS.LIST, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  })
  const data = await readJSON<NewAgentToken>(res, "Could not create the agent token")
  if (typeof data?.token !== "string" || !data.token) {
    throw new AgentTokenError("Unexpected agent token response", res.status)
  }
  return data
}

export async function revokeAgentToken(id: string): Promise<void> {
  const res = await authFetch(API_ENDPOINTS.AGENT_TOKENS.BY_ID(id), { method: "DELETE" })
  await readJSON<{ revoked: boolean }>(res, "Could not revoke the agent token")
}

/** The MCP server's address, as an agent on another machine must call it. */
export function agentGatewayUrl(): string {
  return API_ENDPOINTS.AGENT_TOKENS.MCP
}

/** Setup commands for the two clients the card documents. */
export function agentSetupSnippets(url: string, token: string): { claudeCode: string; cursor: string } {
  return {
    claudeCode: `claude mcp add --transport http rsync-ai ${url} --header "Authorization: Bearer ${token}"`,
    cursor: JSON.stringify(
      { mcpServers: { "rsync-ai": { url, headers: { Authorization: `Bearer ${token}` } } } },
      null,
      2,
    ),
  }
}

/**
 * Setup for the apps the "Connect your AI app" cards list, shown before any
 * token exists. They never hold a token: VS Code asks for it (an input with
 * password: true, so it is not saved in the file) and Gemini CLI reads it from
 * the RSYNC_AGENT_TOKEN environment variable.
 */
export function agentAppSnippets(url: string): { vscode: string; geminiCli: string } {
  return {
    vscode: JSON.stringify(
      {
        inputs: [
          { type: "promptString", id: "rsync-ai-token", description: "rsync.ai agent token", password: true },
        ],
        servers: {
          "rsync-ai": { type: "http", url, headers: { Authorization: "Bearer ${input:rsync-ai-token}" } },
        },
      },
      null,
      2,
    ),
    geminiCli: JSON.stringify(
      { mcpServers: { "rsync-ai": { httpUrl: url, headers: { Authorization: "Bearer ${RSYNC_AGENT_TOKEN}" } } } },
      null,
      2,
    ),
  }
}
