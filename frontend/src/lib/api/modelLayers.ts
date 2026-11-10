/**
 * The workspace's own model layers (Silver, Gold, Marts…), backed by
 * api-gateway handlers/workspace_layers.go. Anyone in the workspace lists them;
 * only an owner or admin adds, renames, moves or deletes one (can_manage says
 * which). A layer is a label on a model: it changes nothing about a run.
 */
import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

export interface WorkspaceLayer {
  id: string
  name: string
  description: string
  /** 0-based and dense: the order the workspace reads its layers in. */
  position: number
  /** Models in this layer that the caller can see. */
  model_count: number
}

/** Throws with the server's own `error` text, so a 409 says which name is taken. */
async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: unknown } | null
    throw new Error(typeof body?.error === "string" && body.error ? body.error : fallback)
  }
  return res.json() as Promise<T>
}

function send(url: string, method: "POST" | "PATCH", body: unknown): Promise<Response> {
  return authFetch(url, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
}

export async function listModelLayers(): Promise<{ layers: WorkspaceLayer[]; can_manage: boolean }> {
  const data = await readJSON<{ layers?: WorkspaceLayer[]; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.MODEL_LAYERS.LIST),
    "Could not load the model layers",
  )
  const layers = Array.isArray(data.layers) ? [...data.layers] : []
  layers.sort((a, b) => a.position - b.position)
  return { layers, can_manage: !!data.can_manage }
}

export async function createModelLayer(input: { name: string; description: string }): Promise<string> {
  const data = await readJSON<{ id?: string }>(
    await send(API_ENDPOINTS.MODEL_LAYERS.LIST, "POST", input),
    "Could not add the layer",
  )
  return data.id ?? ""
}

export async function updateModelLayer(
  id: string,
  patch: { name?: string; description?: string; position?: number },
): Promise<void> {
  await readJSON(await send(API_ENDPOINTS.MODEL_LAYERS.ITEM(id), "PATCH", patch), "Could not change the layer")
}

/** Returns how many models the delete left with no layer. */
export async function deleteModelLayer(id: string): Promise<number> {
  const data = await readJSON<{ models_unlabelled?: number }>(
    await authFetch(API_ENDPOINTS.MODEL_LAYERS.ITEM(id), { method: "DELETE" }),
    "Could not delete the layer",
  )
  return typeof data.models_unlabelled === "number" ? data.models_unlabelled : 0
}
