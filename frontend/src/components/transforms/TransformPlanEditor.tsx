"use client";

import React, { useEffect, useState } from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Plus,
  Trash2,
  GripVertical,
  Filter,
  Save,
  Wand2,
  Sparkles,
  Settings,
  ChevronDown,
  ChevronUp,
  Eye,
  AlertTriangle,
  RefreshCw,
} from "lucide-react";
import { authFetch } from "@/lib/api/auth-fetch";
import { useWorkspaceRole } from "@/contexts/WorkspaceContext";
import { meetsRole } from "@/lib/workspace/roles";
import {
  fromApiTransforms,
  isSupportedOperation,
  toEngineTransform,
  type TransformOperation,
  type TransformRule,
} from "@/lib/pipeline/transformOps";
import {
  describeReplace,
  describeRuleTypeLimit,
  FORWARD_ONLY_NOTE,
  isPlanDirty,
  loadPipelineTransformPlan,
  operationRuns,
  planFingerprint,
  savePipelineTransformPlan,
  type PlanRule,
} from "@/lib/pipeline/transformPlan";
import {
  SUPPORTED_OPERATION_LIST,
  TRANSFORM_OPERATIONS,
  UNSUPPORTED_OPERATION_LIST,
} from "@/components/transforms/transformOperations";
import { TransformRuleFields } from "@/components/transforms/TransformRuleFields";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { TabHint } from "@/components/ui/tab-hint";
import { useSuggestionTab } from "@/hooks/useSuggestionTab";
import { toast } from "sonner";

/** What Tab writes into the empty describe box: a rule that fits any table. */
export const NL_SUGGESTION = "Mask all email addresses";

const isRenderable = (operation: string) =>
  Object.prototype.hasOwnProperty.call(TRANSFORM_OPERATIONS, operation);

/**
 * Add and edit one pipeline's transforms, inside that pipeline's Transforms tab.
 *
 * This was the standalone Transform Builder page (/transforms). It lived apart
 * from the pipeline, so it needed a pipeline picker, and the tab showed the rules
 * while the page changed them — two screens for one list. The editor now opens
 * where the rules are shown, already bound to the pipeline.
 *
 * `onClose(true)` after a save, `onClose(false)` when the operator leaves without
 * one; the tab re-reads the stored plan either way it needs to.
 */
