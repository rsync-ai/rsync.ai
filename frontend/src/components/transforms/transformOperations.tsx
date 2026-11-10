/**
 * What each transform operation is called and which settings it takes.
 *
 * Shared by the Transforms tab's editor and the pipeline-creation transform step
 * (SuggestionsReviewDialog), so a rule added in either place is edited with the
 * same fields and stored in the same shape.
 */
import React from "react";
import {
  ArrowRight,
  Code,
  Copy,
  EyeOff,
  Filter,
  Hash,
  Settings,
  Sparkles,
  Table2,
} from "lucide-react";
import {
  isSupportedOperation,
  SUPPORTED_OPERATIONS,
  type TransformOperation,
} from "@/lib/pipeline/transformOps";

// No side here. Every rule runs on both the batch and the CDC path (see RuleSides in
// transformOps.ts); the per-operation producer/consumer default this table used to
// carry only decided which tab an operation appeared under.
export interface TransformOperationConfig {
  name: string;
  description: string;
  icon: React.ReactNode;
  fields: TransformField[];
}

export interface TransformField {
  name: string;
  label: string;
  type: "text" | "textarea" | "select" | "multiselect" | "number" | "boolean";
  options?: { value: string; label: string }[];
  placeholder?: string;
  required?: boolean;
  /**
   * The field names source columns: "one" column, or "many" as a comma-separated
   * list. Where the editor knows the tables' schema it offers those columns to
   * pick from instead of a free-text box.
   */
  column?: "one" | "many";
}

