"use client"

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  Loader2,
  Sparkles,
  Shield,
  Zap,
  AlertTriangle,
  CheckCircle2,
  Eye,
  Info,
  ChevronDown,
  ChevronRight,
  Settings2,
  Plus,
  Trash2,
} from "lucide-react"
import { Skeleton } from "@/components/ui/skeleton"
import { toast } from "sonner"
import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { metadataErrorMessage } from "@/lib/api/connections"
import {
  generateSuggestions,
  type SuggestionsResponse,
  type TransformSuggestion,
} from "@/lib/api/suggestions"
import { describeTransform, TRANSFORM_TYPE_BLURBS, transformTypeLabel } from "@/lib/transform-display"
import { SUPPORTED_OPERATION_LIST, TRANSFORM_OPERATIONS } from "@/components/transforms/transformOperations"
import { TransformRuleFields, type TableColumns } from "@/components/transforms/TransformRuleFields"
import {
  isSupportedOperation,
  toEngineTransform,
  type TransformOperation,
} from "@/lib/pipeline/transformOps"
import {
  describeRuleTypeLimit,
  loadPipelineTransformPlan,
  operationRuns,
  ruleTypeRuns,
  savePipelineTransformPlan,
  stableStringify,
  type PlanRule,
} from "@/lib/pipeline/transformPlan"

export interface TableMetadata {
  name: string
  schema?: string
  columns: Array<{
    name: string
    type: string
    nullable?: boolean
    is_primary_key?: boolean
    is_foreign_key?: boolean
  }>
  row_count?: number
  primary_keys?: string[]
  foreign_keys?: Array<{
    column: string
    references_table: string
    references_column: string
  }>
  indexes?: Array<{
    name: string
    columns: string[]
    unique: boolean
  }>
}

interface SuggestionsReviewDialogProps {
  isOpen: boolean
  // Skip/close handler. May be async (it resumes the parked pipeline) — the dialog
  // awaits it so it can show a spinner and stay open on failure.
  onClose: () => void | Promise<void>
  pipelineId: string
  sourceConnectionId: string
  selectedTables: string[]
  // Resumes the parked pipeline. The plan is already saved by then: the dialog
  // writes it to transform_definitions itself, and that table is the only copy the
  // run reads, so the resume carries just how many rules this step added.
  onApplyContinue: (saved: { count: number }) => Promise<void>
  // If the parent already fetched schema (pre-fetch on table selection), pass it here
  // to skip the schema fetch step and jump straight to LLM suggestions.
  prefetchedSchema?: TableMetadata[]
  // The destination connector type (gcs, postgresql, ...). The service gives
  // index advice only for a destination it knows has indexes.
  destinationType?: string
}

// ---------------------------------------------------------------------------
// Type-specific transform editor cards (P4). Each receives the *effective*
// config (suggestion config merged with any prior user edits) and reports
// changes back via onChange, which the parent merges into editedConfigs[idx].
// Unknown types fall through to a read-only JSON view.
// ---------------------------------------------------------------------------

type ConfigEditor = {
  config: Record<string, unknown>
  onChange: (patch: Record<string, unknown>) => void
}

function RenameColumnsCard({ config, onChange }: ConfigEditor) {
  const mappings = (config.mappings && typeof config.mappings === "object"
    ? (config.mappings as Record<string, string>)
    : {})
  const entries = Object.entries(mappings)
  if (entries.length === 0) {
    return <div className="text-xs text-muted-foreground mt-2">No column mappings.</div>
  }
  return (
    <div className="space-y-2 mt-2">
      {entries.map(([from, to]) => (
        <div key={from} className="flex items-center gap-2">
          <span className="text-xs font-mono text-muted-foreground truncate max-w-[40%]" title={from}>
            {from}
          </span>
          <span className="text-xs text-muted-foreground">→</span>
          <Input
            className="h-8 text-xs flex-1"
            value={to}
            onChange={(e) =>
              onChange({ mappings: { ...mappings, [from]: e.target.value } })
            }
          />
        </div>
      ))}
    </div>
  )
}

function JSONFlattenCard({ config, onChange }: ConfigEditor) {
  const column = String(config.column ?? "")
  const prefix = String(config.prefix ?? "")
  const maxDepth = config.max_depth === undefined ? 2 : Number(config.max_depth)
  return (
    <div className="space-y-2 mt-2">
      <div className="text-xs">
        <span className="text-muted-foreground">Column: </span>
        <span className="font-mono">{column}</span>
      </div>
      <div className="flex items-center gap-2">
        <Label className="text-xs w-20 shrink-0">Prefix</Label>
        <Input
          className="h-8 text-xs flex-1"
          value={prefix}
          onChange={(e) => onChange({ prefix: e.target.value })}
        />
      </div>
      <div className="flex items-center gap-2">
        <Label className="text-xs w-20 shrink-0">Max depth</Label>
        <select
          className="h-8 rounded-md border border-input bg-background px-2 text-xs"
          value={String(maxDepth)}
          onChange={(e) => onChange({ max_depth: Number(e.target.value) })}
        >
          <option value="1">1</option>
          <option value="2">2</option>
          <option value="3">3</option>
          <option value="0">All</option>
        </select>
      </div>
    </div>
  )
}

function ArrayExpandCard({ config, onChange }: ConfigEditor) {
  const column = String(config.column ?? "")
  const outputPrefix = String(config.output_prefix ?? "")
  const maxElements = config.max_elements === undefined ? 10 : Number(config.max_elements)
  return (
    <div className="space-y-2 mt-2">
      <div className="text-xs">
        <span className="text-muted-foreground">Column: </span>
        <span className="font-mono">{column}</span>
      </div>
      <div className="flex items-center gap-2">
        <Label className="text-xs w-24 shrink-0">Output prefix</Label>
        <Input
          className="h-8 text-xs flex-1"
          value={outputPrefix}
          onChange={(e) => onChange({ output_prefix: e.target.value })}
        />
      </div>
      <div className="flex items-center gap-2">
        <Label className="text-xs w-24 shrink-0">Max elements</Label>
        <Input
          type="number"
          min={1}
          max={100}
          className="h-8 text-xs w-24"
          value={Number.isFinite(maxElements) ? maxElements : 10}
          onChange={(e) => {
            const n = parseInt(e.target.value, 10)
            onChange({ max_elements: Number.isFinite(n) ? Math.min(100, Math.max(1, n)) : 10 })
          }}
        />
      </div>
    </div>
  )
}

function TransformConfigEditor({
  transform,
  config,
  onChange,
}: ConfigEditor & { transform: TransformSuggestion }) {
  switch (transform.type) {
    case "rename_columns":
      return <RenameColumnsCard config={config} onChange={onChange} />
    case "json_flatten":
      return <JSONFlattenCard config={config} onChange={onChange} />
    case "array_expand":
      return <ArrayExpandCard config={config} onChange={onChange} />
    default:
      return (
        <div className="text-xs font-mono bg-zinc-100 dark:bg-zinc-800 p-2 rounded mt-2 whitespace-pre-wrap">
          {JSON.stringify(config, null, 2)}
        </div>
      )
  }
}

// TRANSFORM_TYPE_LABELS / transformTypeLabel / TRANSFORM_TYPE_BLURBS now live in
// @/lib/transform-display so the post-creation Transforms tab renders identically.

// The single column name a transform targets, for the compact row label.
function transformColumn(t: TransformSuggestion): string {
  const c = (t.config as Record<string, unknown> | undefined)?.column
  return typeof c === "string" && c ? c : t.summary || t.type
}

/**
 * A plan rule in the engine's vocabulary, for display, preview and duplicate
 * checks. A rule added here or in the Transforms tab's editor is stored under the
 * builder's name (`mask`, `rename`) and an AI suggestion under the engine's
 * (`mask_pii`, `rename_columns`); both normalise to the same engine rule
 * (shared/go/transforms/validate.go normalizeType), so they are compared here.
 */
function engineView(rule: PlanRule): { type: string; config: Record<string, unknown> } {
  const op = rule.operation
  if (isSupportedOperation(op)) {
    // Forced enabled: toEngineTransform drops a disabled rule, and a disabled saved
    // rule still has to be shown and compared.
    const t = toEngineTransform({ ...rule, operation: op, enabled: true })
    if (t) return { type: String(t.type), config: (t.config || {}) as Record<string, unknown> }
  }
  return { type: op, config: rule.config || {} }
}

