"use client"

/**
 * Whether Ask rsync.ai may read sample rows in this workspace: GET/PUT
 * /api/v1/workspace/llm-data (api-gateway handlers/assistant_samples.go).
 *
 * Two switches must both be on before a model sees a value: a platform admin
 * approves a model in Admin > Settings > AI models, and the workspace owner
 * turns sample rows on here. Any member sees the setting; only the owner gets
 * the switch, mirroring security.WSOwner on the PUT. The route exists only
 * while the assistant is on, so a 404 renders nothing. An install that does
 * not allow sample rows (llmSampleRows; the gateway refuses the PUT there) gets
 * the names-only explainer and no switch.
 */

import { useCallback, useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { Loader2, ShieldCheck } from "lucide-react"

import { useFlagState } from "@/config/features"
import {
  getWorkspaceLLMData,
  modelNames,
  setWorkspaceLLMData,
  type WorkspaceLLMData,
} from "@/lib/api/assistant"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
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

export function WorkspaceLLMDataCard() {
  const [data, setData] = useState<WorkspaceLLMData | null>(null)
  const [state, setState] = useState<"loading" | "off" | "failed" | "ready">("loading")
  const [confirming, setConfirming] = useState(false)
  const [saving, setSaving] = useState(false)
  const savingRef = useRef(false)
  const install = useFlagState("llmSampleRows")

  const load = useCallback(async () => {
    try {
      const got = await getWorkspaceLLMData()
      setData(got)
      setState(got ? "ready" : "off")
    } catch {
      setState("failed")
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const save = async (on: boolean) => {
    if (savingRef.current) return
    savingRef.current = true
    setSaving(true)
    try {
      setData(await setWorkspaceLLMData(on ? "sample_rows" : "names_only"))
      toast.success(on ? "Ask rsync.ai may now read sample rows in this workspace" : "Ask rsync.ai reads names only")
      setConfirming(false)
    } catch (err) {
      toast.error(err instanceof Error ? `Could not save the setting: ${err.message}` : "Could not save the setting")
      setConfirming(false)
      // Show what is saved, not what was asked for.
      void load()
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  if (state === "off") return null

  const title = (
    <CardTitle className="flex items-center gap-2 text-sm font-medium">
      <ShieldCheck className="h-4 w-4" />
      Ask rsync.ai and your data
    </CardTitle>
  )
  if (install === "off") {
    return (
      <Card>
        <CardHeader>
          {title}
          <CardDescription className="text-xs">
            Ask rsync.ai sends a model names, types and your question, never a value from your data. Sample rows are
            turned off on this install.
          </CardDescription>
        </CardHeader>
      </Card>
    )
  }

  const on = data?.mode === "sample_rows"
  const approved = data?.approved_models ?? []

  return (
    <Card>
      <CardHeader>
        {title}
        {install === "on" && (
          <CardDescription className="text-xs">
            By default Ask rsync.ai sends a model names, types and your question, never a value from your data. With
            sample rows on, it may read up to 5 rows of a table to see what values look like, with columns marked
            personal or secret withheld and every value checked for personal data before it is sent. Each read is
            audited.
          </CardDescription>
        )}
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {state !== "failed" && (state === "loading" || install === "loading") && (
          <p className="flex items-center gap-2 text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" /> Loading…
          </p>
        )}
        {state === "failed" && (
          <p className="text-red-600 dark:text-red-400">Could not load this setting.</p>
        )}
        {state === "ready" && install === "on" && data && (
          <>
            {data.can_edit ? (
              <div className="flex items-center gap-2">
                <Switch
                  id="workspace-sample-rows"
                  checked={on}
                  disabled={saving}
                  onCheckedChange={(checked) => (checked ? setConfirming(true) : void save(false))}
                />
                <Label htmlFor="workspace-sample-rows" className="font-normal">
                  Let Ask rsync.ai read sample rows
                </Label>
              </div>
            ) : (
              <p className="text-zinc-600 dark:text-zinc-300">
                {on ? "Sample rows are on." : "Ask rsync.ai reads names only."} Only the workspace owner can change this.
              </p>
            )}
            <p className="text-xs text-zinc-500 dark:text-zinc-400">
              {approved.length > 0
                ? `Models an admin allowed sample rows for: ${modelNames(approved)}. Other models get names only.`
                : "No model is allowed sample rows yet: a platform admin allows one in Admin > Settings > AI models. Until then nothing is sent, even with this on."}
            </p>
          </>
        )}
      </CardContent>

      <AlertDialog open={confirming} onOpenChange={(open) => !open && setConfirming(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Let Ask rsync.ai read sample rows?</AlertDialogTitle>
            <AlertDialogDescription>
              Anyone in this workspace who asks Ask rsync.ai a question may then have up to 5 rows of a table sent to{" "}
              {approved.length > 0 ? modelNames(approved) : "a model an admin allows"}, a third-party service. Columns
              whose name or a PII scan marks as personal or secret are withheld, and values are checked for personal
              data before they are sent, but a check can miss something. Turn this off at any time.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault()
                void save(true)
              }}
              disabled={saving}
            >
              {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Turn on
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  )
}
