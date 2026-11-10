"use client"

import { useCallback, useEffect, useState } from "react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Checkbox } from "@/components/ui/checkbox"
import { LocalDateTime } from "@/components/ui/local-date-time"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { AlertTriangle, Bot, KeyRound, Loader2 } from "lucide-react"
import { AgentAppCards } from "./AgentAppCards"
import { CopyBlock } from "./CopyBlock"
import { toast } from "sonner"
import { cn } from "@/lib/utils"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import {
  AGENT_SCOPE_CHOICES,
  AGENT_TOKEN_EXPIRY_DAYS,
  agentGatewayUrl,
  agentSetupSnippets,
  createAgentToken,
  listAgentTokens,
  revokeAgentToken,
  type AgentToken,
  type NewAgentToken,
} from "@/lib/api/agent-tokens"

const EXPIRY_LABEL: Record<(typeof AGENT_TOKEN_EXPIRY_DAYS)[number], string> = {
  30: "30 days",
  90: "90 days",
  365: "1 year",
}

/**
 * Two cards: "Connect your AI app" (pick an app, see its steps) and "Agent
 * tokens" for rsync.ai's MCP server (make one, list them, revoke).
 *
 * A token acts as the person who made it, in the workspace open when it was
 * made, and reads metadata only (connections, pipelines, runs — never row
 * data). Each box in AGENT_SCOPE_CHOICES adds a scope: a member may let it
 * propose pipelines, which they then create in rsync.ai; an admin or owner may
 * also let it generate connectors, build models (whose SQL builds tables from
 * the workspace's data, in its own database), and draft workflows and
 * dashboards. It is shown once, right after it is made, with the setup
 * commands for Claude Code and Cursor. An admin also sees, and can revoke,
 * every member's token in the workspace.
 *
 * Rendered only when /api/v1/features reports agent_gateway, because the
 * routes behind it exist only then.
 */