// Transform operation configurations
export const TRANSFORM_OPERATIONS: Record<TransformOperation, TransformOperationConfig> = {
  filter: {
    name: "Filter Rows",
    description: "Filter rows based on a condition",
    icon: <Filter className="w-4 h-4" />,
    fields: [
      { name: "condition", label: "Filter Condition", type: "text", placeholder: "amount > 100 AND status = 'active'", required: true },
    ],
  },
  select: {
    name: "Select Columns",
    description: "Include only specific columns",
    icon: <Table2 className="w-4 h-4" />,
    fields: [
      { name: "columns", label: "Columns (comma-separated)", type: "text", placeholder: "id, name, email", required: true, column: "many" },
    ],
  },
  exclude: {
    name: "Exclude Columns",
    description: "Remove specific columns",
    icon: <EyeOff className="w-4 h-4" />,
    fields: [
      { name: "columns", label: "Columns to Exclude", type: "text", placeholder: "password, secret_key", column: "many" },
    ],
  },
  rename: {
    name: "Rename Columns",
    description: "Rename column names",
    icon: <Settings className="w-4 h-4" />,
    fields: [
      { name: "mappings", label: "Mappings (old:new, comma-separated)", type: "text", placeholder: "email:user_email, name:full_name" },
    ],
  },
  mask: {
    name: "Mask PII",
    description: "Apply PII masking to columns",
    icon: <Hash className="w-4 h-4" />,
    fields: [
      { name: "column", label: "Column", type: "text", required: true, column: "one" },
      { name: "mask_type", label: "Mask Type", type: "select", options: [
        { value: "hash", label: "Hash (SHA256)" },
        { value: "redact", label: "Redact" },
        { value: "partial_mask", label: "Partial Mask" },
        { value: "remove", label: "Remove" },
      ]},
      // Only the digests hashValue() implements (shared/go/transforms/engine.go).
      // Its default branch silently falls back to SHA-256, so SHA-512, BLAKE2 and
      // "Custom" used to claim an algorithm that was never applied.
      { name: "hash_function", label: "Hash Function", type: "select", options: [
        { value: "sha256", label: "SHA-256" },
        { value: "hmac_sha256", label: "HMAC-SHA256 (needs the server hash key)" },
        { value: "md5", label: "MD5 (legacy)" },
      ]},
    ],
  },
  hash: {
    name: "Hash Column",
    description: "Apply hash function to a column",
    icon: <Hash className="w-4 h-4" />,
    fields: [
      { name: "column", label: "Column", type: "text", required: true, column: "one" },
      // Only the digests hashValue() implements (shared/go/transforms/engine.go).
      // Its default branch silently falls back to SHA-256, so SHA-512, BLAKE2 and
      // "Custom" used to claim an algorithm that was never applied.
      { name: "hash_function", label: "Hash Function", type: "select", options: [
        { value: "sha256", label: "SHA-256" },
        { value: "hmac_sha256", label: "HMAC-SHA256 (needs the server hash key)" },
        { value: "md5", label: "MD5 (legacy)" },
      ]},
    ],
  },
  type_convert: {
    name: "Type Conversion",
    description: "Convert column data types",
    icon: <ArrowRight className="w-4 h-4" />,
    fields: [
      { name: "column", label: "Column", type: "text", required: true, column: "one" },
      // Only the targets convertValue implements (shared/go/transforms/engine.go).
      // "Date" used to sit here and had no case at all: validateConfig rejected
      // it outright ("unsupported target type"), so the option could be picked
      // and never saved. Its companion "Format (for dates)" field was read by
      // nothing in either engine and went with it.
      { name: "to_type", label: "Target Type", type: "select", options: [
        { value: "string", label: "String" },
        { value: "integer", label: "Integer" },
        { value: "float", label: "Float" },
        { value: "boolean", label: "Boolean" },
      ]},
    ],
  },
  null_handle: {
    name: "Handle Nulls",
    description: "Handle null values in columns",
    icon: <Settings className="w-4 h-4" />,
    fields: [
      { name: "column", label: "Column", type: "text", required: true, column: "one" },
      // Only the two strategies the engine actually has (shared/go/transforms/
      // engine.go applyNullHandle: "default"/"fill"/"fill_default", or "drop_row").
      // A third option, "Raise Error", used to sit here and was mapped to drop_row
      // on the way out — it silently dropped the row it promised to complain about.
      { name: "action", label: "Action", type: "select", options: [
        { value: "default", label: "Replace with Default" },
        { value: "skip", label: "Drop Row" },
      ]},
      // Required because strategy=default without it is rejected by the validator.
      { name: "default_value", label: "Default Value", type: "text", required: true },
    ],
  },
  truncate: {
    name: "Truncate",
    description: "Truncate string values",
    icon: <Settings className="w-4 h-4" />,
    fields: [
      { name: "column", label: "Column", type: "text", required: true, column: "one" },
      { name: "max_length", label: "Max Length", type: "number", placeholder: "255" },
    ],
  },
  aggregate: {
    name: "Aggregate",
    description: "Group and aggregate data",
    icon: <Table2 className="w-4 h-4" />,
    fields: [
      { name: "group_by", label: "Group By (comma-separated)", type: "text", placeholder: "region, month" },
      { name: "aggregations", label: "Aggregations (column:func, comma-separated)", type: "text", placeholder: "amount:SUM, orders:COUNT" },
    ],
  },
  join: {
    name: "Join",
    description: "Join with another table",
    icon: <ArrowRight className="w-4 h-4" />,
    fields: [
      { name: "lookup_table", label: "Lookup Table", type: "text", required: true },
      { name: "join_key", label: "Join Key", type: "text", required: true },
      { name: "lookup_key", label: "Lookup Key", type: "text" },
      { name: "join_type", label: "Join Type", type: "select", options: [
        { value: "left", label: "Left Join" },
        { value: "inner", label: "Inner Join" },
        { value: "right", label: "Right Join" },
      ]},
    ],
  },
  enrich: {
    name: "Enrich",
    description: "Enrich data from external API",
    icon: <Sparkles className="w-4 h-4" />,
    fields: [
      { name: "api_endpoint", label: "API Endpoint", type: "text", required: true },
      { name: "input_mapping", label: "Input Mapping (col:field)", type: "text" },
      { name: "output_mapping", label: "Output Mapping (field:col)", type: "text" },
      { name: "cache_enabled", label: "Enable Cache", type: "boolean" },
    ],
  },
  deduplicate: {
    name: "Deduplicate",
    description: "Remove duplicate rows",
    icon: <Copy className="w-4 h-4" />,
    fields: [
      { name: "columns", label: "Dedup Columns (comma-separated)", type: "text", placeholder: "Leave empty for all columns" },
    ],
  },
  sort: {
    name: "Sort",
    description: "Sort rows by columns",
    icon: <ArrowRight className="w-4 h-4" />,
    fields: [
      { name: "columns", label: "Sort Columns (comma-separated)", type: "text", required: true },
      { name: "descending", label: "Descending", type: "boolean" },
    ],
  },
  limit: {
    name: "Limit",
    description: "Limit number of rows",
    icon: <Filter className="w-4 h-4" />,
    fields: [
      { name: "limit", label: "Limit", type: "number", placeholder: "1000", required: true },
      { name: "offset", label: "Offset", type: "number", placeholder: "0" },
    ],
  },
  sql: {
    name: "SQL Query",
    description: "Custom SQL transformation",
    icon: <Code className="w-4 h-4" />,
    fields: [
      { name: "query", label: "SQL Query", type: "textarea", placeholder: "SELECT * FROM data WHERE amount > 100", required: true },
    ],
  },
  python_udf: {
    name: "Python UDF",
    description: "Custom Python function",
    icon: <Code className="w-4 h-4" />,
    fields: [
      { name: "name", label: "Function Name", type: "text", required: true },
      { name: "code", label: "Python Code", type: "textarea", placeholder: "def transform(row):\n    return row", required: true },
      { name: "input_cols", label: "Input Columns", type: "text" },
      { name: "output_cols", label: "Output Columns", type: "text" },
    ],
  },
};

// Producer vs consumer is WHERE a rule runs, not WHAT it can do. The batch executor
// reads the producer rows and the CDC sink reads the consumer rows, but both build the
// same engine (kafka-sink-worker/main.go: NewSimpleTransformEngine +
// NewTransformCoordinator). So every supported operation is legal on both paths, which
// is what lets every rule be stored on both.
export const SUPPORTED_OPERATION_LIST = (Object.keys(TRANSFORM_OPERATIONS) as TransformOperation[])
  .filter(isSupportedOperation);

// Uses the set rather than isSupportedOperation, whose `op is TransformOperation`
// guard would narrow the negative branch to never.
export const UNSUPPORTED_OPERATION_LIST = (Object.keys(TRANSFORM_OPERATIONS) as TransformOperation[])
  .filter((op) => !SUPPORTED_OPERATIONS.has(op));
