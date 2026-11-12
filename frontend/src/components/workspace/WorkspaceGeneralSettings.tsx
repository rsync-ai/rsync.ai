"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { useWorkspace } from "@/contexts/WorkspaceContext"
import { can } from "@/lib/workspace/roles"
import { useUsagePanelState } from "@/config/features"
import type { WorkspaceUsage } from "@/lib/api/usage"
import { toast } from "sonner"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { SettingRow, SettingRows } from "@/components/kit/SettingRow"
import { CopyId } from "@/components/kit/CopyId"
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
import { Building2, Check, Loader2, LogOut } from "lucide-react"
import type { ApiErrorBody } from "@/lib/api/types"

interface WorkspaceGeneralSettingsProps {
  workspaceId: string
  workspaceName: string
  slug?: string
  /** The caller's role in this workspace; gates rename. */
  currentRole: string
  /** Personal identity-anchor workspace — rename/leave are not offered. */
  isPersonal?: boolean
}

/**
 * General tab of the workspace settings hub, one row per setting: name and
 * rename (admin+), slug, workspace ID (copyable), plan (where plans exist), and leave (any member,
 * hidden on personal). Every mutation hits
 * an existing api-gateway endpoint and the backend re-enforces the guard; this
 * component mirrors the rules for UX and surfaces the last-owner 409 on leave.
 */