/** Two rules with the same key do the same thing; saving both would apply it twice. */
function planKey(rule: PlanRule): string {
  const v = engineView(rule)
  return `${rule.enabled ? 1 : 0}|${v.type}|${stableStringify(v.config)}`
}

/**
 * The operator's choices over the suggestions, as one comparable value: equal to
 * the value taken when the suggestions loaded means nothing was changed. Ticking a
 * box and ticking it back is no change.
 */
function selectionKey(
  enabledPII: Record<string, boolean>,
  piiActions: Record<string, string>,
  piiHashFunctions: Record<string, string>,
  enabledTransforms: Record<number, boolean>,
  editedConfigs: Record<number, Record<string, unknown>>
): string {
  return stableStringify({ enabledPII, piiActions, piiHashFunctions, enabledTransforms, editedConfigs })
}

// A rule added in this step only has to name what the engine needs to run it. The
// one exception: "Drop Row" needs no default value.
function missingRequiredField(op: TransformOperation, config: Record<string, unknown>): string | null {
  for (const field of TRANSFORM_OPERATIONS[op].fields) {
    if (!field.required) continue
    if (op === "null_handle" && field.name === "default_value" && config.action === "skip") continue
    const v = config[field.name]
    if (v === undefined || v === null || String(v).trim() === "") return field.label
  }
  return null
}

/**
 * The selected tables and their columns, named the way the operator picked them.
 * That name becomes a rule's `config.table`, and the engine matches it on either
 * the qualified or the bare table name (validate.go tableMatches), so it holds on
 * both the batch path and the CDC path.
 */
function tablesWithColumns(schema: TableMetadata[], selectedTables: string[]): TableColumns[] {
  const all = schema.map((t) => {
    const qualified = t.schema ? `${t.schema}.${t.name}` : t.name
    const picked = selectedTables.find(
      (s) => s.toLowerCase() === qualified.toLowerCase() || s.toLowerCase() === t.name.toLowerCase()
    )
    return {
      picked: Boolean(picked),
      table: picked || qualified,
      columns: (t.columns || []).map((c) => c.name).filter(Boolean),
    }
  })
  // A schema the parent pre-fetched can hold more than the picked tables; offer
  // only those. If none match by name, the schema is all there is to go on.
  const chosen = all.filter((t) => t.picked)
  return (chosen.length > 0 ? chosen : all).map(({ table, columns }) => ({ table, columns }))
}