export function TransformPlanEditor({
  pipelineId,
  onClose,
}: {
  pipelineId: string;
  onClose: (saved: boolean) => void;
}) {
  const [transforms, setTransforms] = useState<TransformRule[]>([]);
  const [nlQuery, setNlQuery] = useState("");
  const nlTab = useSuggestionTab({ value: nlQuery, suggestion: NL_SUGGESTION, onAccept: setNlQuery });
  const [showAddDialog, setShowAddDialog] = useState(false);
  const [editingTransform, setEditingTransform] = useState<TransformRule | null>(null);
  const [previewData, setPreviewData] = useState<Record<string, any>[] | null>(null);
  const [previewSampleJson, setPreviewSampleJson] = useState(
    JSON.stringify(
      [
        { id: 1, email: "alice@example.com", name: "Alice", password: "secret", age: "29" },
        { id: 2, email: "bob@example.com", name: "Bob", password: "hunter2", age: null },
      ],
      null,
      2
    )
  );
  const [previewWarnings, setPreviewWarnings] = useState<string[]>([]);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const [existingRules, setExistingRules] = useState<PlanRule[]>([]);
  const [planError, setPlanError] = useState<string | null>(null);
  const [planLoading, setPlanLoading] = useState(true);
  // The only rule types the pipeline can run (consumer_rule_types, sent for a
  // MongoDB CDC pipeline); null = any type can. Only those are offered: the save
  // refuses any other, and every other rule it was sent with.
  const [ruleTypeLimit, setRuleTypeLimit] = useState<string[] | null>(null);
  // What the last Generate left out because the pipeline cannot run it.
  const [nlLeftOut, setNlLeftOut] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  // The plan as the server last confirmed it. Everything built since is unsaved.
  const [savedFingerprint, setSavedFingerprint] = useState(() => planFingerprint([]));
  // Asked before Cancel throws away unsaved work.
  const [discardOpen, setDiscardOpen] = useState(false);

  const { role } = useWorkspaceRole();
  // Mirrors requirePipelineWorkspaceRole(..., security.WSMember) on the save
  // handler. roleRank returns 0 for an unknown role, so this is false while the
  // workspace context is still loading rather than briefly true.
  const canSave = meetsRole(role, "member");

  // Every new rule runs on BOTH paths. A CDC pipeline's initial load can run as a
  // batch, so a rule saved to the CDC side alone left the historical rows untouched
  // (a mask that applied to changes but not to the rows copied first).
  const addTransform = (operation: TransformOperation) => {
    if (!isSupportedOperation(operation)) return;
    const config = TRANSFORM_OPERATIONS[operation];
    const newTransform: TransformRule = {
      id: crypto.randomUUID(),
      order: transforms.length,
      type: "both",
      operation,
      enabled: true,
      config: {},
      description: config.description,
    };
    setTransforms([...transforms, newTransform]);
    setEditingTransform(newTransform);
    setShowAddDialog(false);
  };

  const updateTransform = (id: string, updates: Partial<TransformRule>) => {
    setTransforms(transforms.map((t) => (t.id === id ? { ...t, ...updates } : t)));
  };

  const removeTransform = (id: string) => {
    setTransforms(transforms.filter((t) => t.id !== id));
  };

  const moveTransform = (id: string, direction: "up" | "down") => {
    const index = transforms.findIndex((t) => t.id === id);
    if (index === -1) return;

    const newIndex = direction === "up" ? index - 1 : index + 1;
    if (newIndex < 0 || newIndex >= transforms.length) return;

    const newTransforms = [...transforms];
    [newTransforms[index], newTransforms[newIndex]] = [newTransforms[newIndex], newTransforms[index]];
    newTransforms.forEach((t, i) => (t.order = i));
    setTransforms(newTransforms);
  };

  // Rules the editor has no card for — most importantly `mask_pii`, which the
  // natural-language pipeline setup materializes and which is NOT one of the
  // editor's operations. They are carried through the save untouched instead of
  // being dropped, because the save is a whole-plan replace: anything left out
  // of the request is deleted, and deleting a mask makes the next run copy
  // those columns to the destination in the clear.
  const carryOver = existingRules.filter((r) => !isRenderable(r.operation));
  const nextRules: PlanRule[] = [...transforms, ...carryOver];

  // Editing has to start from what is really stored, or the first save silently
  // deletes rows the operator never saw.
  const loadPlan = async () => {
    setPlanError(null);
    setPlanLoading(true);
    try {
      const out = await loadPipelineTransformPlan(pipelineId);
      if (!out.ok) {
        setPlanError(out.error);
        setExistingRules([]);
        return;
      }
      setExistingRules(out.data);
      setRuleTypeLimit(out.consumerRuleTypes);
      const loaded = out.data
        .filter((r) => isRenderable(r.operation))
        .map((r, i) => ({
          id: r.id,
          order: i,
          type: r.type,
          operation: r.operation as TransformOperation,
          enabled: r.enabled,
          config: r.config as Record<string, any>,
          description: r.description,
          // The consumer row's id, so a re-save updates both stored rows in place.
          pairId: r.pairId,
        }));
      setTransforms(loaded);
      // What is on screen now IS what is stored, so this is the baseline every
      // later edit is measured against.
      setSavedFingerprint(planFingerprint(loaded));
      setEditingTransform(null);
    } finally {
      setPlanLoading(false);
    }
  };

  useEffect(() => {
    // loadPlan sets state, which is the point: the editor opens holding the
    // pipeline's stored plan.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void loadPlan();
    // loadPlan is re-created on every render; the pipeline id is what matters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pipelineId]);

  const dirty = isPlanDirty(savedFingerprint, transforms);

  // The browser's own guard — the only thing that can stop a reload or a closed
  // tab. The message is the browser's (Chrome and Firefox ignore a custom one),
  // so the visible "Unsaved changes" marker carries the detail.
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  // Leaving the editor over unsaved work is a deletion, so it is asked rather
  // than done.
  const requestClose = () => {
    if (dirty) {
      setDiscardOpen(true);
      return;
    }
    onClose(false);
  };

  const savePlan = async () => {
    setSaving(true);
    try {
      const out = await savePipelineTransformPlan(pipelineId, nextRules);
      if (!out.ok) {
        toast.error(out.error);
        return;
      }
      setConfirmOpen(false);
      toast.success(
        `Saved ${out.data.count} ${out.data.count === 1 ? "transform" : "transforms"} to this pipeline.`
      );
      // The tab re-reads: the server mints ids for new rows and renumbers
      // transform_order, so what it lists is the stored plan, not this copy.
      setSavedFingerprint(planFingerprint(transforms));
      onClose(true);
    } finally {
      setSaving(false);
    }
  };

  const parseNaturalLanguage = async () => {
    if (!nlQuery.trim()) return;

    setLoading(true);
    try {
      // authFetch, not fetch: /api/v1 is behind AuthRequiredMiddleware and
      // CSRFMiddleware, so a bare POST here was a 401/403 every time.
      const res = await authFetch(`/api/v1/transforms/parse`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ natural_language: nlQuery }),
      });

      if (res.ok) {
        const data = await res.json();
        const generated = fromApiTransforms(data?.transforms);
        const parsed = generated.filter((r) => operationRuns(ruleTypeLimit, r.operation));
        const dropped = generated.filter((r) => !operationRuns(ruleTypeLimit, r.operation));
        setNlLeftOut(
          ruleTypeLimit && dropped.length > 0
            ? describeRuleTypeLimit(ruleTypeLimit, {
                count: dropped.length,
                noun: "generated rule",
                types: [...new Set(dropped.map((r) => String(toEngineTransform({ ...r, enabled: true })?.type ?? r.operation)))],
              })
            : null
        );
        if (parsed.length > 0) {
          // Append, don't replace: Generate is a starting point, not a whole plan.
          setTransforms((prev) => [
            ...prev,
            ...parsed.map((r, i) => ({
              ...r,
              order: prev.length + i,
              description: TRANSFORM_OPERATIONS[r.operation]?.description,
            })),
          ]);
          setNlQuery("");
        } else if (dropped.length === 0) {
          toast.error(
            "Nothing recognizable in that sentence. Try naming a column and one of: filter, mask, hash, rename, select, exclude."
          );
        }
      } else {
        const body = await res.json().catch(() => null);
        toast.error(body?.error || body?.message || `Could not parse that (HTTP ${res.status}).`);
      }
    } catch (error) {
      toast.error("The API is unreachable.");
      console.error("Failed to parse NL query:", error);
    } finally {
      setLoading(false);
    }
  };

  const previewTransformations = async () => {
    setLoading(true);
    setPreviewError(null);
    setPreviewWarnings([]);
    try {
      let sampleRows: unknown = [];
      try {
        sampleRows = JSON.parse(previewSampleJson || "[]");
      } catch {
        throw new Error("Preview sample data must be valid JSON (an array of row objects).");
      }

      if (!Array.isArray(sampleRows)) {
        throw new Error("Preview sample data must be a JSON array of row objects.");
      }

      const engineTransforms = transforms
        .map(toEngineTransform)
        .filter((t0): t0 is Record<string, any> => Boolean(t0));

      // Same gate as /transforms/parse above.
      const res = await authFetch(`/api/v1/transforms/preview`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ transforms: engineTransforms, sample_data: sampleRows }),
      });

      if (res.ok) {
        const data = await res.json();
        setPreviewData(data.preview || []);
        setPreviewWarnings(Array.isArray(data.warnings) ? data.warnings : []);
      } else {
        const body = await res.json().catch(() => null);
        const msg = body?.error || body?.message || "Failed to preview transformations.";
        throw new Error(msg);
      }
    } catch (error) {
      const msg = error instanceof Error ? error.message : "Failed to preview transformations.";
      setPreviewError(msg);
      console.error("Failed to preview transformations:", error);
    } finally {
      setLoading(false);
    }
  };

  const renderTransformCard = (transform: TransformRule, index: number) => {
    const config = TRANSFORM_OPERATIONS[transform.operation];
    const isExpanded = editingTransform?.id === transform.id;

    return (
      <Card
        key={transform.id}
        className={`mb-3 ${transform.enabled ? "" : "opacity-50"} ${isExpanded ? "ring-2 ring-blue-500" : ""}`}
      >
        <CardContent className="pt-4">
          <div className="flex items-center gap-3" data-testid="transform-card-row">
            <div className="cursor-move text-muted-foreground shrink-0">
              <GripVertical className="w-5 h-5" />
            </div>

            {/* min-w-0 is load-bearing, not tidying. A flex child defaults to
                min-width:auto, so this block could not shrink below the
                intrinsic width of the badge + operation name, and the button
                group to its right was pushed clean out of the card: measured on
                app.rsync.ai, "Rename Columns" overflowed by 125px, which put
                its settings and delete buttons past the card edge where no
                pointer could reach them. The name truncates; the controls stay. */}
            <div className="flex-1 min-w-0" data-testid="transform-card-title">
              {/* The name has its line to itself. With the badges beside it, every
                  sibling was shrink-0, so the name was the only thing that could
                  give way and on a narrow card it reached 0px — worst on the card
                  whose badge says to remove it. The badges wrap on the line below;
                  a floor on the name instead would push the controls off-card
                  again (#1166). The full name is on hover too. */}
              <div className="flex items-center gap-2 min-w-0 font-medium" data-testid="transform-card-name">
                <span className="shrink-0">{config.icon}</span>
                <span className="truncate" title={config.name}>{config.name}</span>
              </div>
              <div className="mt-1 flex flex-wrap items-center gap-2" data-testid="transform-card-badges">
                {transform.type === "both" ? (
                  <Badge variant="outline" className="text-muted-foreground">
                    Batch + CDC
                  </Badge>
                ) : (
                  // Saved by the older builder to one path only. It loads as it was
                  // stored and is never widened behind the operator's back: running a
                  // rule on a path it never ran on changes what that path writes, so
                  // it is offered, not done.
                  <>
                    <Badge
                      variant="outline"
                      className="border-amber-500 text-amber-600 dark:text-amber-500"
                      title={
                        transform.type === "producer"
                          ? "Runs on batch runs only — CDC change events are written without it."
                          : "Runs on CDC change events only — batch runs, including an initial load that runs as a batch, are written without it."
                      }
                    >
                      {transform.type === "producer" ? "Batch only" : "CDC only"}
                    </Badge>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="h-6 px-2 text-xs"
                      onClick={() => updateTransform(transform.id, { type: "both" })}
                      aria-label={`Run ${config.name} on both batch and CDC`}
                    >
                      Run on both
                    </Button>
                  </>
                )}
                {/* A plan saved before the dialog stopped offering these still
                    loads here, and the save refuses the whole plan while one is
                    present. Say which card is the problem. */}
                {!isSupportedOperation(transform.operation) && (
                  <Badge variant="destructive" className="text-[10px]">
                    No engine can run this — remove it to save
                  </Badge>
                )}
              </div>
              {!isExpanded && Object.keys(transform.config).length > 0 && (
                <p className="text-sm text-muted-foreground mt-1 truncate">
                  {JSON.stringify(transform.config)}
                </p>
              )}
            </div>

            {/* Every control here is icon-only, so the rule's name goes IN the
                label: a screen reader otherwise announced five bare "button"s per
                card. `title` carries the same string for pointer users. */}
            <div className="flex items-center gap-2 shrink-0" data-testid="transform-card-controls">
              <Switch
                checked={transform.enabled}
                onCheckedChange={(checked) => updateTransform(transform.id, { enabled: checked })}
                aria-label={`${transform.enabled ? "Disable" : "Enable"} ${config.name}`}
                title={`${transform.enabled ? "Disable" : "Enable"} ${config.name}`}
              />
              <Button
                variant="ghost"
                size="icon"
                onClick={() => moveTransform(transform.id, "up")}
                disabled={index === 0}
                aria-label={`Move ${config.name} earlier`}
                title={`Move ${config.name} earlier`}
              >
                <ChevronUp className="w-4 h-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                onClick={() => moveTransform(transform.id, "down")}
                disabled={index === transforms.length - 1}
                aria-label={`Move ${config.name} later`}
                title={`Move ${config.name} later`}
              >
                <ChevronDown className="w-4 h-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                onClick={() => setEditingTransform(isExpanded ? null : transform)}
                aria-expanded={isExpanded}
                aria-label={isExpanded ? `Hide ${config.name} settings` : `Edit ${config.name} settings`}
                title={isExpanded ? `Hide ${config.name} settings` : `Edit ${config.name} settings`}
              >
                <Settings className="w-4 h-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                className="text-red-500 hover:text-red-600"
                onClick={() => removeTransform(transform.id)}
                aria-label={`Remove ${config.name}`}
                title={`Remove ${config.name}`}
              >
                <Trash2 className="w-4 h-4" />
              </Button>
            </div>
          </div>

          {isExpanded && (
            <div className="mt-4 pt-4 border-t">
              <TransformRuleFields
                operation={transform.operation}
                config={transform.config}
                onChange={(next) => updateTransform(transform.id, { config: next })}
              />
            </div>
          )}
        </CardContent>
      </Card>
    );
  };

  return (
    <div className="space-y-4" data-testid="transform-plan-editor">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <h2 className="flex shrink-0 items-center gap-2 text-sm font-semibold text-zinc-900 dark:text-white">
          <Wand2 className="h-4 w-4 text-purple-500" />
          Edit transforms
        </h2>
        <div className="ml-auto flex items-center gap-2">
          {dirty && (
            <span className="text-xs text-amber-600 dark:text-amber-500 flex items-center gap-1" role="status">
              <AlertTriangle className="w-3.5 h-3.5" />
              Unsaved changes
            </span>
          )}
          <Button variant="outline" size="sm" className="h-7" onClick={requestClose} disabled={saving}>
            Cancel
          </Button>
          <Button
            size="sm"
            className="h-7"
            disabled={loading || saving || planLoading || Boolean(planError) || !canSave}
            onClick={() => setConfirmOpen(true)}
            title={!canSave ? "Only workspace members and admins can change a pipeline's transforms." : undefined}
          >
            {saving ? <RefreshCw className="w-3.5 h-3.5 mr-1.5 animate-spin" /> : <Save className="w-3.5 h-3.5 mr-1.5" />}
            Save transforms
          </Button>
        </div>
      </div>

      {planLoading && (
        <p className="text-sm text-muted-foreground flex items-center gap-2">
          <RefreshCw className="w-4 h-4 animate-spin" />
          Loading this pipeline&apos;s transforms…
        </p>
      )}

      {planError && (
        <Alert variant="destructive">
          <AlertTriangle className="w-4 h-4" />
          <AlertDescription>{planError}</AlertDescription>
        </Alert>
      )}

      {carryOver.length > 0 && (
        <Alert>
          <AlertTriangle className="w-4 h-4" />
          <AlertDescription>
            {carryOver.length} {carryOver.length === 1 ? "rule uses an operation" : "rules use operations"} this
            editor cannot display ({[...new Set(carryOver.map((r) => r.operation))].join(", ")}) — typically
            masking rules created by the pipeline&apos;s natural-language setup. They are not editable here and are
            kept unchanged when you save.
          </AlertDescription>
        </Alert>
      )}

      {!canSave && (
        <p className="text-sm text-muted-foreground">
          You can build and preview a plan, but only workspace members and admins can save one to a pipeline.
        </p>
      )}

      {/* The plan: ONE list. Each rule is stored twice -- a producer row the batch
          executor reads and a consumer row the CDC sink reads -- because a CDC
          pipeline's existing rows can arrive either way: as a Debezium snapshot
          (sink, consumer row) or, with a batch initial load, through the executor
          (producer row). Two lists made every rule one-sided by default. */}
      <Card>
        <CardHeader>
          <div className="flex justify-between items-center gap-3">
            <div className="min-w-0">
              <CardTitle className="text-base">Transforms</CardTitle>
              <CardDescription>
                Run in order on every batch run and every CDC change event, including the rows copied by an initial load.
              </CardDescription>
            </div>
            <Button size="sm" variant="outline" className="shrink-0" onClick={() => setShowAddDialog(true)}>
              <Plus className="w-4 h-4 mr-2" />
              Add
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {/* "No transforms" is a claim about what is stored, so it waits for a
              read that succeeded: a failed one is not an empty pipeline. */}
          {transforms.length === 0 ? (
            !planLoading &&
            !planError && (
              <div className="text-center py-8 text-muted-foreground border-2 border-dashed rounded-lg">
                <Filter className="w-10 h-10 mx-auto mb-2 opacity-50" />
                <p>No transforms</p>
                <p className="text-sm">Add filters, masks, or column selections</p>
              </div>
            )
          ) : (
            transforms.map((t, i) => renderTransformCard(t, i))
          )}
        </CardContent>
      </Card>

      <Card className="bg-gradient-to-br from-purple-500/10 to-blue-500/10 border-purple-500/20">
        <CardContent className="pt-6">
          <div className="flex flex-col gap-3 @xl:flex-row @xl:gap-4">
            <div className="flex-1">
              <Label htmlFor={`nl-${pipelineId}`} className="flex items-center gap-2 mb-2">
                <Sparkles className="w-4 h-4 text-purple-500" />
                Describe your transformations in natural language
              </Label>
              <Textarea
                id={`nl-${pipelineId}`}
                value={nlQuery}
                onChange={(e) => setNlQuery(e.target.value)}
                onKeyDown={nlTab.onKeyDown}
                placeholder={`e.g. ${NL_SUGGESTION}`}
                className="min-h-[80px]"
              />
              {nlTab.showHint && <TabHint className="mt-1.5" />}
              {nlLeftOut && <p className="mt-1.5 text-xs text-amber-700 dark:text-amber-400">{nlLeftOut}</p>}
            </div>
            <div className="flex flex-col justify-end">
              <Button
                onClick={parseNaturalLanguage}
                disabled={loading || !nlQuery.trim()}
                className="bg-purple-600 hover:bg-purple-700"
              >
                {loading ? <RefreshCw className="w-4 h-4 mr-2 animate-spin" /> : <Wand2 className="w-4 h-4 mr-2" />}
                Generate
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Preview sample rows (used by the backend preview endpoint). Nothing from
          the pipeline's source is read here: the rows are what the operator types. */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-4">
            <div className="min-w-0">
              <CardTitle className="text-base">Preview sample data</CardTitle>
              <CardDescription>Paste a JSON array of row objects to preview transforms.</CardDescription>
            </div>
            <Button variant="outline" className="shrink-0" onClick={previewTransformations} disabled={loading}>
              {loading ? <RefreshCw className="w-4 h-4 mr-2 animate-spin" /> : <Eye className="w-4 h-4 mr-2" />}
              Preview
            </Button>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          {previewError && (
            <Alert>
              <AlertDescription>{previewError}</AlertDescription>
            </Alert>
          )}
          {previewWarnings.length > 0 && (
            <Alert>
              <AlertDescription>
                <div className="font-medium mb-1">Preview warnings</div>
                <ul className="list-disc pl-5 space-y-1">
                  {previewWarnings.map((w, i) => (
                    <li key={i}>{w}</li>
                  ))}
                </ul>
              </AlertDescription>
            </Alert>
          )}
          <Textarea
            aria-label="Preview sample data"
            value={previewSampleJson}
            onChange={(e) => setPreviewSampleJson(e.target.value)}
            className="min-h-[140px] font-mono text-xs"
          />
        </CardContent>
      </Card>

      {previewData && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <Eye className="w-5 h-5" />
              Transform Preview
            </CardTitle>
            <CardDescription>Sample of transformed data</CardDescription>
          </CardHeader>
          <CardContent>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b">
                    {previewData.length > 0 &&
                      Object.keys(previewData[0]).map((key) => (
                        <th key={key} className="text-left p-2 font-medium">
                          {key}
                        </th>
                      ))}
                  </tr>
                </thead>
                <tbody>
                  {previewData.slice(0, 5).map((row, i) => (
                    <tr key={i} className="border-b">
                      {Object.values(row).map((val, j) => (
                        <td key={j} className="p-2 font-mono text-xs">
                          {String(val)}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </CardContent>
        </Card>
      )}

      <Dialog open={showAddDialog} onOpenChange={setShowAddDialog}>
        <DialogContent className="max-w-2xl max-h-[80vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Add Transformation</DialogTitle>
            <DialogDescription>
              Choose a transformation. It runs on batch runs and on CDC change events alike.
            </DialogDescription>
          </DialogHeader>
          {ruleTypeLimit && (
            <p className="text-xs text-amber-700 dark:text-amber-400">{describeRuleTypeLimit(ruleTypeLimit)}</p>
          )}

          <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
            {SUPPORTED_OPERATION_LIST.filter((op) => operationRuns(ruleTypeLimit, op)).map((op) => {
              const config = TRANSFORM_OPERATIONS[op];
              return (
                <Button
                  key={op}
                  variant="outline"
                  className="h-auto p-4 flex flex-col items-start gap-2"
                  onClick={() => addTransform(op)}
                >
                  <div className="flex items-center gap-2">
                    {config.icon}
                    <span className="font-medium">{config.name}</span>
                  </div>
                  <p className="text-xs text-muted-foreground text-left whitespace-normal">{config.description}</p>
                </Button>
              );
            })}
          </div>

          {/* Still listed, because they are a real roadmap — but not clickable,
              because no engine can run them. They are the Tier-2 (DuckDB)
              operations; that engine is a stub today. */}
          {UNSUPPORTED_OPERATION_LIST.length > 0 && (
            <div className="mt-6">
              <p className="text-xs font-medium text-muted-foreground mb-2">Not available yet — no execution engine</p>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                {UNSUPPORTED_OPERATION_LIST.map((op) => {
                  const config = TRANSFORM_OPERATIONS[op];
                  return (
                    <div
                      key={op}
                      className="h-auto p-4 flex flex-col items-start gap-2 rounded-md border border-dashed opacity-60"
                      aria-disabled="true"
                    >
                      <div className="flex items-center gap-2">
                        {config.icon}
                        <span className="font-medium text-sm">{config.name}</span>
                        <Badge variant="secondary" className="text-[10px]">
                          Coming soon
                        </Badge>
                      </div>
                      <p className="text-xs text-muted-foreground text-left">{config.description}</p>
                    </div>
                  );
                })}
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={discardOpen} onOpenChange={setDiscardOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard unsaved changes?</AlertDialogTitle>
            <AlertDialogDescription>
              {transforms.length} {transforms.length === 1 ? "rule is" : "rules are"} on screen that differ from what is
              stored on this pipeline. Leaving the editor throws them away, and they cannot be recovered.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep editing</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                setDiscardOpen(false);
                onClose(false);
              }}
            >
              Discard changes
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Saving is a REPLACE, not an append: the handler deletes every transform
          on the pipeline inside the transaction and re-inserts the request body.
          The dialog says so in counts, and names any masking rule that is about
          to disappear. */}
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Replace this pipeline&apos;s transforms?</AlertDialogTitle>
            <AlertDialogDescription>{describeReplace(existingRules, nextRules)}</AlertDialogDescription>
            {/* Transforms run as rows are moved; nothing re-reads the destination.
                Saying "forward only" was not enough for an operator to see that a
                new mask leaves the rows already copied in the clear. */}
            <p className="text-sm text-muted-foreground" data-testid="forward-only-note">
              {FORWARD_ONLY_NOTE}
            </p>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                void savePlan();
              }}
              disabled={saving}
            >
              {saving ? "Saving…" : "Replace and save"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
