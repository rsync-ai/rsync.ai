"use client"

/**
 * Model layers: the workspace's own labels for its models (Silver, Gold, Marts…),
 * in the order the Models page and Lineage show them. Everyone in the workspace
 * sees them; only owners and admins add, rename, reorder or delete one
 * (handlers/workspace_layers.go). A layer changes nothing about a run, and
 * deleting one leaves its models in no layer, with their SQL and schedule intact.
 */

import { useState } from "react"
import { ArrowDown, ArrowUp, Loader2, Pencil, Plus, Trash2 } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
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
import { LayerBadge } from "@/components/explorer/modelLayer"
import {
  createModelLayer,
  deleteModelLayer,
  updateModelLayer,
  type WorkspaceLayer,
} from "@/lib/api/modelLayers"
import { useWorkspaceLayers } from "@/lib/hooks/useWorkspaceLayers"

// The server's limits (workspace_layers.go), so the form stops typing where the
// server would refuse.
const NAME_MAX = 40
const DESCRIPTION_MAX = 200
const LAYER_MAX = 20

function models(n: number): string {
  return `${n} model${n === 1 ? "" : "s"}`
}

export function ModelLayersCard() {
  const { layers, canManage, error: loadError, reload } = useWorkspaceLayers()
  // The form open: "new" to add a layer, or the id of the layer being edited.
  const [editing, setEditing] = useState<string>("")
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [rowBusy, setRowBusy] = useState("")
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null)
  const [deleting, setDeleting] = useState<WorkspaceLayer | null>(null)

  const openForm = (layer?: WorkspaceLayer) => {
    setEditing(layer ? layer.id : "new")
    setName(layer?.name ?? "")
    setDescription(layer?.description ?? "")
    setFormError(null)
    setMessage(null)
  }

  const closeForm = () => {
    setEditing("")
    setName("")
    setDescription("")
    setFormError(null)
  }

  const save = async () => {
    if (!editing || saving) return
    setSaving(true)
    setFormError(null)
    try {
      if (editing === "new") await createModelLayer({ name, description })
      else await updateModelLayer(editing, { name, description })
      closeForm()
      await reload()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "Could not save the layer")
    } finally {
      setSaving(false)
    }
  }

  const move = async (layer: WorkspaceLayer, by: -1 | 1) => {
    setRowBusy(layer.id)
    setMessage(null)
    try {
      await updateModelLayer(layer.id, { position: layer.position + by })
      await reload()
    } catch (err) {
      setMessage({ ok: false, text: err instanceof Error ? err.message : "Could not move the layer" })
    } finally {
      setRowBusy("")
    }
  }

  const confirmDelete = async () => {
    const layer = deleting
    if (!layer) return
    setRowBusy(layer.id)
    setMessage(null)
    try {
      const unlabelled = await deleteModelLayer(layer.id)
      setDeleting(null)
      setMessage({
        ok: true,
        text:
          unlabelled > 0
            ? `Deleted ${layer.name}. ${models(unlabelled)} now ${unlabelled === 1 ? "has" : "have"} no layer.`
            : `Deleted ${layer.name}.`,
      })
      await reload()
    } catch (err) {
      setDeleting(null)
      setMessage({ ok: false, text: err instanceof Error ? err.message : "Could not delete the layer" })
    } finally {
      setRowBusy("")
    }
  }

  const full = (layers?.length ?? 0) >= LAYER_MAX

  const form = (
    <form
      aria-label={editing === "new" ? "Add a layer" : "Edit the layer"}
      className="space-y-3 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800"
      onSubmit={(e) => {
        e.preventDefault()
        void save()
      }}
    >
      <div className="space-y-1">
        <Label htmlFor="layer-name">Name</Label>
        <Input
          id="layer-name"
          value={name}
          placeholder="Silver"
          maxLength={NAME_MAX}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      <div className="space-y-1">
        <Label htmlFor="layer-description">Description (optional)</Label>
        <Input
          id="layer-description"
          value={description}
          placeholder="Cleaned from one pipeline table: current rows, typed, deduplicated"
          maxLength={DESCRIPTION_MAX}
          onChange={(e) => setDescription(e.target.value)}
        />
        <p className="text-xs text-zinc-500 dark:text-zinc-400">Shown under the layer picker when someone saves a model.</p>
      </div>
      {formError && (
        <p role="alert" className="text-xs text-red-700 dark:text-red-300">
          {formError}
        </p>
      )}
      <div className="flex gap-2">
        <Button size="sm" type="submit" disabled={saving}>
          {saving && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
          {editing === "new" ? "Add layer" : "Save"}
        </Button>
        <Button size="sm" type="button" variant="ghost" onClick={closeForm}>
          Cancel
        </Button>
      </div>
    </form>
  )

  return (
    <Card>
      <CardHeader>
        <CardTitle>Model layers</CardTitle>
        <CardDescription>
          Labels that group this workspace&apos;s models on the Models page and in Lineage, in the order set here: for
          example Silver for models that clean one pipeline table, then Gold for models that join and aggregate them. A
          layer changes nothing about a run. The tables pipelines write are pipeline tables, not a layer.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loadError && (
          <p role="alert" className="text-sm text-red-700 dark:text-red-300">
            {loadError}
          </p>
        )}
        {layers === null && !loadError && (
          <div role="status" className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" /> Loading…
          </div>
        )}
        {layers !== null && layers.length === 0 && (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">No layers yet. Every model is in no layer.</p>
        )}
        {layers !== null && layers.length > 0 && (
          <ol aria-label="Model layers" className="divide-y divide-zinc-100 rounded-lg border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
            {layers.map((layer, i) =>
              editing === layer.id ? (
                <li key={layer.id} className="p-2">
                  {form}
                </li>
              ) : (
                <li key={layer.id} className="flex flex-wrap items-center gap-2 px-3 py-2">
                  <LayerBadge layer={layer.name} position={layer.position} />
                  <span className="text-xs text-zinc-500 dark:text-zinc-400">{models(layer.model_count)}</span>
                  {layer.description && (
                    <span className="min-w-0 basis-full text-xs text-zinc-600 dark:text-zinc-300 @md:basis-auto">
                      {layer.description}
                    </span>
                  )}
                  {canManage && (
                    <span className="ml-auto flex gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rowBusy !== "" || i === 0}
                        onClick={() => void move(layer, -1)}
                      >
                        <ArrowUp className="h-3.5 w-3.5" />
                        <span className="sr-only">Move {layer.name} up</span>
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rowBusy !== "" || i === layers.length - 1}
                        onClick={() => void move(layer, 1)}
                      >
                        <ArrowDown className="h-3.5 w-3.5" />
                        <span className="sr-only">Move {layer.name} down</span>
                      </Button>
                      <Button size="sm" variant="ghost" disabled={rowBusy !== ""} onClick={() => openForm(layer)}>
                        <Pencil className="h-3.5 w-3.5" />
                        <span className="sr-only">Edit {layer.name}</span>
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rowBusy !== ""}
                        onClick={() => {
                          setMessage(null)
                          setDeleting(layer)
                        }}
                      >
                        {rowBusy === layer.id ? (
                          <Loader2 className="h-3.5 w-3.5 animate-spin" />
                        ) : (
                          <Trash2 className="h-3.5 w-3.5" />
                        )}
                        <span className="sr-only">Delete {layer.name}</span>
                      </Button>
                    </span>
                  )}
                </li>
              ),
            )}
          </ol>
        )}

        {message && (
          <p
            role={message.ok ? "status" : "alert"}
            className={message.ok ? "text-xs text-emerald-700 dark:text-emerald-300" : "text-xs text-red-700 dark:text-red-300"}
          >
            {message.text}
          </p>
        )}

        {canManage && editing === "new" && form}
        {canManage && editing !== "new" && layers !== null && (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" disabled={full} onClick={() => openForm()}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Add layer
            </Button>
            {full && (
              <span className="text-xs text-zinc-500 dark:text-zinc-400">
                A workspace can have at most {LAYER_MAX} layers.
              </span>
            )}
          </div>
        )}
        {!canManage && layers !== null && (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Only workspace owners and admins add, rename, reorder or delete layers.
          </p>
        )}
      </CardContent>

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && rowBusy === "" && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete the layer {deleting?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleting && deleting.model_count > 0
                ? `Its ${models(deleting.model_count)} you can see will be in no layer. `
                : "No model you can see is in it. "}
              Models keep their SQL, their table and their schedule: only the label goes.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={rowBusy !== ""}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault()
                void confirmDelete()
              }}
              disabled={rowBusy !== ""}
            >
              {rowBusy !== "" && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
              Delete layer
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  )
}
