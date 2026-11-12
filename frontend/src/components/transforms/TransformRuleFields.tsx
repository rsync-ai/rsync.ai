"use client";

import React, { useId } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { TRANSFORM_OPERATIONS, type TransformField } from "@/components/transforms/transformOperations";
import type { TransformOperation } from "@/lib/pipeline/transformOps";

/** One source table and its column names, as the creation dialog knows them. */
export interface TableColumns {
  table: string;
  columns: string[];
}

// Radix Select refuses an empty-string item value, so "every table" needs a token.
const ALL_TABLES = "__all_tables__";

/**
 * The settings of one transform rule: the operation's own fields and, when the
 * tables are known, which table it applies to.
 *
 * The Transforms tab's editor and the pipeline-creation transform step both edit rules
 * with this, so the two cannot drift into storing different shapes for the same
 * operation. The table choice is written as `config.table`, which the engine reads
 * as the rule's scope (shared/go/transforms/validate.go normalizeOne) on both the
 * batch path and the CDC path; with no `table` the rule applies to every table.
 */
export function TransformRuleFields({
  operation,
  config,
  onChange,
  schema,
}: {
  operation: TransformOperation;
  config: Record<string, any>;
  onChange: (next: Record<string, any>) => void;
  /** The source tables and their columns. Without it every field is free text. */
  schema?: TableColumns[];
}) {
  const baseId = useId();
  const op = TRANSFORM_OPERATIONS[operation];
  if (!op) return null;

  const table = typeof config.table === "string" && config.table.trim() ? config.table.trim() : "";
  const tables = schema?.filter((t) => t.table) ?? [];
  // Columns of the chosen table, or of every selected table when the rule is not
  // scoped. Deduplicated: two tables commonly share `id`, `created_at`.
  const columns = Array.from(
    new Set(
      (table ? tables.filter((t) => t.table === table) : tables).flatMap((t) => t.columns)
    )
  );

  const set = (name: string, value: unknown) => onChange({ ...config, [name]: value });

  const setTable = (value: string) => {
    const next = { ...config };
    if (value === ALL_TABLES || !value) delete next.table;
    else next.table = value;
    onChange(next);
  };

  const renderColumnField = (field: TransformField, id: string) => {
    if (field.column === "one") {
      const current = String(config[field.name] ?? "");
      return (
        <Select value={current || undefined} onValueChange={(v) => set(field.name, v)}>
          <SelectTrigger id={id} aria-label={field.label}>
            <SelectValue placeholder="Pick a column" />
          </SelectTrigger>
          <SelectContent>
            {/* A value saved earlier that is not in this schema stays pickable,
                so opening the editor never silently changes the rule. */}
            {current && !columns.includes(current) && (
              <SelectItem value={current}>{current}</SelectItem>
            )}
            {columns.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      );
    }
    // "many": stored as the comma-separated string the free-text box produced, so a
    // rule reads the same whichever editor wrote it.
    const chosen = String(config[field.name] ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    const toggle = (c: string) => {
      const next = chosen.includes(c) ? chosen.filter((x) => x !== c) : [...chosen, c];
      set(field.name, next.join(", "));
    };
    return (
      <div id={id} role="group" aria-label={field.label} className="flex flex-wrap gap-1.5">
        {Array.from(new Set([...chosen.filter((c) => !columns.includes(c)), ...columns])).map((c) => {
          const on = chosen.includes(c);
          return (
            <Button
              key={c}
              type="button"
              size="sm"
              variant={on ? "default" : "outline"}
              className="h-7 px-2 text-xs font-mono"
              aria-pressed={on}
              onClick={() => toggle(c)}
            >
              {c}
            </Button>
          );
        })}
      </div>
    );
  };

  return (
    <div className="space-y-4">
      {tables.length > 0 ? (
        <div className="space-y-2">
          <Label htmlFor={`${baseId}-table`}>Applies to</Label>
          <Select value={table || ALL_TABLES} onValueChange={setTable}>
            <SelectTrigger id={`${baseId}-table`} aria-label="Applies to">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_TABLES}>All selected tables</SelectItem>
              {table && !tables.some((t) => t.table === table) && (
                <SelectItem value={table}>{table}</SelectItem>
              )}
              {tables.map((t) => (
                <SelectItem key={t.table} value={t.table}>
                  {t.table}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      ) : (
        // No schema to pick from (the Transforms tab's editor). A scope written by the
        // creation step is still shown, because a rule that silently applies to one
        // table only would read as applying to all of them.
        table && (
          <div className="space-y-2">
            <Label htmlFor={`${baseId}-table`}>Applies to table</Label>
            <Input
              id={`${baseId}-table`}
              value={table}
              onChange={(e) => setTable(e.target.value)}
              placeholder="Every table"
            />
          </div>
        )
      )}

      {op.fields.map((field) => {
        const id = `${baseId}-${field.name}`;
        return (
          <div key={field.name} className="space-y-2">
            <Label htmlFor={id}>{field.label}</Label>
            {field.column && columns.length > 0 ? (
              renderColumnField(field, id)
            ) : field.type === "textarea" ? (
              <Textarea
                id={id}
                value={config[field.name] || ""}
                onChange={(e) => set(field.name, e.target.value)}
                placeholder={field.placeholder}
                className="font-mono"
                rows={4}
              />
            ) : field.type === "select" ? (
              <Select value={config[field.name] || ""} onValueChange={(value) => set(field.name, value)}>
                <SelectTrigger id={id} aria-label={field.label}>
                  <SelectValue placeholder={`Select ${field.label}`} />
                </SelectTrigger>
                <SelectContent>
                  {field.options?.map((opt) => (
                    <SelectItem key={opt.value} value={opt.value}>
                      {opt.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : field.type === "boolean" ? (
              <Switch
                id={id}
                checked={config[field.name] || false}
                onCheckedChange={(checked) => set(field.name, checked)}
              />
            ) : field.type === "number" ? (
              <Input
                id={id}
                type="number"
                value={config[field.name] || ""}
                onChange={(e) => set(field.name, parseInt(e.target.value) || 0)}
                placeholder={field.placeholder}
              />
            ) : (
              <Input
                id={id}
                value={config[field.name] || ""}
                onChange={(e) => set(field.name, e.target.value)}
                placeholder={field.placeholder}
              />
            )}
          </div>
        );
      })}
    </div>
  );
}