export function WorkspaceGeneralSettings({
  workspaceId,
  workspaceName,
  slug,
  currentRole,
  isPersonal = false,
}: WorkspaceGeneralSettingsProps) {
  const router = useRouter()
  const { refresh } = useWorkspace()
  const usagePanel = useUsagePanelState()

  // A personal workspace is the user's identity anchor — it can't be renamed or
  // left (the backend refuses both with 409). Gate the UI upfront so the user isn't
  // told only after clicking, matching how the Leave card hides itself.
  const canRename = can(currentRole, "rename_workspace") && !isPersonal
  const canLeave = can(currentRole, "leave_workspace") && !isPersonal

  const [name, setName] = useState(workspaceName)
  const [saving, setSaving] = useState(false)
  // Synchronous in-flight latch: the disabled-button/`saving` state updates async,
  // so Enter + a fast double-click can fire multiple PATCHes in one tick before the
  // re-render. The ref flips immediately, so re-entrant calls bail here.
  const savingRef = useRef(false)
  const [currentUserId, setCurrentUserId] = useState<string | null>(null)
  const [leaveOpen, setLeaveOpen] = useState(false)
  const [leaving, setLeaving] = useState(false)

  // Keep the field in sync if the active workspace changes under us.
  useEffect(() => setName(workspaceName), [workspaceName])

  // Leaving DELETEs the caller's OWN membership, so we need the caller's user id.
  // There is no client auth context; read it from /auth/me (same as WorkspaceMembers).
  const loadCurrentUser = useCallback(async () => {
    try {
      const res = await authFetch(API_ENDPOINTS.AUTH.ME)
      if (!res.ok) return
      const data = (await res.json()) as { user_id?: string }
      if (data.user_id) setCurrentUserId(data.user_id)
    } catch {
      // non-fatal; the Leave confirm stays disabled until we know who we are
    }
  }, [])

  useEffect(() => {
    if (canLeave) void loadCurrentUser()
  }, [canLeave, loadCurrentUser])

  const trimmed = name.trim()
  const nameChanged = trimmed !== "" && trimmed !== workspaceName
  const canSave = canRename && nameChanged && !saving

  const handleRename = async () => {
    if (!canSave || savingRef.current) return // re-entrant guard (double-submit race)
    savingRef.current = true
    setSaving(true)
    try {
      const res = await authFetch(API_ENDPOINTS.WORKSPACES.UPDATE(workspaceId), {
        method: "PATCH",
        body: JSON.stringify({ name: trimmed }),
      })
      if (!res.ok) {
        const err = (await res.json().catch(() => ({}))) as ApiErrorBody
        toast.error(err.error ?? "Failed to rename workspace")
        return
      }
      toast.success(`Renamed to "${trimmed}"`)
      void refresh() // update the header switcher + other consumers
    } catch {
      toast.error("Failed to rename workspace")
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  const handleLeave = async () => {
    if (leaving || !currentUserId) return
    setLeaving(true)
    try {
      const res = await authFetch(API_ENDPOINTS.WORKSPACES.MEMBER_DELETE(workspaceId, currentUserId), {
        method: "DELETE",
      })
      if (!res.ok) {
        const err = (await res.json().catch(() => ({}))) as ApiErrorBody
        const msg =
          res.status === 409
            ? err.error ??
              "You're the last owner. Transfer ownership to someone else, or delete the workspace instead."
            : res.status === 404
              ? "You're no longer a member of this workspace."
              : err.error ?? "Failed to leave workspace"
        toast.error(msg)
        return // keep the dialog open so the reason is visible
      }
      // 204 — left. Re-fetch the workspace list through the shared provider, which
      // re-resolves the active selection and re-points (or clears) it when it was the
      // workspace we just left — so the next request's X-Workspace-ID can't be a
      // workspace we no longer belong to. One refetch instead of three.
      await refresh()
      toast.success(`You left "${workspaceName}"`)
      setLeaveOpen(false)
      router.push("/")
    } catch {
      toast.error("Failed to leave workspace")
    } finally {
      setLeaving(false)
    }
  }

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Building2 className="h-4 w-4 text-violet-500" />
            General
          </CardTitle>
          <CardDescription>This workspace&apos;s name and your membership in it.</CardDescription>
        </CardHeader>
        <CardContent>
          {/* No "Your role" row: the role badge beside the page title shows it,
              so this tab printed it a second time (#56). */}
          <SettingRows>
            <SettingRow label="Workspace name" htmlFor="ws-name" hint="Shown in the workspace switcher at the top of every page.">
              <div className="flex items-center gap-2">
                <Input
                  id="ws-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && canSave && void handleRename()}
                  disabled={!canRename || saving}
                  aria-describedby="ws-name-hint"
                  className="max-w-sm"
                />
                {canRename && (
                  // Outlined until there is a change to save: a solid button that
                  // is greyed out before any edit read as a broken form.
                  <Button
                    variant={nameChanged ? "default" : "outline"}
                    onClick={() => void handleRename()}
                    disabled={!canSave}
                    className="gap-1.5"
                  >
                    {saving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Check className="h-3.5 w-3.5" />}
                    Save
                  </Button>
                )}
              </div>
              {isPersonal ? (
                <p className="mt-1.5 text-xs text-zinc-500 dark:text-zinc-400">
                  This is your personal workspace — its name is fixed and can&apos;t be changed.
                </p>
              ) : !canRename ? (
                <p className="mt-1.5 text-xs text-zinc-500 dark:text-zinc-400">
                  Only admins and owners can rename this workspace.
                </p>
              ) : null}
            </SettingRow>

            {/* Slug (read-only — derived by the backend, stable identifier) */}
            <SettingRow label="Slug">
              <code className="block w-fit rounded bg-zinc-100 px-2 py-1 font-mono text-sm text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300">
                {slug || "—"}
              </code>
            </SettingRow>

            {/* Every member sees it: scripts and the API name the workspace by
                its id (the X-Workspace-ID header), and it was shown nowhere. */}
            <SettingRow
              label="Workspace ID"
              hint={<>Scripts and the API name this workspace by its ID, in the X-Workspace-ID header.</>}
            >
              <CopyId value={workspaceId} label="workspace ID" full className="px-2 py-1 text-sm" />
            </SettingRow>

            {/* Plans exist only where the usage panel is on; keyed so a switch
                to another workspace reads its plan afresh. */}
            {usagePanel === "on" && <PlanRow key={workspaceId} />}

            {canLeave && (
              <SettingRow
                label="Leave workspace"
                hint={<>You lose access to its pipelines and dashboards. You&apos;ll need a new invite to rejoin.</>}
              >
                <Button variant="outline" onClick={() => setLeaveOpen(true)} className="gap-1.5">
                  <LogOut className="h-3.5 w-3.5" />
                  Leave workspace
                </Button>
              </SettingRow>
            )}
          </SettingRows>
        </CardContent>
      </Card>

      <AlertDialog open={leaveOpen} onOpenChange={(open) => !open && setLeaveOpen(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Leave this workspace?</AlertDialogTitle>
            <AlertDialogDescription>
              You&apos;ll immediately lose access to {workspaceName ? `"${workspaceName}"` : "this workspace"}{" "}
              and its connections, pipelines and runs. You can be re-invited later.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={leaving}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault() // keep open until the request settles / on error
                void handleLeave()
              }}
              disabled={leaving || !currentUserId}
              className="bg-red-600 text-white hover:bg-red-700 focus:ring-red-600 dark:bg-red-600 dark:hover:bg-red-700"
            >
              {leaving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Leave
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

type PlanRead = { kind: "loading" } | { kind: "failed" } | { kind: "ready"; usage: WorkspaceUsage }

// The same read the Usage page makes. authFetch sends the active workspace,
// which is the one these settings are for.
async function readPlan(): Promise<PlanRead> {
  try {
    const res = await authFetch("/api/v1/usage", { method: "GET" })
    if (!res.ok) return { kind: "failed" }
    return { kind: "ready", usage: (await res.json()) as WorkspaceUsage }
  } catch {
    return { kind: "failed" }
  }
}

const count = (n: number) => n.toLocaleString("en-US")

/** "Pro · 14 pipelines", or "Free · 1 of 2 pipelines" on a plan with a limit. */
function planSummary({ plan, pipelines_used: used, pipeline_limit: limit }: WorkspaceUsage) {
  const name = plan ? plan.charAt(0).toUpperCase() + plan.slice(1) : "No plan"
  const pipelines =
    limit == null ? `${count(used)} pipeline${used === 1 ? "" : "s"}` : `${count(used)} of ${count(limit)} pipelines`
  return `${name} · ${pipelines}`
}

function PlanRow() {
  const [read, setRead] = useState<PlanRead>({ kind: "loading" })

  useEffect(() => {
    let live = true
    void readPlan().then((next) => {
      if (live) setRead(next)
    })
    return () => {
      live = false
    }
  }, [])

  return (
    <SettingRow label="Plan" hint="What this workspace can use.">
      <div className="flex flex-wrap items-center gap-3">
        <p className="text-sm text-zinc-900 dark:text-zinc-100">
          {read.kind === "ready" ? (
            planSummary(read.usage)
          ) : read.kind === "failed" ? (
            <span className="text-zinc-500 dark:text-zinc-400">The plan did not load.</span>
          ) : (
            <span className="text-zinc-500 dark:text-zinc-400">Loading…</span>
          )}
        </p>
        <Button asChild variant="outline" size="sm">
          <Link href="/usage">View usage</Link>
        </Button>
      </div>
    </SettingRow>
  )
}
