"use client";

import React, { useState, useEffect, useMemo } from "react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogFooter } from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  CheckCircle,
  XCircle,
  AlertTriangle,
  Search,
  RefreshCw,
  Hash,
  Lock,
  Unlock,
  ShieldCheck,
} from "lucide-react";
import { API_ENDPOINTS } from "@/lib/config/api";
import { authFetch } from "@/lib/api/auth-fetch";
import { toast } from "sonner";
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace";
import { maskedColumnsNotScanned, type PIIMaskedColumn } from "@/lib/pii/masked-columns";
import { maskTypeLabel, piiTypeLabel } from "@/lib/pii/pii-types";
import Link from "next/link";
import { PageHeader } from "@/components/layout/PageHeader";
import { EmptyState } from "@/components/kit/EmptyState";
import { StatCard } from "@/components/kit/StatCard";
import { ChartCard } from "@/components/kit/ChartCard";
import { FilterChips, type FilterChip } from "@/components/kit/FilterChips";
import { StatusPill, pillToneClass, type PillTone } from "@/components/kit/StatusPill";
import { ChartCardsSkeleton, StatCardsSkeleton } from "@/components/kit/PageSkeleton";
import { EChartNoSSR } from "@/components/charts/EChartNoSSR";
import type { HBarChartSettings } from "@/components/charts/chart-settings";
import { hbarHeight } from "@/components/pipeline/table-stats-charts";

// Types
interface PIIScanResult {
  id: string;
  pipeline_id: string;
  table_name: string;
  column_name: string;
  pii_type: string;
  confidence: number;
  detection_method: string;
  suggested_masking: string;
  approved_action?: string;
  approved_by?: string;
  approved_at?: string;
  created_at: string;
}

interface ApprovalRequest {
  id: string;
  pipeline_id: string;
  requested_by: string;
  columns: PIIColumnRequest[];
  justification: string;
  status: "pending" | "approved" | "denied" | "partial" | "expired";
  decided_by?: string;
  decided_at?: string;
  conditions?: string[];
  expires_at?: string;
  created_at: string;
}

interface PIIColumnRequest {
  table_name: string;
  column_name: string;
  pii_type: string;
  requested_action: string;
  hash_function?: string;
  approved?: boolean;
  decision?: string;
}

interface PIIPolicy {
  id: string;
  policy_type: "always_mask" | "require_approval" | "audit_required";
  pii_type: string;
  action: string;
  hash_function?: string;
  condition?: string;
  priority: number;
  enabled: boolean;
}

interface HashFunction {
  id: string;
  name: string;
  type: "builtin" | "python" | "api" | "lookup_table";
  description: string;
  reversible: boolean;
  enabled: boolean;
}

type Decision = "approved" | "denied";

// The five reads the page is drawn from, in the order a failure is reported.
type Section = "scan" | "masked" | "approvals" | "policies" | "hashes";

const APPROVAL_TONES: Record<ApprovalRequest["status"], { tone: PillTone; label: string }> = {
  pending: { tone: "wait", label: "Pending" },
  approved: { tone: "ok", label: "Approved" },
  denied: { tone: "fail", label: "Denied" },
  partial: { tone: "wait", label: "Partly approved" },
  expired: { tone: "cancel", label: "Expired" },
};

const plural = (n: number, one: string, many = `${one}s`) => `${n.toLocaleString("en-US")} ${n === 1 ? one : many}`;

// What a section whose read failed shows in place of its count or its empty
// state: "Nothing waiting" under a failed read would be a confident fiction.
const DID_NOT_LOAD = "Did not load";
const TRY_AGAIN = "Use Refresh to try again.";

/** Bars that count columns by a label, most first: the two charts on the Overview tab. */
function countChart(description: string, label: string, rows: readonly { label: string }[]): HBarChartSettings {
  return {
    type: "hbar",
    stacked: false,
    description,
    x: { field: "label", label, categories: rows.map((r) => r.label) },
    y: { field: "columns", format: "integer", title: "Columns" },
    series: { values: [{ key: "columns", label: "Columns", color: "series-1" }] },
  };
}