export function AgentAccessCard() {
  const [tokens, setTokens] = useState<AgentToken[]>([])
  const [status, setStatus] = useState<"loading" | "failed" | "ready">("loading")
  const [name, setName] = useState("")
  const [days, setDays] = useState<number>(30)
  const [ticked, setTicked] = useState<ReadonlySet<string>>(new Set())
  const { meets, isLoading: roleLoading } = useWorkspaceRole()
  // The server refuses an admin scope below admin (agentTokenScopes). Any user
  // may grant pipelines:plan, but a viewer's token is refused on every call,
  // so the card offers it from member up.
  const offered = roleLoading ? [] : AGENT_SCOPE_CHOICES.filter((c) => meets(c.minRole))
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<NewAgentToken | null>(null)
  const [confirm, setConfirm] = useState<AgentToken | null>(null)
  const [revoking, setRevoking] = useState(false)

  // State is only set after the await, so the mount effect never sets state
  // synchronously; Retry flips back to "loading" itself.
  const fetchTokens = useCallback(async () => {
    try {
      setTokens(await listAgentTokens())
      setStatus("ready")
    } catch {
      setStatus("failed")
    }
  }, [])

  useEffect(() => {
    void fetchTokens()
  }, [fetchTokens])

  const retry = () => {
    setStatus("loading")
    void fetchTokens()
  }

  const create = async () => {
    if (!name.trim() || creating) return
    setCreating(true)
    const scopes = offered.filter((c) => ticked.has(c.scope)).map((c) => c.scope)
    try {
      const token = await createAgentToken({
        name: name.trim(),
        expires_in_days: days,
        ...(scopes.length > 0 ? { scopes } : {}),
      })
      setCreated(token)
      setName("")
      setTicked(new Set())
      await fetchTokens()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not create the agent token")
    } finally {
      setCreating(false)
    }
  }

  const revoke = async () => {
    if (!confirm) return
    setRevoking(true)
    try {
      await revokeAgentToken(confirm.id)
      toast.success(`Revoked "${confirm.name}"`)
      if (created?.id === confirm.id) setCreated(null)
      setConfirm(null)
      await fetchTokens()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not revoke the agent token")
    } finally {
      setRevoking(false)
    }
  }

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Bot className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
            Connect your AI app
          </CardTitle>
          <CardDescription>
            Let your own AI app read this workspace&apos;s connections, pipelines and runs through rsync.ai&apos;s MCP
            server. A token acts as you, in this workspace only, and reads metadata, never your rows. With
            &quot;Can build models&quot; it can also build tables from your data, inside your own database.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <AgentAppCards />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <KeyRound className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
            Agent tokens
          </CardTitle>
          <CardDescription>
            A token is shown once, when you create it. An admin also sees, and can revoke, every member&apos;s token.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          {created ? (
            <NewTokenPanel token={created} onDone={() => setCreated(null)} />
          ) : (
            <form
              className="space-y-3"
              onSubmit={(e) => {
                e.preventDefault()
                void create()
              }}
            >
              <div className="space-y-2">
                <Label htmlFor="agent-token-name">Token name</Label>
                <Input
                  id="agent-token-name"
                  placeholder="e.g. Claude Code on my laptop"
                  value={name}
                  maxLength={100}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              {offered.map((c) => {
                const id = `agent-token-${c.scope.replace(":", "-")}`
                return (
                  <div key={c.scope} className="flex items-start gap-2">
                    <Checkbox
                      id={id}
                      checked={ticked.has(c.scope)}
                      onCheckedChange={(v) =>
                        setTicked((prev) => {
                          const next = new Set(prev)
                          if (v === true) next.add(c.scope)
                          else next.delete(c.scope)
                          return next
                        })
                      }
                      className="mt-0.5"
                    />
                    <div className="space-y-1">
                      <Label htmlFor={id}>{c.label}</Label>
                      <p className="text-xs text-zinc-500 dark:text-zinc-400">{c.help}</p>
                    </div>
                  </div>
                )
              })}
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex flex-wrap items-center gap-2" role="group" aria-label="Expires after">
                  <span className="text-sm text-zinc-500 dark:text-zinc-400">Expires after</span>
                  {AGENT_TOKEN_EXPIRY_DAYS.map((d) => (
                    <Button
                      key={d}
                      type="button"
                      variant="outline"
                      size="sm"
                      aria-pressed={days === d}
                      className={cn(
                        days === d &&
                          "border-violet-500 bg-violet-50 text-violet-700 dark:bg-violet-950 dark:text-violet-300",
                      )}
                      onClick={() => setDays(d)}
                    >
                      {EXPIRY_LABEL[d]}
                    </Button>
                  ))}
                </div>
                <Button type="submit" disabled={creating || !name.trim()}>
                  {creating ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <KeyRound className="mr-2 h-4 w-4" />}
                  Create token
                </Button>
              </div>
            </form>
          )}

          <div className="border-t border-zinc-200 pt-4 dark:border-zinc-800">
            {status === "loading" && (
              <p className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
                <Loader2 className="h-4 w-4 animate-spin" /> Loading agent tokens…
              </p>
            )}
            {status === "failed" && (
              <div className="flex items-center justify-between gap-3">
                <p className="flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-300">
                  <AlertTriangle className="h-4 w-4 text-amber-500" />
                  Could not load agent tokens.
                </p>
                <Button variant="outline" size="sm" onClick={retry}>
                  Retry
                </Button>
              </div>
            )}
            {status === "ready" && tokens.length === 0 && (
              <p className="text-sm text-zinc-500 dark:text-zinc-400">No agent tokens in this workspace yet.</p>
            )}
            {status === "ready" && tokens.length > 0 && (
              <ul className="-my-3 divide-y divide-zinc-200 dark:divide-zinc-800" aria-label="Agent tokens">
                {tokens.map((t) => (
                  <TokenRow key={t.id} token={t} onRevoke={() => setConfirm(t)} />
                ))}
              </ul>
            )}
          </div>
        </CardContent>
      </Card>

      <AlertDialog
        open={confirm !== null}
        onOpenChange={(open) => {
          if (!open) setConfirm(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke agent token?</AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.mine === false
                ? `"${confirm?.name}" belongs to ${confirm?.owner_email}. Any agent using it loses access on its next call.`
                : `Any agent using "${confirm?.name}" loses access on its next call.`}{" "}
              This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revoking}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault() // keep open until the request settles / on error
                void revoke()
              }}
              disabled={revoking}
              className="bg-red-600 text-white hover:bg-red-700 focus:ring-red-600 dark:bg-red-600 dark:hover:bg-red-700"
            >
              {revoking && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Revoke token
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

function tokenState(t: AgentToken): "revoked" | "expired" | "active" {
  if (t.revoked_at) return "revoked"
  if (new Date(t.expires_at).getTime() <= Date.now()) return "expired"
  return "active"
}

function TokenRow({ token, onRevoke }: { token: AgentToken; onRevoke: () => void }) {
  const state = tokenState(token)
  return (
    <li className="flex flex-wrap items-center justify-between gap-3 py-3">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2 font-medium">
          <span className="truncate">{token.name}</span>
          <code className="rounded bg-zinc-100 px-1.5 py-0.5 text-xs text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
            {token.token_prefix}…
          </code>
          {state === "revoked" && <Badge variant="destructive">Revoked</Badge>}
          {state === "expired" && <Badge variant="secondary">Expired</Badge>}
          {AGENT_SCOPE_CHOICES.filter((c) => token.scopes?.includes(c.scope)).map((c) => (
            <Badge key={c.scope} variant="outline">
              {c.badge}
            </Badge>
          ))}
        </div>
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          {!token.mine && <>{token.owner_email} · </>}
          Last used <LocalDateTime value={token.last_used_at} fallback="never" /> · Expires{" "}
          <LocalDateTime value={token.expires_at} />
        </p>
      </div>
      {state === "active" && (
        <Button variant="outline" size="sm" onClick={onRevoke} aria-label={`Revoke ${token.name}`}>
          Revoke
        </Button>
      )}
    </li>
  )
}

function NewTokenPanel({ token, onDone }: { token: NewAgentToken; onDone: () => void }) {
  const url = agentGatewayUrl()
  const snippets = agentSetupSnippets(url, token.token)
  return (
    <div className="space-y-4 rounded-md border border-amber-300 bg-amber-50 p-4 dark:border-amber-800 dark:bg-amber-950/30">
      <p className="flex items-center gap-2 text-sm font-medium text-amber-800 dark:text-amber-300">
        <AlertTriangle className="h-4 w-4" />
        Copy &quot;{token.name}&quot; now. It will not be shown again.
      </p>
      <CopyBlock label="Token" value={token.token} />
      <CopyBlock label="Claude Code" value={snippets.claudeCode} />
      <CopyBlock label="Cursor (.cursor/mcp.json)" value={snippets.cursor} />
      <p className="text-xs text-zinc-600 dark:text-zinc-400">
        Expires <LocalDateTime value={token.expires_at} />. Keep it out of shared files and chats; revoke it here if it
        leaks.
      </p>
      <div className="flex justify-end">
        <Button variant="outline" size="sm" onClick={onDone}>
          Done
        </Button>
      </div>
    </div>
  )
}
