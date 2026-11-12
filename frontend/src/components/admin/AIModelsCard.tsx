"use client"

import { useCallback, useEffect, useState } from "react"
import { AlertTriangle, CheckCircle2, KeyRound, Loader2, Sparkles, XCircle } from "lucide-react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
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
import { Switch } from "@/components/ui/switch"
import {
  adminListModelKeys,
  adminRemoveModelKey,
  adminSetModelKey,
  adminSetSampleRows,
  adminTestModelKey,
  type AdminModelEntry,
  type AdminModelList,
  type AdminModelTest,
} from "@/lib/api/assistant"
import { useFlagState } from "@/config/features"

/**
 * Admin > Settings > AI models: the keys the in-app assistant answers with.
 *
 * Platform admins only (the routes sit behind AdminRoleMiddleware). A key is
 * write-only: the API never returns it, and this card drops it from state as
 * soon as it is sent. A key saved here wins over the environment file's; Remove
 * falls back to that one. Rendered only when /api/v1/features reports
 * assistant, because the routes exist only then. The sample-rows switches show
 * only on an install that allows sample rows (llmSampleRows); the gateway
 * refuses an approval anywhere else.
 */
export function AIModelsCard() {
  const [list, setList] = useState<AdminModelList | null>(null)
  const [status, setStatus] = useState<"loading" | "failed" | "ready">("loading")
  const [editing, setEditing] = useState<AdminModelEntry | null>(null)
  const [removing, setRemoving] = useState<AdminModelEntry | null>(null)
  const [busy, setBusy] = useState(false)
  const [tests, setTests] = useState<Record<string, AdminModelTest | "running">>({})
  // Turning sample rows on asks first; turning them off does not.
  const [approving, setApproving] = useState<AdminModelEntry | null>(null)
  const sampleRows = useFlagState("llmSampleRows")

  const fetchList = useCallback(async () => {
    try {
      setList(await adminListModelKeys())
      setStatus("ready")
    } catch {
      setStatus("failed")
    }
  }, [])

  useEffect(() => {
    void fetchList()
  }, [fetchList])

  // A saved or removed key makes the last test result stale.
  const clearTest = (entry: string) =>
    setTests((t) => {
      const next = { ...t }
      delete next[entry]
      return next
    })

  const retry = () => {
    setStatus("loading")
    void fetchList()
  }

  const save = async (entry: AdminModelEntry, apiKey: string, model: string) => {
    setBusy(true)
    try {
      setList(await adminSetModelKey(entry.entry, { apiKey: apiKey || undefined, model }))
      clearTest(entry.entry)
      toast.success(apiKey ? `Saved the ${entry.label} key` : `Saved the ${entry.label} model`)
      setEditing(null)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not save the key")
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    if (!removing) return
    setBusy(true)
    try {
      setList(await adminRemoveModelKey(removing.entry))
      clearTest(removing.entry)
      toast.success(`Removed the ${removing.label} key`)
      setRemoving(null)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not remove the key")
    } finally {
      setBusy(false)
    }
  }

  const setSampleRows = async (entry: AdminModelEntry, allowed: boolean) => {
    setBusy(true)
    try {
      setList(await adminSetSampleRows(entry.entry, allowed))
      toast.success(
        allowed ? `${entry.label} may now get sample rows where an owner turns them on` : `${entry.label} gets no sample rows`,
      )
      setApproving(null)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Could not save the setting")
    } finally {
      setBusy(false)
    }
  }

  const test = async (entry: AdminModelEntry) => {
    setTests((t) => ({ ...t, [entry.entry]: "running" }))
    try {
      const result = await adminTestModelKey(entry.entry)
      setTests((t) => ({ ...t, [entry.entry]: result }))
    } catch (err) {
      setTests((t) => ({
        ...t,
        [entry.entry]: {
          ok: false,
          entry: entry.entry,
          message: err instanceof Error ? err.message : "Could not test the key",
        },
      }))
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Sparkles className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
          AI models
        </CardTitle>
        <CardDescription>
          The keys the in-app assistant (&quot;Ask rsync.ai&quot;) answers with. A model shows in its picker once it has a
          key. A key saved here is stored encrypted, is never shown again, and wins over the environment file&apos;s;
          removing it falls back to that one. The model gets names, settings and the user&apos;s question, never row
          data
          {sampleRows === "on" &&
            ", unless you allow sample rows for it below and a workspace owner turns them on: then up to 5 rows of a table, with personal and secret columns withheld"}
          {sampleRows === "off" && ": sample rows are turned off on this install"}. For Gemini, use a paid key: on
          Google&apos;s free tier, prompts may be used to improve Google&apos;s products.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {status === "loading" && (
          <p className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" /> Loading AI models…
          </p>
        )}
        {status === "failed" && (
          <div className="flex items-center justify-between gap-3">
            <p className="flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-300">
              <AlertTriangle className="h-4 w-4 text-amber-500" />
              Could not load the AI models.
            </p>
            <Button variant="outline" size="sm" onClick={retry}>
              Retry
            </Button>
          </div>
        )}
        {status === "ready" && list && (
          <>
            {!list.env_known && (
              <p className="flex items-center gap-2 text-xs text-amber-700 dark:text-amber-300">
                <AlertTriangle className="h-3.5 w-3.5" />
                The model service did not answer, so keys from the environment file are not shown.
              </p>
            )}
            <ul className="divide-y divide-zinc-200 dark:divide-zinc-800" aria-label="AI models">
              {list.entries.map((entry) => (
                <ModelRow
                  key={entry.entry}
                  entry={entry}
                  editing={editing?.entry === entry.entry}
                  busy={busy}
                  test={tests[entry.entry]}
                  onEdit={() => setEditing(entry)}
                  onCancel={() => setEditing(null)}
                  onSave={(apiKey, model) => void save(entry, apiKey, model)}
                  onRemove={() => setRemoving(entry)}
                  onTest={() => void test(entry)}
                  sampleRowsKnown={list.sample_rows_known === true && sampleRows === "on"}
                  onSampleRows={(allowed) => (allowed ? setApproving(entry) : void setSampleRows(entry, false))}
                />
              ))}
            </ul>
          </>
        )}
      </CardContent>

      <AlertDialog
        open={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove the {removing?.label} key?</AlertDialogTitle>
            <AlertDialogDescription>
              {removing?.env_key_set
                ? `Questions to ${removing?.label} then use the environment file's key.`
                : `${removing?.label} then leaves the assistant's picker: the environment file sets no key for it.`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault() // keep open until the request settles
                void remove()
              }}
              disabled={busy}
              className="bg-red-600 text-white hover:bg-red-700 focus:ring-red-600 dark:bg-red-600 dark:hover:bg-red-700"
            >
              {busy && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Remove key
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        open={approving !== null}
        onOpenChange={(open) => {
          if (!open) setApproving(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Allow sample rows to {approving?.label}?</AlertDialogTitle>
            <AlertDialogDescription>
              In a workspace whose owner turns sample rows on, Ask rsync.ai may then send {approving?.label} up to 5 rows
              of a table, values cut to 64 characters, to see what the data looks like. Columns whose name or a PII
              scan marks as personal or secret are withheld, and every value is checked for personal data before it
              is sent. Each read is audited. Nothing changes until an owner turns it on.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault() // keep open until the request settles
                if (approving) void setSampleRows(approving, true)
              }}
              disabled={busy}
            >
              {busy && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Allow sample rows
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  )
}

function sourceBadge(entry: AdminModelEntry) {
  if (entry.source === "panel") return <Badge variant="secondary">Key saved here</Badge>
  if (entry.source === "env") return <Badge variant="secondary">Key from the environment file</Badge>
  return <Badge variant="outline">No key</Badge>
}

function answeringModel(entry: AdminModelEntry): string {
  if (entry.source === "panel") return entry.model || entry.default_model
  if (entry.source === "env") return entry.env_model ?? ""
  return ""
}

function ModelRow({
  entry,
  editing,
  busy,
  test,
  onEdit,
  onCancel,
  onSave,
  onRemove,
  onTest,
  sampleRowsKnown,
  onSampleRows,
}: {
  entry: AdminModelEntry
  editing: boolean
  busy: boolean
  test?: AdminModelTest | "running"
  onEdit: () => void
  onCancel: () => void
  onSave: (apiKey: string, model: string) => void
  onRemove: () => void
  onTest: () => void
  sampleRowsKnown: boolean
  onSampleRows: (allowed: boolean) => void
}) {
  const model = answeringModel(entry)
  const switchId = `ai-model-sample-rows-${entry.entry}`
  return (
    <li className="space-y-3 py-3" aria-label={entry.label}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <p className="flex flex-wrap items-center gap-2 font-medium">
            {entry.label}
            {sourceBadge(entry)}
          </p>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            {model && (
              <>
                Model <code className="font-mono">{model}</code>
              </>
            )}
            {entry.source === "panel" && entry.updated_at && (
              <>
                {model && " · "}Set{entry.updated_by ? ` by ${entry.updated_by}` : ""}{" "}
                <LocalDateTime value={entry.updated_at} />
              </>
            )}
            {entry.source === "panel" && entry.env_key_set && " · the environment file also sets one"}
          </p>
        </div>
        {!editing && (
          <div className="flex flex-wrap gap-2">
            {entry.source !== "none" && (
              <Button
                variant="outline"
                size="sm"
                onClick={onTest}
                disabled={test === "running"}
                aria-label={`Test ${entry.label}`}
              >
                {test === "running" && <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />}
                Test
              </Button>
            )}
            <Button variant="outline" size="sm" onClick={onEdit} aria-label={`Set ${entry.label} key`}>
              <KeyRound className="mr-1.5 h-3.5 w-3.5" />
              {entry.panel_key_set ? "Change" : "Set key"}
            </Button>
            {entry.panel_key_set && (
              <Button variant="outline" size="sm" onClick={onRemove} aria-label={`Remove ${entry.label} key`}>
                Remove
              </Button>
            )}
          </div>
        )}
      </div>
      {sampleRowsKnown && (
        <div className="flex items-center gap-2">
          <Switch
            id={switchId}
            checked={entry.sample_rows === true}
            disabled={busy}
            onCheckedChange={onSampleRows}
            aria-label={`Allow sample rows to ${entry.label}`}
          />
          <Label htmlFor={switchId} className="text-xs font-normal text-zinc-600 dark:text-zinc-300">
            Sample rows: up to 5 rows of a table, where a workspace owner turns them on
          </Label>
        </div>
      )}
      {test && test !== "running" && <TestResult result={test} />}
      {editing && <KeyForm entry={entry} busy={busy} onCancel={onCancel} onSave={onSave} />}
    </li>
  )
}

function TestResult({ result }: { result: AdminModelTest }) {
  if (result.ok) {
    return (
      <p className="flex items-center gap-1.5 text-xs text-emerald-700 dark:text-emerald-300" role="status">
        <CheckCircle2 className="h-3.5 w-3.5" />
        {result.label ?? "The model"} answered{result.model ? ` with ${result.model}` : ""}.
      </p>
    )
  }
  return (
    <p className="flex items-center gap-1.5 text-xs text-red-700 dark:text-red-300" role="status">
      <XCircle className="h-3.5 w-3.5" />
      The test failed: {result.message || result.error || "no answer"}
    </p>
  )
}

// The key lives in this form's state only until Save; the row unmounts the
// form on success, so it is gone from memory and the page.
function KeyForm({
  entry,
  busy,
  onCancel,
  onSave,
}: {
  entry: AdminModelEntry
  busy: boolean
  onCancel: () => void
  onSave: (apiKey: string, model: string) => void
}) {
  const [apiKey, setApiKey] = useState("")
  const [model, setModel] = useState(entry.panel_key_set ? entry.model : "")
  const keyId = `ai-model-key-${entry.entry}`
  const modelId = `ai-model-id-${entry.entry}`
  const canSave = !busy && (apiKey.trim() !== "" || entry.panel_key_set)
  return (
    <form
      className="space-y-3 rounded-md border border-zinc-200 p-3 dark:border-zinc-800"
      onSubmit={(e) => {
        e.preventDefault()
        if (canSave) onSave(apiKey.trim(), model.trim())
      }}
    >
      <div className="space-y-1.5">
        <Label htmlFor={keyId}>{entry.label} API key</Label>
        <Input
          id={keyId}
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={apiKey}
          maxLength={512}
          placeholder={entry.panel_key_set ? "Leave empty to keep the saved key" : "Paste the key"}
          onChange={(e) => setApiKey(e.target.value)}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor={modelId}>Model id (optional)</Label>
        <Input
          id={modelId}
          value={model}
          maxLength={100}
          placeholder={entry.default_model}
          onChange={(e) => setModel(e.target.value)}
        />
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          Empty uses {entry.default_model || "the default"}. A model id only, never an address.
        </p>
      </div>
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button type="submit" size="sm" disabled={!canSave}>
          {busy && <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />}
          Save
        </Button>
      </div>
    </form>
  )
}