/** How many of `items` fall under each label, most first. */
function countBy<T>(items: readonly T[], keyOf: (item: T) => string, labelOf: (key: string) => string) {
  const counts = new Map<string, number>();
  for (const item of items) counts.set(keyOf(item), (counts.get(keyOf(item)) ?? 0) + 1);
  return [...counts]
    .map(([key, columns]) => ({ key, label: labelOf(key), columns }))
    .sort((a, b) => b.columns - a.columns || a.label.localeCompare(b.label));
}

export default function PIIDashboardPage() {
  const [activeTab, setActiveTab] = useState("overview");
  const [scanResults, setScanResults] = useState<PIIScanResult[]>([]);
  const [maskedColumns, setMaskedColumns] = useState<PIIMaskedColumn[]>([]);
  const [maskedTruncated, setMaskedTruncated] = useState(false);
  const [approvalRequests, setApprovalRequests] = useState<ApprovalRequest[]>([]);
  const [policies, setPolicies] = useState<PIIPolicy[]>([]);
  const [hashFunctions, setHashFunctions] = useState<HashFunction[]>([]);
  const [loadErrors, setLoadErrors] = useState<string[]>([]);
  const [failed, setFailed] = useState<Partial<Record<Section, boolean>>>({});
  // Until the first load is back the cards are grey blocks, not a row of zeros.
  const [loaded, setLoaded] = useState(false);
  const [loading, setLoading] = useState(false);
  const [selectedRequest, setSelectedRequest] = useState<ApprovalRequest | null>(null);
  // Which button opened the dialog: it asks to confirm that one decision.
  const [decision, setDecision] = useState<Decision>("approved");
  const [showDecisionDialog, setShowDecisionDialog] = useState(false);
  const [decisionNotes, setDecisionNotes] = useState("");
  const [searchTerm, setSearchTerm] = useState("");
  const [filterPIIType, setFilterPIIType] = useState<string>("all");

  useEffect(() => {
    fetchData();
  }, []);

  // PII data is workspace-scoped; re-fetch when the active workspace changes
  // (header switcher / other tab) so the page never shows another workspace's
  // scan results, approvals, or policy counts. (R6)
  useEffect(
    () => onActiveWorkspaceChange(() => fetchData()),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const fetchData = async () => {
    const isStale = captureWorkspace();
    setLoading(true);

    // Read one section. Each of these used to be `if (res.ok)` with no else, so
    // a failing endpoint left its state at the initial empty array and the page
    // rendered "0 PII Columns Detected" — a clean, confident, wrong answer. That
    // is what the PII page showed for the whole period its tables were missing
    // from the schema and every one of these endpoints was returning 500. An
    // empty workspace and a broken backend have to look different.
    const read = async <T,>(label: string, path: string): Promise<{ data?: T; error?: string }> => {
      try {
        const res = await authFetch(`${API_ENDPOINTS.API_GATEWAY_URL}${path}`, { cache: "no-store" });
        if (!res.ok) return { error: `${label}: HTTP ${res.status}` };
        return { data: (await res.json()) as T };
      } catch (error) {
        console.error(`Failed to fetch ${label}:`, error);
        return { error: `${label}: ${error instanceof Error ? error.message : "request failed"}` };
      }
    };

    // The five reads run side by side, and nothing is applied until all are back.
    const [scan, masked, approvals, pols, hashes] = await Promise.all([
      read<{ results?: PIIScanResult[] }>("Scan results", "/api/v1/pii/scan/results"),
      read<{ columns?: PIIMaskedColumn[]; truncated?: boolean }>("Masked columns", "/api/v1/pii/masked-columns"),
      read<{ requests?: ApprovalRequest[] }>("Approval requests", "/api/v1/pii/approvals?status=pending"),
      read<{ policies?: PIIPolicy[] }>("Policies", "/api/v1/pii/policies"),
      read<{ functions?: HashFunction[] }>("Hash functions", "/api/v1/hash-functions"),
    ]);
    // The active workspace changed mid-flight: half of one workspace's findings
    // shown beside half of another's would be worse than the previous
    // workspace's, so all five are dropped and the switch's own refresh draws.
    if (isStale()) return;

    if (scan.data) setScanResults(scan.data.results || []);
    if (masked.data) {
      setMaskedColumns(masked.data.columns || []);
      setMaskedTruncated(Boolean(masked.data.truncated));
    }
    if (approvals.data) setApprovalRequests(approvals.data.requests || []);
    if (pols.data) setPolicies(pols.data.policies || []);
    if (hashes.data) setHashFunctions(hashes.data.functions || []);
    const sections: [Section, { error?: string }][] = [
      ["scan", scan],
      ["masked", masked],
      ["approvals", approvals],
      ["policies", pols],
      ["hashes", hashes],
    ];
    setFailed(Object.fromEntries(sections.map(([s, r]) => [s, Boolean(r.error)])));
    setLoadErrors(sections.flatMap(([, r]) => (r.error ? [r.error] : [])));
    setLoaded(true);
    setLoading(false);
  };

  const openDecision = (request: ApprovalRequest, next: Decision) => {
    setSelectedRequest(request);
    setDecision(next);
    setShowDecisionDialog(true);
  };

  const handleApprovalDecision = async (requestId: string, decision: Decision) => {
    try {
      const res = await authFetch(`${API_ENDPOINTS.API_GATEWAY_URL}/api/v1/pii/approvals/${requestId}/decide`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          decision,
          notes: decisionNotes,
        }),
      });

      if (res.ok) {
        toast.success(decision === "approved" ? "Request approved" : "Request denied");
        setApprovalRequests(prev => prev.filter(r => r.id !== requestId));
        setShowDecisionDialog(false);
        setDecisionNotes("");
        setSelectedRequest(null);
        return;
      }

      // `if (res.ok)` with no else: a rejected decision produced no message, no
      // console line and no state change -- the row simply stayed in the list and
      // the dialog stayed open, which looks like a mis-click. On this page that
      // silence is worse than cosmetic: an approval that never landed leaves PII
      // masking un-applied while the operator believes they have decided it.
      const body = (await res.json().catch(() => null)) as Record<string, unknown> | null;
      const raw = body?.error ?? body?.message ?? body?.detail;
      const reason = typeof raw === "string" && raw.trim() ? raw.trim() : `The server answered ${res.status}.`;
      toast.error(
        decision === "approved" ? "Could not approve this request" : "Could not deny this request",
        { description: `${reason} The request is unchanged.` }
      );
    } catch (error) {
      console.error("Failed to submit decision:", error);
      toast.error("Could not submit the decision", {
        description: `${error instanceof Error ? error.message : "The request never reached the server."} The request is unchanged.`,
      });
    }
  };

  const maskedOnly = maskedColumnsNotScanned(scanResults, maskedColumns);
  const pending = approvalRequests.filter(r => r.status === "pending");
  const policiesOn = policies.filter(p => p.enabled).length;
  const maskingPipelines = new Set(maskedColumns.map(m => m.pipeline_id)).size;

  // The types the scans found, most columns first. The type filter is built
  // from them, so it offers every type the scanner reports and none it has not.
  const typeRows = useMemo(() => countBy(scanResults, (r) => r.pii_type, piiTypeLabel), [scanResults]);
  const maskRows = useMemo(() => countBy(maskedColumns, (m) => m.action, maskTypeLabel), [maskedColumns]);
  const typeChart = useMemo(
    () => countChart("Columns PII scans found, by PII type, most first.", "PII type", typeRows),
    [typeRows],
  );
  const maskChart = useMemo(
    () => countChart("Columns pipelines mask, by how they are masked, most first.", "Mask type", maskRows),
    [maskRows],
  );
  const typeChips: FilterChip<string>[] = [
    { value: "all", label: "All types", count: scanResults.length },
    ...typeRows.map((r) => ({ value: r.key, label: r.label, count: r.columns })),
  ];
  // A type picked from a list that no longer has it (a refresh) reads as All.
  const typeFilter = typeChips.some((c) => c.value === filterPIIType) ? filterPIIType : "all";
  // Filtered by the type the chips show, so a pressed "All types" lists them all.
  const filteredScanResults = scanResults.filter(result => {
    const matchesSearch =
      result.table_name.toLowerCase().includes(searchTerm.toLowerCase()) ||
      result.column_name.toLowerCase().includes(searchTerm.toLowerCase());
    const matchesFilter = typeFilter === "all" || result.pii_type === typeFilter;
    return matchesSearch && matchesFilter;
  });

  return (
    <div className="space-y-5">
      <PageHeader heading="PII Management" description="Detect, protect, and manage personally identifiable information">
        <Button onClick={fetchData} variant="outline" size="sm" disabled={loading}>
          <RefreshCw className={`w-4 h-4 mr-2 ${loading ? "motion-safe:animate-spin" : ""}`} />
          Refresh
        </Button>
      </PageHeader>

      {/* A section that could not be loaded says so. The counts below are
          derived from these five fetches, so a silent failure would show up as
          a confident zero rather than as a problem. */}
      {loadErrors.length > 0 && (
        <Alert variant="destructive">
          <AlertTriangle className="w-4 h-4" />
          <AlertDescription>
            Some PII data could not be loaded, so the figures below are incomplete:{" "}
            {loadErrors.join("; ")}. Use Refresh to try again.
          </AlertDescription>
        </Alert>
      )}

      {!loaded ? (
        <StatCardsSkeleton count={4} label="Loading PII findings…" className="@xl:grid-cols-2 @5xl:grid-cols-4" />
      ) : (
        <div className="grid gap-4 @xl:grid-cols-2 @5xl:grid-cols-4">
          {/* A pipeline that masks a column found PII there too; without the
              masks this read 0 while pipelines masked columns. */}
          <StatCard
            title="Columns found"
            value={failed.scan || failed.masked ? undefined : scanResults.length + maskedOnly}
            hint={failed.scan || failed.masked ? DID_NOT_LOAD : `${scanResults.length} from scans · ${maskedOnly} masked by pipelines`}
          />
          <StatCard
            title="Masked"
            value={failed.masked ? undefined : maskedColumns.length}
            hint={
              failed.masked
                ? DID_NOT_LOAD
                : maskedColumns.length > 0
                  ? `in ${plural(maskingPipelines, "pipeline")}`
                  : "No pipeline masks a column"
            }
          />
          <StatCard
            title="Pending approvals"
            value={failed.approvals ? undefined : pending.length}
            tone={!failed.approvals && pending.length > 0 ? "warn" : "default"}
            hint={failed.approvals ? DID_NOT_LOAD : pending.length > 0 ? "Waiting for an admin" : "Nothing waiting"}
          />
          {/* Deliberately not "Protected", and no padlock: these policies are
              stored, not enforced (see the note on the Policies tab). */}
          <StatCard
            title="Policies"
            value={failed.policies ? undefined : policiesOn}
            hint={failed.policies ? DID_NOT_LOAD : `of ${policies.length} on · stored, not enforced`}
          />
        </div>
      )}

      {/* Main Tabs */}
      <Tabs value={activeTab} onValueChange={setActiveTab} className="space-y-4">
        {/* An "Audit Log" tab said only "will be displayed here": nothing feeds it yet,
            so it is gone until something does (launch review, PR1). */}
        <TabsList className="grid grid-cols-4 w-full max-w-2xl">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="approvals">
            Approvals
            {pending.length > 0 && (
              <span className={`ml-2 rounded-full px-1.5 text-xs font-medium tabular-nums ${pillToneClass("wait")}`}>
                {pending.length}
              </span>
            )}
          </TabsTrigger>
          <TabsTrigger value="policies">Policies</TabsTrigger>
          <TabsTrigger value="hashing">Hash functions</TabsTrigger>
        </TabsList>

        {/* Overview Tab */}
        <TabsContent value="overview" className="space-y-4">
          {!loaded ? (
            <ChartCardsSkeleton />
          ) : (
            <div className="grid gap-4 @3xl:grid-cols-2">
              <ChartCard
                title="Scanned columns by type"
                description={
                  failed.scan
                    ? "Scan results did not load."
                    : `${plural(scanResults.length, "column")} found by scans. Click a bar to list its columns.`
                }
              >
                {typeRows.length === 0 ? (
                  <EmptyState
                    size="sm"
                    icon={Search}
                    title={failed.scan ? "Scan results did not load" : "No scan results yet"}
                    description={failed.scan ? TRY_AGAIN : "Columns a PII scan flags are counted here by type."}
                    className="h-[200px]"
                  />
                ) : (
                  <EChartNoSSR
                    settings={typeChart}
                    rows={typeRows}
                    height={hbarHeight(typeRows.length)}
                    onCategoryClick={(i) => typeRows[i] && setFilterPIIType(typeRows[i].key)}
                  />
                )}
              </ChartCard>
              <ChartCard
                title="How masked columns are protected"
                description={`${plural(maskedColumns.length, "column")} pipelines mask, by mask type`}
                empty={
                  failed.masked
                    ? `Masked columns did not load. ${TRY_AGAIN}`
                    : maskRows.length === 0
                      ? "No pipeline masks a column yet"
                      : undefined
                }
                emptyIcon={ShieldCheck}
              >
                <EChartNoSSR settings={maskChart} rows={maskRows} height={hbarHeight(maskRows.length)} />
              </ChartCard>
            </div>
          )}

          {/* Detected PII Table */}
          <Card>
            <CardHeader className="space-y-3">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <CardTitle>Detected PII columns</CardTitle>
                  <CardDescription>Columns a PII scan identified as personal data</CardDescription>
                </div>
                <div className="relative">
                  <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 text-muted-foreground w-4 h-4" />
                  <Input
                    aria-label="Search columns"
                    placeholder="Search columns..."
                    className="pl-10 w-64"
                    value={searchTerm}
                    onChange={(e) => setSearchTerm(e.target.value)}
                  />
                </div>
              </div>
              {typeRows.length > 0 && (
                <FilterChips label="PII type" chips={typeChips} value={typeFilter} onChange={setFilterPIIType} />
              )}
            </CardHeader>
            <CardContent>
              <Table aria-label="Detected PII columns">
                <TableHeader>
                  <TableRow>
                    <TableHead>Detected column</TableHead>
                    <TableHead>Type</TableHead>
                    <TableHead>Confidence</TableHead>
                    <TableHead>Suggested</TableHead>
                    <TableHead>Status</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filteredScanResults.length === 0 && (
                    <TableRow>
                      <TableCell colSpan={5} className="py-8 text-center text-sm text-muted-foreground">
                        {failed.scan
                          ? `Scan results did not load. ${TRY_AGAIN}`
                          : scanResults.length === 0
                            ? "No scan has flagged a column yet."
                            : "No column matches this search and type."}
                      </TableCell>
                    </TableRow>
                  )}
                  {filteredScanResults.map((result) => (
                    <TableRow key={result.id}>
                      <TableCell className="font-mono text-sm">
                        {result.table_name}.{result.column_name}
                      </TableCell>
                      <TableCell className="text-sm">{piiTypeLabel(result.pii_type)}</TableCell>
                      <TableCell title={`Found by ${result.detection_method}`}>
                        <div className="flex items-center gap-2">
                          <div className="w-20 bg-zinc-100 dark:bg-zinc-800 rounded-full h-1.5">
                            <div
                              className="bg-blue-600 dark:bg-blue-400 h-1.5 rounded-full"
                              style={{ width: `${result.confidence * 100}%` }}
                            />
                          </div>
                          <span className="text-sm tabular-nums">{(result.confidence * 100).toFixed(0)}%</span>
                        </div>
                      </TableCell>
                      <TableCell className="font-mono text-sm">{result.suggested_masking}</TableCell>
                      <TableCell>
                        {result.approved_action ? (
                          <StatusPill tone="ok" title={`Approved action: ${result.approved_action}`}>
                            {result.approved_action === result.suggested_masking
                              ? "Approved"
                              : `Approved: ${result.approved_action}`}
                          </StatusPill>
                        ) : (
                          <StatusPill tone="skip">Not reviewed</StatusPill>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </CardContent>
          </Card>

          {/* Read-only: this lists what the pipelines' saved transforms say,
              and changes nothing. */}
          <Card>
            <CardHeader>
              <CardTitle>Masked by pipelines</CardTitle>
              <CardDescription>
                Columns your pipelines&apos; saved transforms mask or hash. To change one, edit that pipeline&apos;s transforms.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {maskedTruncated && (
                <p className="text-sm text-muted-foreground mb-3">
                  This workspace has more transform rules than one page reads, so this list is incomplete.
                </p>
              )}
              {maskedColumns.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  {failed.masked
                    ? `Masked columns did not load. ${TRY_AGAIN}`
                    : "No pipeline in this workspace masks or hashes a column."}
                </p>
              ) : (
                <Table aria-label="Columns masked by pipelines">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Pipeline</TableHead>
                      <TableHead>Table</TableHead>
                      <TableHead>Column</TableHead>
                      <TableHead>Action</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {maskedColumns.map((m) => (
                      <TableRow key={`${m.pipeline_id}|${m.table ?? ""}|${m.column}|${m.action}`}>
                        <TableCell>
                          <Link href={`/pipelines/${m.pipeline_id}`} className="hover:underline">
                            {m.pipeline_name || m.pipeline_id}
                          </Link>
                        </TableCell>
                        <TableCell className="font-mono text-sm">
                          {m.table || <span className="text-muted-foreground font-sans">Every table</span>}
                        </TableCell>
                        <TableCell className="font-mono text-sm">{m.column}</TableCell>
                        <TableCell>
                          <Badge variant="outline">{m.action}</Badge>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </TabsContent>

        {/* Approvals Tab */}
        <TabsContent value="approvals" className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>Pending approval requests</CardTitle>
              <CardDescription>Review and decide on PII transfer requests</CardDescription>
            </CardHeader>
            <CardContent>
              {pending.length === 0 ? (
                <EmptyState
                  size="sm"
                  icon={CheckCircle}
                  title={failed.approvals ? "Approval requests did not load" : "No pending approval requests"}
                  description={failed.approvals ? TRY_AGAIN : "A request to move PII unmasked waits here for an admin."}
                />
              ) : (
                <div className="space-y-4">
                  {pending.map((request) => (
                    <div
                      key={request.id}
                      className="rounded-xl border border-amber-200 p-4 dark:border-amber-400/20"
                    >
                      <div className="flex flex-wrap justify-between items-start gap-3">
                        <div className="min-w-0 space-y-2">
                          <div className="flex items-center gap-2">
                            <AlertTriangle className="w-5 h-5 text-amber-600 dark:text-amber-400" />
                            <span className="font-semibold">PII transfer request</span>
                            <StatusPill tone={APPROVAL_TONES[request.status].tone}>
                              {APPROVAL_TONES[request.status].label}
                            </StatusPill>
                          </div>
                          <p className="text-sm text-muted-foreground">
                            Pipeline:{" "}
                            <Link href={`/pipelines/${request.pipeline_id}`} className="font-mono hover:underline">
                              {request.pipeline_id}
                            </Link>
                          </p>
                          <p className="text-sm">
                            <span className="text-muted-foreground">Requested by:</span>{" "}
                            {request.requested_by}
                          </p>
                          <p className="text-sm">
                            <span className="text-muted-foreground">Justification:</span>{" "}
                            {request.justification}
                          </p>

                          <div className="mt-4">
                            <p className="text-sm font-medium mb-2">Columns requested:</p>
                            <div className="flex flex-wrap gap-2">
                              {request.columns.map((col, idx) => (
                                <Badge key={idx} variant="outline" className="text-xs">
                                  {col.table_name}.{col.column_name} ({piiTypeLabel(col.pii_type)}) → {col.requested_action}
                                </Badge>
                              ))}
                            </div>
                          </div>
                        </div>
                        <div className="flex gap-2">
                          <Button
                            variant="outline"
                            className="border-red-200 text-red-700 hover:bg-red-50 dark:border-red-400/30 dark:text-red-300 dark:hover:bg-red-400/10"
                            onClick={() => openDecision(request, "denied")}
                          >
                            <XCircle className="w-4 h-4 mr-2" />
                            Deny
                          </Button>
                          <Button variant="success" onClick={() => openDecision(request, "approved")}>
                            <CheckCircle className="w-4 h-4 mr-2" />
                            Approve
                          </Button>
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>

        {/* Policies Tab */}
        <TabsContent value="policies" className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>PII policies</CardTitle>
              <CardDescription>
                Stored policy records. These are not enforced by the pipeline runtime —
                to actually mask a column, add the <code>mask_pii</code> transform to the
                pipeline.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Table aria-label="PII policies">
                <TableHeader>
                  <TableRow>
                    <TableHead>Policy type</TableHead>
                    <TableHead>PII type</TableHead>
                    <TableHead>Action</TableHead>
                    <TableHead>Condition</TableHead>
                    <TableHead>Priority</TableHead>
                    <TableHead>Status</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {policies.length === 0 && (
                    <TableRow>
                      <TableCell colSpan={6} className="py-8 text-center text-sm text-muted-foreground">
                        {failed.policies ? `Policies did not load. ${TRY_AGAIN}` : "No policy is stored in this workspace."}
                      </TableCell>
                    </TableRow>
                  )}
                  {policies.map((policy) => (
                    <TableRow key={policy.id}>
                      <TableCell className="font-mono text-sm">{policy.policy_type}</TableCell>
                      <TableCell className="text-sm">{piiTypeLabel(policy.pii_type)}</TableCell>
                      <TableCell className="font-mono text-sm">{policy.action}</TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {policy.condition || "-"}
                      </TableCell>
                      <TableCell className="tabular-nums">{policy.priority}</TableCell>
                      <TableCell>
                        {/* Grey either way: an enabled policy is stored, not enforced. */}
                        <StatusPill tone={policy.enabled ? "cancel" : "skip"}>
                          {policy.enabled ? "Enabled" : "Disabled"}
                        </StatusPill>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </CardContent>
          </Card>
        </TabsContent>

        {/* Hash Functions Tab */}
        <TabsContent value="hashing" className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>Hash functions</CardTitle>
              <CardDescription>Built-in and custom hash functions for PII masking</CardDescription>
            </CardHeader>
            <CardContent>
              {hashFunctions.length === 0 ? (
                <EmptyState
                  size="sm"
                  icon={Hash}
                  title={failed.hashes ? "Hash functions did not load" : "No hash functions listed"}
                  description={failed.hashes ? TRY_AGAIN : "The functions a mask can hash with are listed here."}
                />
              ) : (
                <div className="grid grid-cols-1 @xl:grid-cols-2 @3xl:grid-cols-3 gap-4">
                  {hashFunctions.map((func) => (
                    <div key={func.id} className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800">
                      <div className="flex items-start justify-between">
                        <div>
                          <h3 className="font-semibold flex items-center gap-2">
                            <Hash className="w-4 h-4" />
                            {func.name}
                          </h3>
                          <Badge variant="outline" className="mt-1">{func.type}</Badge>
                        </div>
                        {func.reversible ? (
                          <Unlock className="w-5 h-5 text-amber-600 dark:text-amber-400" aria-label="Reversible" />
                        ) : (
                          <Lock className="w-5 h-5 text-emerald-600 dark:text-emerald-400" aria-label="One-way" />
                        )}
                      </div>
                      <p className="text-sm text-muted-foreground mt-3">{func.description}</p>
                      <div className="flex items-center justify-between mt-4">
                        <span className="text-xs text-muted-foreground">
                          {func.reversible ? "Reversible" : "One-way"}
                        </span>
                        <StatusPill tone={func.enabled ? "ok" : "skip"}>
                          {func.enabled ? "Active" : "Disabled"}
                        </StatusPill>
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>

      </Tabs>

      {/* Decision Dialog: it confirms the one decision whose button opened it. */}
      <Dialog open={showDecisionDialog} onOpenChange={setShowDecisionDialog} closeOnOutsideClick={false}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{decision === "approved" ? "Approve this request?" : "Deny this request?"}</DialogTitle>
            <DialogDescription>
              {decision === "approved"
                ? "You are approving this PII transfer request."
                : "You are denying this PII transfer request."}
            </DialogDescription>
          </DialogHeader>

          {selectedRequest && (
            <div className="space-y-4">
              <Alert>
                <AlertTriangle className="w-4 h-4" />
                <AlertDescription>
                  This decision covers {plural(selectedRequest.columns.length, "column")} containing sensitive data.
                </AlertDescription>
              </Alert>

              <div className="space-y-2">
                <Label htmlFor="pii-decision-notes">Decision notes</Label>
                <Textarea
                  id="pii-decision-notes"
                  placeholder="Add any notes or conditions for this decision..."
                  value={decisionNotes}
                  onChange={(e) => setDecisionNotes(e.target.value)}
                />
              </div>
            </div>
          )}

          <DialogFooter>
            <Button variant="outline" onClick={() => setShowDecisionDialog(false)}>
              Cancel
            </Button>
            {decision === "denied" ? (
              <Button
                variant="destructive"
                onClick={() => selectedRequest && handleApprovalDecision(selectedRequest.id, "denied")}
              >
                Deny request
              </Button>
            ) : (
              <Button
                variant="success"
                onClick={() => selectedRequest && handleApprovalDecision(selectedRequest.id, "approved")}
              >
                Approve request
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