export function SuggestionsReviewDialog({
  isOpen,
  onClose,
  pipelineId,
  sourceConnectionId,
  selectedTables,
  onApplyContinue,
  prefetchedSchema,
  destinationType,
}: SuggestionsReviewDialogProps) {
  const [loading, setLoading] = useState(false)
  const [loadStep, setLoadStep] = useState<"schema" | "suggestions">("schema")
  const [retryCount, setRetryCount] = useState(0)
  const [error, setError] = useState<string | null>(null)
  // A failed save or resume. Kept apart from `error`, which is the suggestions
  // load failing and replaces the whole dialog body: this one must leave the
  // operator's plan on screen so they can retry it.
  const [actionError, setActionError] = useState<string | null>(null)
  const [suggestions, setSuggestions] = useState<SuggestionsResponse | null>(null)
  const [schema, setSchema] = useState<TableMetadata[]>(prefetchedSchema ?? [])
  
  // Transform toggles and edits
  const [enabledPII, setEnabledPII] = useState<Record<string, boolean>>({})
  const [enabledTransforms, setEnabledTransforms] = useState<Record<number, boolean>>({})
  const [piiActions, setPiiActions] = useState<Record<string, string>>({}) // column -> action
  // Per-transform user edits (keyed by suggestion index) and per-PII-column hash
  // algorithm selection, merged into the transform config at build time (P4).
  const [editedConfigs, setEditedConfigs] = useState<Record<number, Record<string, unknown>>>({})
  const [piiHashFunctions, setPiiHashFunctions] = useState<Record<string, string>>({}) // column -> hash_function
  
  // Preview state
  const [previewOpen, setPreviewOpen] = useState(false)
  const [previewTable, setPreviewTable] = useState<string>("")
  const [previewData, setPreviewData] = useState<{
    original: any[]
    transformed: any[]
    stats: any
    warnings: string[]
  } | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  
  const [submitting, setSubmitting] = useState(false)
  // Skipping/closing triggers an async pipeline-resume in the parent that takes a
  // few seconds before it unmounts this dialog. Track it so the Skip button shows
  // an immediate spinner + disables the actions — otherwise the click looks dead.
  const [closing, setClosing] = useState(false)
  // Apply and Skip both resume the SAME parked node. Keeping Skip clickable during
  // Apply (the escape hatch) opens a double-submit window, so this ref claims the
  // single resume: whichever fires first sets it, and the other aborts — preventing
  // two conflicting resume signals (transforms vs. skip) racing to the backend.
  const committedRef = useRef(false)
  // The suggestion choices as they loaded, so a close can tell whether the
  // operator changed any (selectionKey). null until the suggestions load.
  const [loadedSelection, setLoadedSelection] = useState<string | null>(null)
  // Escape, the backdrop or X over unsaved work: ask before it is thrown away.
  const [confirmDiscard, setConfirmDiscard] = useState(false)
  const keepEditingRef = useRef<HTMLButtonElement>(null)
  const discardQuestionId = useId()

  // What the pipeline already has. Saving is a REPLACE (transformPlan.ts), so the
  // dialog must know these before it writes anything: a table-selection step on a
  // pipeline that already has transforms (a re-discovery) would otherwise delete
  // them. null = not loaded yet, or the load failed.
  const [existingRules, setExistingRules] = useState<PlanRule[] | null>(null)
  const [existingError, setExistingError] = useState<string | null>(null)
  const [existingAttempt, setExistingAttempt] = useState(0)
  // The only rule types the pipeline can run (consumer_rule_types, sent for a
  // MongoDB CDC pipeline); null = any type can. Only those are offered: the save
  // refuses any other, and every other rule it was sent with.
  const [ruleTypeLimit, setRuleTypeLimit] = useState<string[] | null>(null)

  // Rules the operator adds in this step, with the Transforms tab's rule fields.
  const [customRules, setCustomRules] = useState<PlanRule[]>([])
  const [draftOp, setDraftOp] = useState<TransformOperation | "">("")
  const [draftConfig, setDraftConfig] = useState<Record<string, any>>({})
  const [draftError, setDraftError] = useState<string | null>(null)
  const [editingCustom, setEditingCustom] = useState<string | null>(null)

  // Transform-tab view state. Cards default to COLLAPSED so a long list (e.g.
  // 37 json_flatten suggestions) reads as a few grouped rows, not a wall of
  // config editors. expandedRows tracks which individual configs the user has
  // opened; collapsedGroups tracks which type-groups are folded shut.
  const [expandedRows, setExpandedRows] = useState<Record<number, boolean>>({})
  const [collapsedGroups, setCollapsedGroups] = useState<Record<string, boolean>>({})

  // Group transforms by type while preserving each suggestion's ORIGINAL index
  // (enabledTransforms/editedConfigs are keyed by that index, so it must survive
  // grouping). Order of first appearance is kept for stable rendering.
  const transformGroups = useMemo(() => {
    const order: string[] = []
    const byType: Record<string, Array<{ transform: TransformSuggestion; idx: number }>> = {}
    const list = suggestions?.transforms || []
    for (let i = 0; i < list.length; i++) {
      const t = list[i]
      if (!ruleTypeRuns(ruleTypeLimit, t.type || "")) continue
      const key = t.type || "other"
      if (!byType[key]) {
        byType[key] = []
        order.push(key)
      }
      byType[key].push({ transform: t, idx: i })
    }
    return order.map((type) => ({ type, items: byType[type] }))
  }, [suggestions, ruleTypeLimit])

  // The suggestions this pipeline cannot run: not listed, not saved, and named.
  const leftOutTransforms = useMemo(
    () => (suggestions?.transforms || []).filter((t) => !ruleTypeRuns(ruleTypeLimit, t.type || "")),
    [suggestions, ruleTypeLimit]
  )
  const offeredTransformCount = (suggestions?.transforms?.length || 0) - leftOutTransforms.length
  const ruleTypeNote = ruleTypeLimit
    ? describeRuleTypeLimit(ruleTypeLimit, {
        count: leftOutTransforms.length,
        noun: "suggestion",
        types: [...new Set(leftOutTransforms.map((t) => t.type || "other"))],
      })
    : null

  function normalizeValue(v: unknown): string {
    if (v == null) return ""
    if (typeof v === "string") return v
    if (typeof v === "number" || typeof v === "boolean") return String(v)
    try {
      return JSON.stringify(v)
    } catch {
      return String(v)
    }
  }

  function inferColumnsForPreview(table: string, original: any[], transformed: any[]): string[] {
    // Prefer schema-defined column ordering if available.
    const lower = String(table || "").toLowerCase()
    const schemaMatch =
      schema.find((t) => `${t.schema ? `${t.schema}.` : ""}${t.name}`.toLowerCase() === lower) ||
      schema.find((t) => String(t.name || "").toLowerCase() === lower) ||
      null
    const colsFromSchema = schemaMatch?.columns?.map((c) => c.name).filter(Boolean) || []

    const keys = new Set<string>()
    for (const c of colsFromSchema) keys.add(c)
    const addKeys = (rows: any[]) => {
      for (const r of rows || []) {
        if (!r || typeof r !== "object") continue
        for (const k of Object.keys(r)) keys.add(k)
      }
    }
    addKeys((original || []).slice(0, 10))
    addKeys((transformed || []).slice(0, 10))

    return Array.from(keys)
  }

  function PreviewGrid(props: { title: string; columns: string[]; rows: any[]; compareTo?: any[] }) {
    const { title, columns, rows, compareTo } = props
    const limited = (rows || []).slice(0, 10)
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">{title}</CardTitle>
        </CardHeader>
        <CardContent>
          {limited.length === 0 ? (
            <div className="text-sm text-zinc-600 dark:text-zinc-400">No rows to display</div>
          ) : (
            <div className="overflow-auto max-h-96 border border-zinc-200 dark:border-zinc-800 rounded">
              <table className="min-w-full text-xs">
                <thead className="sticky top-0 bg-white dark:bg-zinc-950">
                  <tr>
                    {columns.map((c) => (
                      <th
                        key={c}
                        className="text-left font-medium px-3 py-2 border-b border-zinc-200 dark:border-zinc-800 whitespace-nowrap"
                      >
                        {c}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {limited.map((row, idx) => {
                    const base = compareTo && Array.isArray(compareTo) ? compareTo[idx] : undefined
                    return (
                      <tr key={idx} className="border-b border-zinc-100 dark:border-zinc-900">
                        {columns.map((c) => {
                          const v = row?.[c]
                          const bv = base?.[c]
                          const changed = base !== undefined && normalizeValue(v) !== normalizeValue(bv)
                          return (
                            <td
                              key={c}
                              className={`px-3 py-2 align-top whitespace-nowrap ${
                                changed ? "bg-amber-50 dark:bg-amber-950/20" : ""
                              }`}
                            >
                              <span className="font-mono">{normalizeValue(v)}</span>
                            </td>
                          )
                        })}
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    )
  }

  // Fetch schema and generate suggestions when dialog opens (or retryCount increments).
  // retryCount in deps means clicking Retry re-runs this effect without closing/reopening.
  useEffect(() => {
    if (!isOpen) {
      setSuggestions(null)
      setSchema(prefetchedSchema ?? [])
      setError(null)
      setEnabledPII({})
      setEnabledTransforms({})
      setPiiActions({})
      setEditedConfigs({})
      setPiiHashFunctions({})
      setPreviewOpen(false)
      setPreviewData(null)
      setClosing(false)
      setActionError(null)
      setExistingRules(null)
      setExistingError(null)
      setRuleTypeLimit(null)
      setCustomRules([])
      setDraftOp("")
      setDraftConfig({})
      setDraftError(null)
      setEditingCustom(null)
      setLoadedSelection(null)
      setConfirmDiscard(false)
      committedRef.current = false
      return
    }

    const fetchAndGenerate = async () => {
      setLoading(true)
      setError(null)

      try {
        // Step 1: schema — skip if parent already fetched it.
        let tables: TableMetadata[] = prefetchedSchema ?? schema
        if (tables.length === 0) {
          setLoadStep("schema")
          const tablesParam = selectedTables.join(",")
          const schemaRes = await authFetch(
            `${API_ENDPOINTS.CONNECTIONS.GET(sourceConnectionId)}/metadata?tables=${encodeURIComponent(
              tablesParam
            )}&include_columns=true`,
            { cache: "no-store" }
          )
          // "Schema discovery failed" names nothing; `details` carries the
          // connector's own reason (a DNS or auth error).
          if (!schemaRes.ok) throw new Error(await metadataErrorMessage(schemaRes))
          const schemaData = await schemaRes.json()
          tables = schemaData.tables || []
          setSchema(tables)
        }

        if (tables.length === 0) {
          throw new Error("No schema information available for selected tables")
        }

        // Step 2: AI suggestions
        setLoadStep("suggestions")
        const suggestionsData = await generateSuggestions({
          schema: {
            tables: tables.map((t) => ({
              name: t.name,
              columns: t.columns.map((c) => ({
                name: c.name,
                type: c.type,
                nullable: c.nullable,
                description: "",
              })),
            })),
          },
          intent: {
            use_case: "sync",
            description: "Data pipeline sync",
            source_type: "database",
            // Not in the deps below: a late value must not re-run the model call.
            // An unknown destination gets no index advice, which is the safe side.
            destination_type: destinationType || "unknown",
          },
        })

        setSuggestions(suggestionsData)

        const piiEnabled: Record<string, boolean> = {}
        const piiActs: Record<string, string> = {}
        for (const pii of suggestionsData.pii_columns || []) {
          piiEnabled[pii.column] = true
          piiActs[pii.column] = pii.suggested_action || "mask"
        }
        setEnabledPII(piiEnabled)
        setPiiActions(piiActs)

        const transformEnabled: Record<number, boolean> = {}
        for (let i = 0; i < (suggestionsData.transforms || []).length; i++) {
          transformEnabled[i] = true
        }
        setEnabledTransforms(transformEnabled)
        setLoadedSelection(selectionKey(piiEnabled, piiActs, {}, transformEnabled, {}))
      } catch (err: any) {
        setError(err?.message || "Failed to generate suggestions")
      } finally {
        setLoading(false)
      }
    }

    void fetchAndGenerate()
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isOpen, sourceConnectionId, selectedTables, retryCount])

  // Load the pipeline's saved transforms, which the save below has to keep.
  useEffect(() => {
    if (!isOpen) return
    if (!pipelineId) {
      setExistingRules([])
      return
    }
    let cancelled = false
    setExistingError(null)
    void loadPipelineTransformPlan(pipelineId).then((res) => {
      if (cancelled) return
      if (res.ok) {
        setExistingRules(res.data)
        setRuleTypeLimit(res.consumerRuleTypes)
      } else setExistingError(res.error)
    })
    return () => {
      cancelled = true
    }
  }, [isOpen, pipelineId, existingAttempt])

  const tableColumns = useMemo(() => tablesWithColumns(schema, selectedTables), [schema, selectedTables])

  const buildActiveTransforms = useCallback((): TransformSuggestion[] => {
    const transforms: TransformSuggestion[] = []

    if (!suggestions) return transforms

    // PII transforms
    for (const pii of suggestions.pii_columns || []) {
      if (enabledPII[pii.column]) {
        const action = piiActions[pii.column] || pii.suggested_action || "mask"
        // "none" means user explicitly chose to keep the field as-is — skip transform
        if (action === "none") continue
        const maskType = action === "mask" ? "hash" : action
        const config: Record<string, unknown> = {
          column: pii.column,
          mask_type: maskType,
          pii_type: pii.pii_types?.join(",") || "unknown",
        }
        // Only carry a hash_function when the engine will actually hash. For
        // redact/mask the value is replaced, so the algorithm is irrelevant.
        if (maskType === "hash") {
          config.hash_function = piiHashFunctions[pii.column] || "sha256"
        }
        transforms.push({
          type: "mask_pii",
          config,
          summary: `${action.charAt(0).toUpperCase() + action.slice(1)} ${pii.column}`,
          reason: `PII detected: ${pii.pii_types?.join(", ")}`,
        })
      }
    }

    // Other transforms — merge any per-card user edits over the suggested config.
    // Default to enabled (?? true) to match the render-side default at the group
    // checkbox and per-row toggle, so a never-touched suggestion that shows as
    // CHECKED is also persisted (not silently dropped).
    for (let i = 0; i < (suggestions.transforms || []).length; i++) {
      if ((enabledTransforms[i] ?? true) && ruleTypeRuns(ruleTypeLimit, suggestions.transforms[i].type || "")) {
        const base = suggestions.transforms[i]
        const edits = editedConfigs[i]
        if (edits && Object.keys(edits).length > 0) {
          transforms.push({ ...base, config: { ...base.config, ...edits } })
        } else {
          transforms.push(base)
        }
      }
    }

    return transforms
  }, [suggestions, enabledPII, enabledTransforms, piiActions, editedConfigs, piiHashFunctions, ruleTypeLimit])

  /**
   * The plan this step saves: the pipeline's saved rules first, unchanged, then
   * the checked suggestions, then the rules added here. Every new rule runs on
   * both the batch and the CDC path ("both"), because a CDC pipeline's initial
   * load can take either. A rule already in the plan is not added twice — a
   * second mask would hash the hash.
   */
  const planRules = useMemo((): PlanRule[] => {
    const seen = new Set<string>()
    const out: PlanRule[] = []
    const push = (r: PlanRule) => {
      const k = planKey(r)
      if (seen.has(k)) return
      seen.add(k)
      out.push({ ...r, order: out.length })
    }
    // Saved rules go in exactly as stored — even a duplicate pair — so nothing
    // already saved is dropped by a re-save, and addedCount stays exact.
    for (const r of existingRules ?? []) {
      seen.add(planKey(r))
      out.push({ ...r, order: out.length })
    }
    for (const t of buildActiveTransforms()) {
      push({ id: "", order: 0, type: "both", operation: t.type, enabled: true, config: { ...(t.config || {}) } })
    }
    for (const r of customRules) push(r)
    return out
  }, [existingRules, buildActiveTransforms, customRules])

  const existingCount = existingRules?.length ?? 0
  const addedCount = planRules.length - existingCount

  const addCustomRule = () => {
    if (!draftOp) return
    const missing = missingRequiredField(draftOp, draftConfig)
    if (missing) {
      setDraftError(`${missing} is required.`)
      return
    }
    const rule: PlanRule = {
      id: crypto.randomUUID(),
      order: 0,
      type: "both",
      operation: draftOp,
      enabled: true,
      config: { ...draftConfig },
      description: TRANSFORM_OPERATIONS[draftOp].description,
    }
    if (planRules.some((r) => planKey(r) === planKey(rule))) {
      setDraftError("Already in the plan.")
      return
    }
    setCustomRules((prev) => [...prev, rule])
    setDraftOp("")
    setDraftConfig({})
    setDraftError(null)
  }

  const handlePreview = useCallback(
    async (table: string) => {
      if (!sourceConnectionId || !table) return

      // The whole plan as the run will apply it, saved rules included.
      const activeTransforms = planRules.filter((r) => r.enabled).map(engineView)
      if (activeTransforms.length === 0) {
        // Nothing meaningful to show — tell user to enable at least one transform.
        setPreviewTable(table)
        setPreviewOpen(true)
        setPreviewData(null)
        setPreviewLoading(false)
        return
      }

      setPreviewLoading(true)
      setPreviewTable(table)
      setPreviewOpen(true)
      setPreviewData(null)

      try {
        // Single call: backend samples the table and applies transforms server-side.
        const previewRes = await authFetch(`${API_ENDPOINTS.API_GATEWAY_URL}/api/v1/transforms/preview`, {
          method: "POST",
          body: JSON.stringify({
            transforms: activeTransforms,
            source_connection_id: sourceConnectionId,
            table,
            limit: 10,
          }),
        })

        if (!previewRes.ok) {
          throw new Error(`Preview failed (${previewRes.status})`)
        }

        const previewResult = await previewRes.json()
        const original: any[] = previewResult.original_rows || previewResult.sample_data || []
        const transformed: any[] = previewResult.preview || []
        const applied = Number(previewResult.transforms_applied || 0)
        const warnings: string[] = Array.isArray(previewResult.warnings) ? previewResult.warnings : []
        if (applied === 0 && activeTransforms.length > 0) {
          warnings.unshift("Transforms selected but none applied — check column names match your schema.")
        }
        setPreviewData({
          original,
          transformed,
          stats: {
            rows_before: original.length,
            rows_after: transformed.length,
            columns_before: Object.keys(original[0] || {}).length,
            columns_after: Object.keys(transformed[0] || {}).length,
          },
          warnings,
        })
      } catch (err: any) {
        setPreviewData({
          original: [],
          transformed: [],
          stats: { rows_before: 0, rows_after: 0, columns_before: 0, columns_after: 0 },
          warnings: [err?.message || "Preview failed"],
        })
      } finally {
        setPreviewLoading(false)
      }
    },
    [sourceConnectionId, planRules]
  )

  const handleApply = useCallback(async () => {
    // A resume (apply or a raced-in skip) already owns this node — don't start.
    if (committedRef.current) return
    setSubmitting(true)
    setActionError(null)

    try {
      // Nothing new means nothing to write: the saved rules are already stored,
      // and re-writing them would only re-mint their ids.
      if (addedCount > 0) {
        // A rule edited after "Add to plan" can have lost a required field.
        for (const r of customRules) {
          const missing = missingRequiredField(r.operation as TransformOperation, r.config)
          if (missing) {
            throw new Error(
              `Transforms not saved: ${TRANSFORM_OPERATIONS[r.operation as TransformOperation].name} needs ${missing}.`
            )
          }
        }
        if (!pipelineId) throw new Error("Transforms not saved: this step has no pipeline to save them to.")
        // The button is disabled until the saved rules are known; this guards a
        // click that lands in between. A save without them would delete them.
        if (existingRules === null) throw new Error("Transforms not saved: the pipeline's saved transforms have not loaded.")
        // The shared save the Transforms tab's editor uses: one producer row and one
        // consumer row per rule, bounded by a timeout.
        const saved = await savePipelineTransformPlan(pipelineId, planRules)
        if (!saved.ok) throw new Error(`Transforms not saved: ${saved.error}`)
      }

      // If a Skip raced in while the save POST was in flight, it has already claimed
      // the single resume — abort so we don't fire a second, conflicting signal.
      if (committedRef.current) return
      committedRef.current = true

      // onApplyContinue already closes this dialog (unmounts it) once the resume
      // signal is sent, so we do NOT also call onClose() here — onClose is the SKIP
      // handler, and calling it fired a spurious second (skip) resume on top of the
      // apply.
      await onApplyContinue({ count: addedCount })

      // Only after the resume went through (a failed one reports in the dialog),
      // and with its own duration so it doesn't outlive the step it describes.
      // The CDC sink caches consumer transforms for up to 30s, so changes to a
      // pipeline that is already running propagate within that window.
      if (addedCount > 0) {
        toast.success("Transforms saved. Changes propagate to running pipelines within 30 seconds.", {
          duration: 5000,
        })
      }
    } catch (err: any) {
      // Release the claim so the user can retry from the still-open modal.
      committedRef.current = false
      setActionError(err?.message || "Failed to apply transforms")
    } finally {
      setSubmitting(false)
    }
  }, [addedCount, customRules, existingRules, onApplyContinue, pipelineId, planRules])

  // The Skip resume: the Skip button, and "Discard and start" in the question
  // that Escape, the backdrop and X raise over unsaved work. Awaits the parent's async
  // resume so it can (a) show a "Skipping…" spinner while it runs, and (b) stay open
  // with the user's selection intact if the resume fails. `setSubmitting(false)` keeps
  // Skip a real escape hatch even if an Apply is mid-flight — the user must always be
  // able to bail out. Guards only against a repeated skip (`closing`), not `submitting`.
  const requestClose = useCallback(async () => {
    // Already skipping, or an Apply has already committed the resume (its signal is
    // in flight) — ignore, so Skip can't fire a second, conflicting resume.
    if (closing || committedRef.current) return
    committedRef.current = true
    setSubmitting(false)
    setActionError(null)
    setClosing(true)
    try {
      await onClose()
      // On success the parent unmounts this dialog; nothing else to do.
    } catch (err: any) {
      // Resume failed — release the claim, reopen the actions, and surface the error.
      committedRef.current = false
      setClosing(false)
      setActionError(err?.message || "Failed to skip suggestions")
    }
  }, [closing, onClose])

  // What Escape, the backdrop and X would throw away. Suggestions as they loaded
  // are not the operator's work, so closing over them stays one step.
  const hasUnsavedWork =
    !loading &&
    !error &&
    (customRules.length > 0 ||
      draftOp !== "" ||
      (loadedSelection !== null &&
        selectionKey(enabledPII, piiActions, piiHashFunctions, enabledTransforms, editedConfigs) !== loadedSelection))

  // Escape and X (an outside click never closes this dialog). Only the Skip
  // button skips without asking: these also fire by accident, and closing
  // starts the pipeline without the rules set up here
  // (KI-TRANSFORM-STEP-CLOSE-DISCARDS-RULES).
  const requestDismiss = useCallback(() => {
    if (closing || committedRef.current) return
    // A second Escape backs out of the question rather than answering it.
    if (confirmDiscard) {
      setConfirmDiscard(false)
      return
    }
    if (hasUnsavedWork) {
      setConfirmDiscard(true)
      return
    }
    void requestClose()
  }, [closing, confirmDiscard, hasUnsavedWork, requestClose])

  useEffect(() => {
    if (confirmDiscard) keepEditingRef.current?.focus()
  }, [confirmDiscard])

  // ---- Transform group helpers (operate on original indices) ----
  // Tri-state group checkbox: true if all enabled, false if none, "indeterminate" otherwise.
  const groupCheckState = (items: Array<{ idx: number }>): boolean | "indeterminate" => {
    const on = items.filter((it) => enabledTransforms[it.idx] ?? true).length
    if (on === 0) return false
    if (on === items.length) return true
    return "indeterminate"
  }

  const setGroupEnabled = (items: Array<{ idx: number }>, enabled: boolean) => {
    setEnabledTransforms((prev) => {
      const next = { ...prev }
      for (const it of items) next[it.idx] = enabled
      return next
    })
  }

  // Apply one max_depth to every json_flatten suggestion in a group at once.
  const setGroupMaxDepth = (items: Array<{ idx: number }>, depth: number) => {
    setEditedConfigs((prev) => {
      const next = { ...prev }
      for (const it of items) {
        next[it.idx] = { ...(next[it.idx] || {}), max_depth: depth }
      }
      return next
    })
  }

  // Saving with the saved rules unknown would delete them (a REPLACE), so a plan
  // that adds anything waits for them.
  const saveBlocked = addedCount > 0 && existingRules === null

  return (
    <>
      <Dialog open={isOpen} onOpenChange={(open) => { if (!open) requestDismiss() }} closeOnOutsideClick={false}>
        <DialogContent className="w-[95vw] max-w-4xl max-h-[90vh] overflow-hidden flex flex-col">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Sparkles className="h-5 w-5 text-violet-600" />
              Transforms
            </DialogTitle>
            <DialogDescription>
              Keep the AI suggestions you want and add your own rules. Every rule runs on the initial
              load and on every change after it. Optimizations are advice only.
            </DialogDescription>
          </DialogHeader>

          {loading ? (
            <div className="space-y-4 py-2">
              <div className="flex items-center gap-3 text-sm text-zinc-500 dark:text-zinc-400 mb-2">
                <Loader2 className="h-4 w-4 animate-spin text-violet-500 shrink-0" />
                {loadStep === "schema"
                  ? "Step 1/2 — Fetching table schema…"
                  : "Step 2/2 — Analyzing for PII and optimizations…"}
              </div>
              {/* Skeleton cards — one per expected suggestion type */}
              {[1, 2, 3].map((i) => (
                <div key={i} className="rounded-xl border border-zinc-100 dark:border-zinc-800 p-4 space-y-2">
                  <div className="flex items-center gap-3">
                    <Skeleton className="h-5 w-5 rounded" />
                    <Skeleton className="h-4 w-48" />
                    <Skeleton className="h-5 w-16 ml-auto rounded-full" />
                  </div>
                  <Skeleton className="h-3 w-full" />
                  <Skeleton className="h-3 w-3/4" />
                </div>
              ))}
            </div>
          ) : error ? (
            // Error case used to render only the red message — leaving the user
            // trapped because the footer buttons live in the success branch only.
            // Surface explicit Skip and Retry actions here so the dialog is
            // never an unrecoverable dead-end.
            <div className="space-y-3">
              <div className="p-6 border border-red-200 bg-red-50 dark:bg-red-900/20 dark:border-red-800 rounded-md">
                <div className="flex items-start gap-3">
                  <AlertTriangle className="h-5 w-5 text-red-600 shrink-0" />
                  <div className="flex-1">
                    <div className="font-medium text-red-900 dark:text-red-100">
                      Failed to generate suggestions
                    </div>
                    <div className="text-sm text-red-700 dark:text-red-300 mt-1">{error}</div>
                    <div className="text-xs text-red-700/80 dark:text-red-300/80 mt-2">
                      You can continue without AI-generated transforms — the pipeline will sync the
                      raw schema as-is. You can always add transforms later.
                    </div>
                  </div>
                </div>
              </div>
              <div className="flex items-center justify-end gap-2">
                <Button
                  variant="outline"
                  onClick={() => {
                    // Retry: reset state and re-fetch. Never blocked by
                    // a stuck `submitting` flag — that flag's only purpose
                    // is preventing double-submit on the primary action.
                    setError(null)
                    setSubmitting(false)
                    setSchema(prefetchedSchema ?? [])
                    setRetryCount((n) => n + 1)
                  }}
                >
                  Retry
                </Button>
                <Button
                  variant="outline"
                  onClick={async () => {
                    // Continue without transforms. Guard against double-
                    // click via the in-handler check, NOT the disabled
                    // prop — a hung onApplyContinue would otherwise leave
                    // the user with no escape (no clickable Cancel either).
                    if (submitting) return
                    setSubmitting(true)
                    try {
                      // onApplyContinue already dismisses this dialog. Do NOT also
                      // call onClose() — that's the SKIP handler and would fire a
                      // spurious second resume on top of the continue.
                      await onApplyContinue({ count: 0 })
                    } catch {
                      // Reset so the user can retry or cancel.
                      setSubmitting(false)
                    }
                  }}
                >
                  Continue without transforms
                </Button>
                <Button
                  variant="ghost"
                  onClick={() => {
                    // Cancel is an escape hatch — always clickable.
                    // Force-resetting `submitting` here means a hung
                    // primary-action call doesn't trap the user.
                    setSubmitting(false)
                    onClose()
                  }}
                >
                  Cancel
                </Button>
              </div>
            </div>
          ) : (
            <>
              <div className="flex items-center justify-between px-1 py-2">
                <div className="text-sm text-zinc-600 dark:text-zinc-400">
                  <span className="font-medium text-foreground">{planRules.length}</span>{" "}
                  {planRules.length === 1 ? "transform" : "transforms"} in the plan
                  {existingCount > 0 && (
                    <span className="text-muted-foreground"> ({existingCount} already saved)</span>
                  )}
                  {selectedTables.length > 0 && (
                    <span className="text-muted-foreground">
                      {" "}
                      to {selectedTables.length} table{selectedTables.length > 1 ? "s" : ""}
                    </span>
                  )}
                </div>
                {selectedTables.length > 0 && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => handlePreview(selectedTables[0])}
                  >
                    <Eye className="h-4 w-4 mr-2" />
                    Preview
                  </Button>
                )}
              </div>

              {/*
                Partial-success banner. The backend now distinguishes between
                a hard failure (no usable suggestions → caught by the error
                branch above) and a degraded response (e.g. LLM transform
                call timed out but PII/optimization heuristics still ran).
                For the degraded case we show an informational amber banner
                so the user understands why the Transforms tab might be
                empty and can choose to continue with the remaining
                suggestions instead of staring at a generic blank state.
              */}
              {suggestions?.degraded && suggestions?.error && (
                <div className="mx-1 mb-2 px-3 py-2 text-sm border border-amber-200 bg-amber-50 dark:bg-amber-900/20 dark:border-amber-800 rounded-md text-amber-900 dark:text-amber-100 flex items-start gap-2">
                  <Info className="h-4 w-4 mt-0.5 shrink-0 text-amber-600" />
                  <span>{suggestions.error}</span>
                </div>
              )}

              {/*
                Scroll model: the Tabs container is the `flex-1 min-h-0` child of
                the DialogContent flex column, so it's bounded by `max-h-[90vh]`.
                The TabsList is `shrink-0` (always pinned/visible), and ONLY the
                content sits in a native `overflow-y-auto` region. We use a plain
                overflow container instead of Radix ScrollArea here because the
                ScrollArea viewport did not reliably resolve its height inside the
                flex column, leaving long lists (PII / 40+ transforms) unscrollable.
                Native overflow with `flex-1 min-h-0` is bulletproof and keeps the
                tab headers and the Skip/Apply footer fixed while the list scrolls.
              */}
              <Tabs defaultValue="pii" className="flex-1 min-h-0 flex flex-col w-full">
                <TabsList className="grid w-full grid-cols-2 sm:grid-cols-5 h-auto shrink-0">
                    <TabsTrigger value="pii">
                      <Shield className="h-4 w-4 mr-2" />
                      PII ({suggestions?.pii_columns?.length || 0})
                    </TabsTrigger>
                    <TabsTrigger value="transforms">
                      <Zap className="h-4 w-4 mr-2" />
                      Transforms ({offeredTransformCount})
                    </TabsTrigger>
                    <TabsTrigger value="custom">
                      <Plus className="h-4 w-4 mr-2" />
                      Add a transform
                    </TabsTrigger>
                    <TabsTrigger value="optimizations">
                      <Sparkles className="h-4 w-4 mr-2" />
                      Optimizations ({suggestions?.optimizations?.length || 0})
                    </TabsTrigger>
                    <TabsTrigger value="plan">
                      <CheckCircle2 className="h-4 w-4 mr-2" />
                      Plan ({planRules.length})
                    </TabsTrigger>
                  </TabsList>

                <div className="flex-1 min-h-0 overflow-y-auto pr-3">
                  <TabsContent value="pii" className="space-y-3 mt-4">
                    {!suggestions?.pii_detected || (suggestions.pii_columns || []).length === 0 ? (
                      <Card className="bg-green-50 border-green-200 dark:bg-green-950/20 dark:border-green-800">
                        <CardContent className="pt-6">
                          <div className="flex items-center gap-3">
                            <CheckCircle2 className="h-5 w-5 text-green-600 dark:text-green-400" />
                            <div className="text-sm text-green-800 dark:text-green-200">
                              No PII detected in selected tables
                            </div>
                          </div>
                        </CardContent>
                      </Card>
                    ) : (
                      (suggestions.pii_columns || []).map((pii, idx) => (
                        <Card key={idx} className="border-amber-200 dark:border-amber-800">
                          <CardHeader className="pb-3">
                            <div className="flex items-start justify-between">
                              <div className="flex items-center gap-3">
                                <Checkbox
                                  checked={enabledPII[pii.column] ?? true}
                                  onCheckedChange={(checked) =>
                                    setEnabledPII((prev) => ({ ...prev, [pii.column]: !!checked }))
                                  }
                                />
                                <div>
                                  <CardTitle className="text-sm font-medium">
                                    {pii.column.includes(".") ? (
                                      <>
                                        <span className="text-muted-foreground font-normal">
                                          {pii.column.slice(0, pii.column.lastIndexOf("."))}.
                                        </span>
                                        <span className="font-semibold">
                                          {pii.column.slice(pii.column.lastIndexOf(".") + 1)}
                                        </span>
                                      </>
                                    ) : (
                                      pii.column
                                    )}
                                  </CardTitle>
                                  <div className="flex gap-2 mt-1">
                                    {(pii.pii_types || []).map((type, i) => (
                                      <Badge key={i} variant="secondary" className="text-xs">
                                        {type}
                                      </Badge>
                                    ))}
                                    <Badge
                                      variant={
                                        pii.confidence === "high"
                                          ? "default"
                                          : pii.confidence === "medium"
                                          ? "secondary"
                                          : "outline"
                                      }
                                      className="text-xs"
                                    >
                                      {pii.confidence} confidence
                                    </Badge>
                                  </div>
                                </div>
                              </div>
                              {enabledPII[pii.column] ? (
                                <div className="flex flex-col gap-2 items-end">
                                  <select
                                    className="h-10 w-36 rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2"
                                    value={piiActions[pii.column] || pii.suggested_action || "mask"}
                                    onChange={(e) =>
                                      setPiiActions((prev) => ({ ...prev, [pii.column]: e.target.value }))
                                    }
                                  >
                                    <option value="none">Keep original</option>
                                    <option value="mask">Mask</option>
                                    <option value="hash">Hash</option>
                                    <option value="redact">Redact</option>
                                    <option value="review">Review</option>
                                  </select>
                                  {/* Hash algorithm selector — only meaningful when the action
                                      hashes the value. "mask" maps to mask_type=hash at build time,
                                      so show the selector for both "mask" and "hash". */}
                                  {(() => {
                                    const act = piiActions[pii.column] || pii.suggested_action || "mask"
                                    return act === "hash" || act === "mask"
                                  })() && (
                                    <select
                                      className="h-9 w-36 rounded-md border border-input bg-background px-3 py-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2"
                                      value={piiHashFunctions[pii.column] || "sha256"}
                                      onChange={(e) =>
                                        setPiiHashFunctions((prev) => ({ ...prev, [pii.column]: e.target.value }))
                                      }
                                      title="Hash algorithm"
                                    >
                                      <option value="sha256">SHA-256 (recommended)</option>
                                      <option value="hmac_sha256" title="Needs RSYNC_PII_HASH_SALT on the server; without it the rule is refused">HMAC-SHA256 (needs server key)</option>
                                      <option value="md5">MD5 (legacy)</option>
                                    </select>
                                  )}
                                </div>
                              ) : (
                                <span className="text-xs text-muted-foreground italic px-2">Not applying</span>
                              )}
                            </div>
                          </CardHeader>
                        </Card>
                      ))
                    )}
                  </TabsContent>

                  <TabsContent value="transforms" className="space-y-3 mt-4">
                    {ruleTypeNote && (
                      <p className="text-xs text-amber-700 dark:text-amber-400">{ruleTypeNote}</p>
                    )}
                    {offeredTransformCount > 0 && (
                      <div className="flex items-start gap-2 rounded-md border border-teal-200 dark:border-teal-900/60 bg-teal-50 dark:bg-teal-950/30 px-3 py-2.5">
                        <Sparkles className="h-4 w-4 text-teal-600 dark:text-teal-400 mt-0.5 shrink-0" />
                        <p className="text-xs text-teal-800 dark:text-teal-200">
                          We auto-selected{" "}
                          <span className="font-medium">{offeredTransformCount} transforms</span>{" "}
                          from your schema. These are safe defaults — uncheck anything you don&apos;t want, then continue.
                        </p>
                      </div>
                    )}
                    {offeredTransformCount === 0 ? (
                      <Card>
                        <CardContent className="pt-6">
                          <div className="text-sm text-zinc-600 dark:text-zinc-400 text-center">
                            No additional transforms suggested
                          </div>
                        </CardContent>
                      </Card>
                    ) : (
                      // Grouped by transform type. Each group is a folder: a header
                      // (tri-state select-all + count + bulk controls) over a list of
                      // compact rows that expand to the per-column config on demand.
                      transformGroups.map((group) => {
                        const folded = collapsedGroups[group.type] ?? false
                        const checkState = groupCheckState(group.items)
                        const isFlatten = group.type === "json_flatten"
                        // Surface the dominant bulk depth (empty if items disagree).
                        const depths = new Set(
                          group.items.map((it) =>
                            String(
                              (editedConfigs[it.idx]?.max_depth ??
                                (it.transform.config as Record<string, unknown>)?.max_depth ??
                                2) as number
                            )
                          )
                        )
                        const bulkDepth = depths.size === 1 ? [...depths][0] : ""
                        return (
                          <Card key={group.type} className="overflow-hidden">
                            {/* Group header */}
                            <div className="flex items-center gap-3 px-3 py-2.5 bg-zinc-50 dark:bg-zinc-900/40 border-b border-zinc-100 dark:border-zinc-800">
                              <Checkbox
                                checked={checkState}
                                onCheckedChange={(checked) => setGroupEnabled(group.items, !!checked)}
                                aria-label={`Toggle all ${transformTypeLabel(group.type)}`}
                              />
                              <button
                                type="button"
                                onClick={() =>
                                  setCollapsedGroups((prev) => ({ ...prev, [group.type]: !folded }))
                                }
                                className="flex items-center gap-1.5 flex-1 min-w-0 text-left"
                              >
                                {folded ? (
                                  <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
                                ) : (
                                  <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
                                )}
                                <span className="text-sm font-medium truncate">
                                  {transformTypeLabel(group.type)}
                                </span>
                                <Badge variant="secondary" className="text-xs shrink-0">
                                  {group.items.length}
                                </Badge>
                              </button>
                              {/* Bulk depth control for json_flatten — one setting, whole group */}
                              {isFlatten && (
                                <div className="flex items-center gap-1.5 shrink-0">
                                  <Label className="text-xs text-muted-foreground hidden sm:inline">
                                    Depth
                                  </Label>
                                  <select
                                    className="h-8 rounded-md border border-input bg-background px-2 text-xs"
                                    value={bulkDepth}
                                    onChange={(e) =>
                                      setGroupMaxDepth(group.items, Number(e.target.value))
                                    }
                                  >
                                    {bulkDepth === "" && <option value="">Mixed</option>}
                                    <option value="1">1</option>
                                    <option value="2">2</option>
                                    <option value="3">3</option>
                                    <option value="0">All</option>
                                  </select>
                                </div>
                              )}
                            </div>

                            {!folded && (
                              <>
                                {TRANSFORM_TYPE_BLURBS[group.type] && (
                                  <div className="px-3 pt-2 text-xs text-muted-foreground">
                                    {TRANSFORM_TYPE_BLURBS[group.type]}
                                  </div>
                                )}
                                <div className="divide-y divide-zinc-100 dark:divide-zinc-800">
                                  {group.items.map(({ transform, idx }) => {
                                    const rowOpen = expandedRows[idx] ?? false
                                    const enabled = enabledTransforms[idx] ?? true
                                    return (
                                      <div key={idx} className="px-3 py-2">
                                        <div className="flex items-center gap-3">
                                          <Checkbox
                                            checked={enabled}
                                            onCheckedChange={(checked) =>
                                              setEnabledTransforms((prev) => ({
                                                ...prev,
                                                [idx]: !!checked,
                                              }))
                                            }
                                          />
                                          <span
                                            className={`text-xs font-mono flex-1 min-w-0 truncate ${
                                              enabled ? "" : "text-muted-foreground line-through"
                                            }`}
                                            title={transformColumn(transform)}
                                          >
                                            {transformColumn(transform)}
                                          </span>
                                          <button
                                            type="button"
                                            onClick={() =>
                                              setExpandedRows((prev) => ({ ...prev, [idx]: !rowOpen }))
                                            }
                                            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground shrink-0"
                                          >
                                            <Settings2 className="h-3.5 w-3.5" />
                                            <span className="hidden sm:inline">
                                              {rowOpen ? "Hide" : "Edit"}
                                            </span>
                                          </button>
                                        </div>
                                        {rowOpen && (
                                          <div className="pl-7 pt-1">
                                            <TransformConfigEditor
                                              transform={transform}
                                              config={{
                                                ...(transform.config || {}),
                                                ...(editedConfigs[idx] || {}),
                                              }}
                                              onChange={(patch) =>
                                                setEditedConfigs((prev) => ({
                                                  ...prev,
                                                  [idx]: { ...(prev[idx] || {}), ...patch },
                                                }))
                                              }
                                            />
                                          </div>
                                        )}
                                      </div>
                                    )
                                  })}
                                </div>
                              </>
                            )}
                          </Card>
                        )
                      })
                    )}
                  </TabsContent>

                  <TabsContent value="custom" className="space-y-4 mt-4">
                    <p className="text-xs text-zinc-500 dark:text-zinc-400">
                      A rule added here joins the plan. Like every rule, it runs on the initial load and on
                      every change after it.
                    </p>
                    {ruleTypeLimit && (
                      <p className="text-xs text-amber-700 dark:text-amber-400">{describeRuleTypeLimit(ruleTypeLimit)}</p>
                    )}
                    <Card>
                      <CardContent className="pt-4 space-y-4">
                        <div className="space-y-2">
                          <Label htmlFor="custom-transform-operation">Operation</Label>
                          <Select
                            value={draftOp || undefined}
                            onValueChange={(v) => {
                              setDraftOp(v as TransformOperation)
                              setDraftConfig({})
                              setDraftError(null)
                            }}
                          >
                            <SelectTrigger id="custom-transform-operation" aria-label="Operation">
                              <SelectValue placeholder="Pick an operation" />
                            </SelectTrigger>
                            <SelectContent>
                              {SUPPORTED_OPERATION_LIST.filter((op) => operationRuns(ruleTypeLimit, op)).map((op) => (
                                <SelectItem key={op} value={op}>
                                  {TRANSFORM_OPERATIONS[op].name}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                          {draftOp && (
                            <p className="text-xs text-muted-foreground">
                              {TRANSFORM_OPERATIONS[draftOp].description}
                            </p>
                          )}
                        </div>
                        {draftOp && (
                          <TransformRuleFields
                            operation={draftOp}
                            config={draftConfig}
                            onChange={(next) => {
                              setDraftConfig(next)
                              setDraftError(null)
                            }}
                            schema={tableColumns}
                          />
                        )}
                        {draftError && (
                          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
                            {draftError}
                          </p>
                        )}
                        <div className="flex justify-end">
                          <Button size="sm" onClick={addCustomRule} disabled={!draftOp}>
                            <Plus className="h-4 w-4 mr-2" />
                            Add to plan
                          </Button>
                        </div>
                      </CardContent>
                    </Card>

                    {customRules.length > 0 && (
                      <div className="space-y-2">
                        <div className="text-sm font-medium">Added here ({customRules.length})</div>
                        {customRules.map((rule) => {
                          const op = rule.operation as TransformOperation
                          const name = TRANSFORM_OPERATIONS[op]?.name ?? rule.operation
                          const view = engineView(rule)
                          const d = describeTransform(view.type, view.config)
                          const editing = editingCustom === rule.id
                          return (
                            <Card key={rule.id}>
                              <CardContent className="pt-3 pb-3 space-y-3">
                                <div className="flex items-center gap-2">
                                  <div className="flex-1 min-w-0">
                                    <div className="text-sm font-medium">{name}</div>
                                    <div className="text-xs text-muted-foreground truncate">
                                      {d.primary}
                                      {d.secondary ? ` ${d.secondary}` : ""}
                                    </div>
                                  </div>
                                  <Button
                                    variant="ghost"
                                    size="sm"
                                    onClick={() => setEditingCustom(editing ? null : rule.id)}
                                    aria-expanded={editing}
                                  >
                                    {editing ? "Done" : "Edit"}
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="sm"
                                    aria-label={`Remove ${name}`}
                                    onClick={() => {
                                      setCustomRules((prev) => prev.filter((r) => r.id !== rule.id))
                                      if (editing) setEditingCustom(null)
                                    }}
                                  >
                                    <Trash2 className="h-4 w-4" />
                                  </Button>
                                </div>
                                {editing && (
                                  <TransformRuleFields
                                    operation={op}
                                    config={rule.config}
                                    onChange={(next) =>
                                      setCustomRules((prev) =>
                                        prev.map((r) => (r.id === rule.id ? { ...r, config: next } : r))
                                      )
                                    }
                                    schema={tableColumns}
                                  />
                                )}
                              </CardContent>
                            </Card>
                          )
                        })}
                      </div>
                    )}
                  </TabsContent>

                  <TabsContent value="optimizations" className="space-y-3 mt-4">
                    {(suggestions?.optimizations || []).length === 0 ? (
                      <Card>
                        <CardContent className="pt-6">
                          <div className="text-sm text-zinc-600 dark:text-zinc-400 text-center">
                            No optimization suggestions
                          </div>
                        </CardContent>
                      </Card>
                    ) : (
                      <>
                      <p className="text-xs text-zinc-500 dark:text-zinc-400">
                        Advice only: Save &amp; continue does not change any of these.
                      </p>
                      {(suggestions?.optimizations || []).map((opt, idx) => (
                        <Card key={idx} className="border-blue-200 dark:border-blue-800">
                          <CardContent className="pt-4">
                            <div className="flex items-start gap-3">
                              <Sparkles className="h-4 w-4 text-blue-600 dark:text-blue-400 mt-1" />
                              <div className="flex-1">
                                <div className="text-sm font-medium">
                                  {opt.description || opt.suggestion || opt.type}
                                </div>
                                {(opt.reason || opt.impact) && (
                                  <div className="text-xs text-zinc-600 dark:text-zinc-400 mt-1">
                                    {opt.reason || opt.impact}
                                  </div>
                                )}
                              </div>
                            </div>
                          </CardContent>
                        </Card>
                      ))}
                      </>
                    )}
                  </TabsContent>

                  <TabsContent value="plan" className="space-y-3 mt-4">
                    <div data-testid="transform-plan" className="space-y-2">
                      {planRules.length === 0 ? (
                        <Card>
                          <CardContent className="pt-6">
                            <div className="text-sm text-zinc-600 dark:text-zinc-400 text-center">
                              No transforms — data is copied as-is
                            </div>
                          </CardContent>
                        </Card>
                      ) : (
                        planRules.map((rule, idx) => {
                          const view = engineView(rule)
                          const d = describeTransform(view.type, view.config)
                          const table = typeof rule.config?.table === "string" ? rule.config.table : ""
                          return (
                            <div
                              key={`${idx}-${planKey(rule)}`}
                              className="flex items-start gap-3 rounded-md border border-zinc-200 dark:border-zinc-800 px-3 py-2"
                            >
                              <span className="text-xs text-muted-foreground tabular-nums mt-0.5">{idx + 1}.</span>
                              <div className="flex-1 min-w-0">
                                <div className="flex flex-wrap items-center gap-2">
                                  <span className="text-sm font-medium">{transformTypeLabel(view.type)}</span>
                                  {idx < existingCount && <Badge variant="secondary">saved</Badge>}
                                  {table && <Badge variant="outline">{table}</Badge>}
                                  {/* A legacy rule stored on one side only. The Transform
                                      Builder offers "Run on both"; this step never converts it. */}
                                  {rule.type === "producer" && <Badge variant="outline">Batch only</Badge>}
                                  {rule.type === "consumer" && <Badge variant="outline">CDC only</Badge>}
                                </div>
                                <div className="text-xs text-muted-foreground truncate">
                                  {d.primary}
                                  {d.secondary ? ` ${d.secondary}` : ""}
                                </div>
                              </div>
                            </div>
                          )
                        })
                      )}
                    </div>
                  </TabsContent>
                </div>
              </Tabs>

              <p className="px-1 pt-2 text-xs text-zinc-500 dark:text-zinc-400">
                You can change these later in the pipeline&apos;s Transforms tab; changes apply to data moved after the save.
              </p>

              {existingError && (
                <div
                  role="alert"
                  className="px-3 py-2 text-sm text-red-700 dark:text-red-300 bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800 rounded-md flex items-center gap-3"
                >
                  <span className="flex-1">
                    Couldn&apos;t load this pipeline&apos;s saved transforms: {existingError}. Saving new ones now
                    could delete them, so Save stays off until they load.
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      setExistingError(null)
                      setExistingAttempt((n) => n + 1)
                    }}
                  >
                    Retry
                  </Button>
                </div>
              )}

              {actionError && (
                <div
                  role="alert"
                  className="px-3 py-2 text-sm text-red-700 bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800 rounded-md"
                >
                  {actionError}
                </div>
              )}

              {confirmDiscard ? (
                <div
                  role="group"
                  aria-labelledby={discardQuestionId}
                  className="flex flex-col gap-3 rounded-md border border-amber-300 bg-amber-50 p-3 dark:border-amber-800 dark:bg-amber-950/30 sm:flex-row sm:items-center sm:justify-between"
                >
                  <div className="text-sm">
                    <p id={discardQuestionId} className="font-medium text-amber-900 dark:text-amber-100">
                      {addedCount > 0
                        ? `Discard ${addedCount} ${addedCount === 1 ? "rule" : "rules"} and start without ${addedCount === 1 ? "it" : "them"}?`
                        : "Discard your changes and start without new transforms?"}
                    </p>
                    <p className="text-amber-800 dark:text-amber-200">
                      Nothing from this step is saved
                      {draftOp ? ", including the rule not yet added to the plan" : ""}.
                    </p>
                  </div>
                  <div className="flex justify-end gap-2">
                    <Button ref={keepEditingRef} variant="outline" onClick={() => setConfirmDiscard(false)}>
                      Keep editing
                    </Button>
                    <Button
                      variant="destructive"
                      onClick={() => {
                        setConfirmDiscard(false)
                        void requestClose()
                      }}
                    >
                      Discard and start
                    </Button>
                  </div>
                </div>
              ) : (
              <DialogFooter className="gap-2">
                <Button
                  variant="outline"
                  onClick={requestClose}
                  // Only disabled while a skip is already in flight — NOT during Apply,
                  // so Skip stays a usable escape hatch if an Apply request hangs.
                  disabled={closing}
                >
                  {closing ? (
                    <>
                      <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                      Skipping…
                    </>
                  ) : existingCount > 0 ? (
                    // Skipping adds nothing; the rules already saved keep running.
                    "Skip — add nothing"
                  ) : (
                    "Skip — no transforms"
                  )}
                </Button>
                <Button onClick={handleApply} disabled={submitting || closing || loading || saveBlocked}>
                  {submitting ? (
                    <>
                      <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                      Saving…
                    </>
                  ) : (
                    <>Save &amp; continue ({planRules.length})</>
                  )}
                </Button>
              </DialogFooter>
              )}
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* Preview Dialog */}
      <Dialog open={previewOpen} onOpenChange={setPreviewOpen}>
        <DialogContent className="w-[95vw] max-w-6xl max-h-[90vh] overflow-hidden flex flex-col">
          <DialogHeader>
            <DialogTitle>Transform Preview: {previewTable}</DialogTitle>
            <DialogDescription>
              Showing up to 10 rows from {previewTable} with active transforms applied
            </DialogDescription>
          </DialogHeader>

          {previewLoading ? (
            <div className="flex items-center justify-center py-12">
              <Loader2 className="h-6 w-6 animate-spin text-violet-600" />
              <span className="ml-3 text-sm text-zinc-500 dark:text-zinc-400">Fetching sample rows and applying transforms…</span>
            </div>
          ) : !previewLoading && previewOpen && previewData === null ? (
            <div className="flex flex-col items-center justify-center py-12 gap-3 text-center">
              <Info className="h-8 w-8 text-zinc-400" />
              <div>
                <div className="font-medium text-zinc-700 dark:text-zinc-200">No transforms selected</div>
                <div className="text-sm text-zinc-500 dark:text-zinc-400 mt-1">
                  Enable at least one PII mask or transform to preview before/after rows.
                </div>
              </div>
            </div>
          ) : previewData ? (
            <ScrollArea className="flex-1">
              <div className="space-y-4">
                {/* Stats */}
                <Card>
                  <CardContent className="pt-4">
                    <div className="grid grid-cols-4 gap-4 text-sm">
                      <div>
                        <div className="text-zinc-500 dark:text-zinc-400">Rows</div>
                        <div className="font-medium">
                          {previewData.stats.rows_before} → {previewData.stats.rows_after}
                        </div>
                      </div>
                      <div>
                        <div className="text-zinc-500 dark:text-zinc-400">Columns</div>
                        <div className="font-medium">
                          {previewData.stats.columns_before} → {previewData.stats.columns_after}
                        </div>
                      </div>
                      <div className="col-span-2">
                        {previewData.warnings.length > 0 && (
                          <div className="text-amber-600 dark:text-amber-400 text-xs">
                            {previewData.warnings.join(", ")}
                          </div>
                        )}
                      </div>
                    </div>
                  </CardContent>
                </Card>

                {/* Data comparison */}
                <div className="grid grid-cols-2 gap-4">
                  {(() => {
                    const cols = inferColumnsForPreview(previewTable, previewData.original, previewData.transformed)
                    return (
                      <>
                        <PreviewGrid title="Original" columns={cols} rows={previewData.original} compareTo={previewData.transformed} />
                        <PreviewGrid title="Transformed" columns={cols} rows={previewData.transformed} compareTo={previewData.original} />
                      </>
                    )
                  })()}
                </div>
              </div>
            </ScrollArea>
          ) : null}

          <DialogFooter>
            <Button variant="outline" onClick={() => setPreviewOpen(false)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
