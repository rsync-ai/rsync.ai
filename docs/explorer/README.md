# The Data Explorer

Write SQL (or open SQL that Ask rsync.ai drafted from a question), run it against a customer
warehouse, then keep the useful ones — as a saved query, a version-controlled artifact, or a
table that rebuilds itself on a schedule.

This page is the map of the whole feature. The one subsystem large enough to need its own
document has one: [Saved queries, models & schedules](saved-queries-and-models.md). Charts and
dashboards, which draw saved queries and models, have theirs:
[Charts and dashboards](charts-and-dashboards.md).

| For | Read |
|---|---|

---

## 1. The shape of it

```
      you type SQL         Ask rsync.ai generate_sql
            │              (question → top-K tables → SQL;
            │               nothing runs)
            │                           │ Open in Explorer
            └────────────► query tab ◄──┘
                               │
                        /explorer/query
                               │
              role gate ──► engine ──► rows (PII-redacted)
                               │
        ┌──────────────────────┼──────────────────────┐
        ▼                      ▼                      ▼
        export             save it            make it a model
    (csv/tsv/json)      (versions, diff,     (rebuild on cron,
                         restore, approval)   interval, or pipeline)
```

SQL you type and SQL you open from Ask rsync.ai run through one execution path. Everything
downstream — export, saving, scheduling — hangs off the same validated SQL.

## 2. Feature map

| Feature | Where |
|---|---|
| Run SQL against a connection | [§3](#3-running-sql) |
| Natural language → SQL | [§4](#4-natural-language--sql) |
| Schema browser, index, and table retrieval | [§5](#5-schema-intelligence) |
| Human-in-the-loop table/column resolution | [§6](#6-human-in-the-loop-resolution) |
| Suggested next actions after a result (route only; no page calls it since D2a) | [§6](#6-human-in-the-loop-resolution) |
| **Save a query; versions, diff, restore, approval** | [§7](#7-saving-versioning-and-scheduling) → [deep dive](saved-queries-and-models.md) |
| **Models: materialize a query as a table** | [§7](#7-saving-versioning-and-scheduling) → [deep dive](saved-queries-and-models.md) |
| **Schedules: cron, interval, after-pipeline** | [§7](#7-saving-versioning-and-scheduling) → [deep dive](saved-queries-and-models.md) |
| Export to CSV / TSV / JSON | [§8](#8-export-and-sharing) |
| **Create chart: chart a saved query; dashboards** | [Charts and dashboards](charts-and-dashboards.md) |
| BI tools (Send to BI removed) | [§9](#9-bi-tools) |
| PII redaction, egress and LLM data rules | [§10](#10-data-protection) |
| SQL editor, completions, statement splitting | [§11](#11-frontend) |

---

## 3. Running SQL

`POST /api/v1/explorer/query` →
[`ExecuteExplorerQuery`](../../api-gateway/internal/handlers/explorer.go:352).

**Role gate first, always.** `validators.ValidateExplorerStatement` runs before anything
touches a connection ([:377](../../api-gateway/internal/handlers/explorer.go:377)):

| Class | Verbs | Minimum role |
|---|---|---|
| `read` | `SELECT`, `WITH` | any member |
| `dml_write` | `INSERT`, `UPDATE`, `DELETE`, `MERGE` | admin |
| `ddl` | `CREATE`, `ALTER` | admin |
| `destructive` | `DROP`, `TRUNCATE`, `ALTER … DROP` | **owner** |
| `blocked` | `GRANT`/`REVOKE`/`CALL`/`EXEC`/`SET` | nobody |

Two status codes, deliberately distinguished
([:378-391](../../api-gateway/internal/handlers/explorer.go:378)): a role shortfall is
**403** (you are authenticated but not permitted); malformed SQL, a blocked class, or
**more than one statement** is **400**. The single-statement rule is a security boundary —
see [the deep dive's §3](saved-queries-and-models.md#3-authorization--the-part-to-change-carefully)
for why stacked SQL is the thing it exists to stop.

**Then routing.** [`ResolveExplorerCapability`](../../api-gateway/internal/handlers/explorer_capability.go:74)
is the single source of truth; unsupported connectors 400 here rather than failing deeper.

The engines live in [`shared/go/sqlexec`](../../shared/go/sqlexec/engine.go:78): `sqlexec.For`
picks one by connector type, and the gateway calls it in process.

| Engine | Strategy | Reads | Writes |
|---|---|---|---|
| PostgreSQL / Redshift | direct driver | `executePostgresQueryWithRedact` | `executeDirectWrite` |
| MySQL / MariaDB | direct driver | `executeMySQLQueryWithRedact` | `executeDirectWrite` |
| SQL Server | direct driver | `executeSQLServerQueryWithRedact` | `executeDirectWrite` |
| Databricks | direct (REST) | `executeDatabricksQueryParams` | `executeDatabricksWrite` |
| BigQuery, ClickHouse, Snowflake, Oracle | **delegated** | [`Orchestrator.ExplorerQuery`](../../shared/go/sqlexec/orchestrator.go:25) | same, via MCP execute |
| MongoDB | **delegated**, document mode — not SQL | `/explorer/documents/find` and `/aggregate` ([plan](document-browse-mode-plan.md)) | none; a model's `$out` only ([§8](saved-queries-and-models.md#mongodb-models)) |

Reads scan rows; writes take a no-rows path and report an affected-row count.

**Errors are classified, not blanket-500'd**
([:469-480](../../api-gateway/internal/handlers/explorer.go:469)). A typo or a stale table
reference is the user's, not the server's: `syntax_error`/`missing_table_or_column` → 400,
`permission_denied` → 403, `timeout` → 504, `network_or_unavailable` → 502.

**Writes are audited.** [`auditExplorerWrite`](../../api-gateway/internal/handlers/explorer.go:508)
records who ran what, against which connection, with the row count and the statement
truncated to 2000 chars. Best-effort — an audit failure never fails the query.

> **Known defect — `LIMIT` is silently downgraded.**
> [`validators.ClampLimit`](../../shared/go/sqlexec/stmt/sql_validator.go:660) replaces a
> user's explicit `LIMIT` with the server cap whenever theirs is larger
> ([:581-584](../../shared/go/sqlexec/stmt/sql_validator.go:581)), and says nothing.
> Preview caps at 500 rows, export at 10 000. Tracked as **DX-LimitDowngrade**; the fix is to honour `min(user, cap)` *and* return a
> "limited to N" flag.

---

## 4. Natural language → SQL

`POST /api/v1/sql/generate` →
[`GenerateSQL`](../../api-gateway/internal/handlers/explorer.go:120). Note the path has
**no `/explorer` prefix** ([main.go:1435](../../api-gateway/cmd/server/main.go:1435)) — it
predates the Explorer namespace and is shared with other NL→SQL callers. Calls the
`TEXT2SQL_ENDPOINT` service (falling back to `LLM_SERVICE_URL`), 120s context timeout.

Four things happen around the LLM call that are easy to miss:

- **The quota gate runs first** ([:132](../../api-gateway/internal/handlers/explorer.go:132)).
  A workspace over its monthly NL→SQL allowance gets **402** *before* the request is built,
  so a blocked workspace pays no LLM cost. Fail-open: no workspace, nil DB, or a query
  error all read as unlimited.
- **The dialect is derived from the connection, not trusted from the client**
  ([:137-164](../../api-gateway/internal/handlers/explorer.go:137)). The UI sends the right
  one, but a caller that omits it used to get PostgreSQL SQL for a MySQL warehouse.
- **Missing schema context produces a warning, not silence**
  ([:171-178](../../api-gateway/internal/handlers/explorer.go:171)). Without schema the
  generator guesses column names — `active` vs `is_active` — and fails at execution. The
  warning makes the cause visible instead of the SQL merely being wrong.
- **Metering counts one success, not one attempt**
  ([`recordNLQueryUsage`](../../api-gateway/internal/handlers/explorer.go:304)). It sits
  after the non-empty-SQL guard, so failed generations bill nothing, and the gateway sees
  one request/response regardless of the llm-service's internal retry fan-out. Direct SQL
  via `/explorer/query` involves no LLM and is never metered.

> **Known gap:** no timeout-plus-backoff on the explorer's NL→SQL calls, so a transient
> 429 or cold start shows as a hung spinner (~70s observed). **DX-SqlGenResilience**.

---

## 5. Schema intelligence

| Route | Handler |
|---|---|
| `GET /explorer/connections/:id/schema-index` | [`GetSchemaIndex`](../../api-gateway/internal/handlers/explorer.go:610) |
| `POST /explorer/connections/:id/schema-index/refresh` | [`RefreshSchemaIndex`](../../api-gateway/internal/handlers/explorer.go:710) |
| `POST /explorer/tables/retrieve` | [`RetrieveTables`](../../api-gateway/internal/handlers/explorer.go:885) |
| `POST /explorer/connections/:id/tables/recommend` | [`GetRecommendedTablesForExplorer`](../../api-gateway/internal/handlers/explorer.go:555) |

[`buildSchemaIndex`](../../api-gateway/internal/handlers/explorer.go:789) fans out to a
per-engine fetcher (Postgres, MySQL, SQL Server, Databricks) and collects tables, columns
and **foreign keys** — the FK graph is what lets NL resolution suggest join keys rather
than guessing them. Cached in Redis with a **10-minute TTL**
([main.go:518](../../api-gateway/cmd/server/main.go:518)) and stamped with a `SchemaHash`,
so a client can tell whether an answer was computed against the schema it is looking at.

Retrieval is **lexical, not vector**: `cache.ExtractSearchTerms` on the question, then
`cache.RetrieveTopTables` for the top K (default 20, hard cap 50 —
[:897-902](../../api-gateway/internal/handlers/explorer.go:897)). Its job is to fit a
large warehouse into a prompt, not to answer the question.

**Internal tables never surface.**
[`isInternalExplorerTable`](../../api-gateway/internal/handlers/explorer.go:537) hides
rsync's own bookkeeping and pipeline-staging tables (`_rsync*`, `rsync_*`, `flat_mysql_*`,
`flat_pg_*`, `flat_postgres_*`) from the index, NL resolution, and HITL. It tolerates a
schema qualifier, and is mirrored on the client by
[`internalTables.ts`](../../frontend/src/lib/explorer/internalTables.ts) — **the pair must
stay in lockstep**; a table hidden in one and shown in the other is the bug this shape
exists to prevent.

---

## 6. Human-in-the-loop resolution

| Route | Handler | Answers |
|---|---|---|
| `POST /explorer/nl/resolve-tables` | [`ResolveExplorerTables`](../../api-gateway/internal/handlers/explorer.go:999) | which tables does this question mean? |
| `POST /explorer/nl/resolve-columns` | [`ResolveExplorerColumns`](../../api-gateway/internal/handlers/explorer.go:1188) | which columns are the metrics/filters? |
| `POST /explorer/nl/next-steps` | [`GetExplorerNextSteps`](../../api-gateway/internal/handlers/explorer.go:1352) | now that there are results, what next? |

Both resolvers return ranked candidates with a confidence and a **reason string**, plus a
`needs_hitl` flag. The Explorer page used to stop and ask on it (a table or metric picker)
rather than proceed on a low-confidence guess; since D2a the Explorer writes no SQL from
questions and calls none of these routes (dashboards-design.md § 7.5). They stay served.
The Explorer's table picker went with it; the pipeline table selector
([PipelineTableSelector](../../frontend/src/components/pipeline/PipelineTableSelector.tsx))
has its own ranker gate, `AUTO_SELECT_MIN_CONFIDENCE`.

**The gateway does not own the HITL threshold.** It passes through the LLM service's own
`needs_hitl` ([:1145](../../api-gateway/internal/handlers/explorer.go:1145),
[:1315](../../api-gateway/internal/handlers/explorer.go:1315)). If you go looking for a
tunable confidence cutoff in the gateway, there isn't one — it lives in the LLM service.

`next-steps` degrades to one static suggestion (Download CSV) when the LLM is unavailable
([:1406-1421](../../api-gateway/internal/handlers/explorer.go:1406)). No frontend
calls it since D2a removed the Explorer's next-steps panel.

> **Known gap:** the SQL re-qualification regex has never been driven live. (The HITL picker
> half of the item went with D2a.) **DX-CornerCaseCoverage**.

---

## 7. Saving, versioning, and scheduling

This is the largest subsystem and has its **own document**:
**[Saved queries, models & schedules](saved-queries-and-models.md)**.

In brief:

- **Saving** — private or workspace-visible, one name per workspace.
- **Version control** — every edit snapshots the prior version in the same transaction, so
  history can never disagree with what ran. History, an in-repo line diff (no new
  dependency), and restore. **Restore is a normal edit**, not a privileged path, so it
  inherits the approval gate automatically.
- **Approval gate** — editing a *scheduled* query does not change it. The SQL becomes a
  proposal an admin must approve, because `saved_queries.sql_text` stays the approved text
  and is the column the run reads.
- **Models** — a saved query with `materialization` set rebuilds a warehouse table
  (`table` mode) or runs its statement as written (`statement` mode), on every SQL engine
  above. On MongoDB a model is a saved pipeline whose result is written to a collection,
  table mode only ([MongoDB models](saved-queries-and-models.md#mongodb-models)).
- **Schedules** — `cron`, `interval`, or `after_upstream` (fires when a producer completes).
  A producer is a pipeline **or** another model, and a model can name up to 16 of them; any
  one of them finishing rebuilds it. One live schedule per query: a model has a clock **or** a
  set of upstreams, never both.

---

## 8. Export and sharing

| Route | Handler | Notes |
|---|---|---|
| `POST /explorer/export` | [`ExportQueryHandler`](../../api-gateway/internal/handlers/explorer.go:1615) | csv / tsv / json |
| `GET /explorer/export.csv` | [`ExportCSVHandler`](../../api-gateway/internal/handlers/explorer.go:1478) | legacy, kept for deep links |

Export re-validates the SQL server-side and clamps the limit to 10 000 regardless of what
the client asked for ([:1627-1632](../../api-gateway/internal/handlers/explorer.go:1627)) —
"client sends 10000; we trust nothing".

**The redaction asymmetry is deliberate.** The preview path masks PII by column name; the
export path does **not** (both exports read with `sqlexec.Limits{Mask: false}`,
[:1547](../../api-gateway/internal/handlers/explorer.go:1547), [:1710](../../api-gateway/internal/handlers/explorer.go:1710)). A data owner exporting
their own rows should get their own data — and the export endpoint has already enforced
workspace-scoped ownership on the connection before it gets there. Read those two functions
together or the unredacted one looks like a hole.

> ### Email and Slack sharing were removed
> The Explorer's email share endpoint (`POST /explorer/share/email`) was removed in
> [#1233](https://github.com/rsync-ai/rsync-ai/pull/1233), and its Slack share (`POST /explorer/share/slack`,
> a webhook URL pasted on every send) in [#1516](https://github.com/rsync-ai/rsync-ai/pull/1516). A result reaches Slack through a
> workflow and the workspace's stored delivery channel (workflows);
> Ask rsync.ai drafts one. A one-off share that picks a stored channel is deferred.

---

## 9. BI tools

The Explorer has no BI hand-off. "Send to BI Tool" (`POST /explorer/metabase/dashboard`, which
created a Metabase card and dashboard from Explorer SQL) was removed: it had no documentation
and only Metabase worked. Charts and dashboards are built into rsync.ai instead (/charts,
/dashboards); see [Charts and dashboards](charts-and-dashboards.md). Export (§8) is the
way to take a result elsewhere.

---

## 10. Data protection

- **PII redaction on preview** — masked by column-name match before rows leave the gateway
  ([postgres.go:148](../../shared/go/sqlexec/postgres.go:148)), on every engine including
  the delegated path ([orchestrator.go:75](../../shared/go/sqlexec/orchestrator.go:75)). Bypassed
  only for data-owner export (§8).
- **Outbound TLS verifies by default.**
  [`resolvePostgresSSLMode`](../../shared/go/sqlexec/hostguard.go:62) defaults
  remote hosts to `verify-full` — encrypt *and* check the certificate and hostname. The
  previous `require` default encrypted without verifying, leaving a MITM window on customer
  DB traffic. Local/docker hosts default to `disable`; a server whose CA isn't in the image
  trust store opts out with `ssl_mode=require`.
- **Local-host classification is address-based**, not a textual dot heuristic, so an IPv6
  or public IPv4 literal is correctly treated as remote
  ([`isLocalDBHost`](../../shared/go/sqlexec/hostguard.go:19),
  [`isPrivateOrLoopbackIP`](../../shared/go/sqlexec/hostguard.go:41)). One
  residual is documented in the code: a dotless single-label hostname resolving to a public
  address is still treated as local.
- **Identifiers injected into `SET search_path` are guarded and quoted**
  ([`isSafeSchemaName`](../../shared/go/sqlexec/postgres.go:276),
  [`quotePGIdent`](../../shared/go/sqlexec/postgres.go:282)).
- **LLM prompts carry metadata only** — schema names, column names, types, FK edges, and the
  user's own question. **Never row values, query results, credentials, or PII.** This is a
  platform rule, not an Explorer-local one.
- **Audit logs are admin-only and are never sent to an LLM**, which is why the raw statement
  is stored unscrubbed ([:508-518](../../api-gateway/internal/handlers/explorer.go:508)).

---

## 11. Frontend

`frontend/src/components/explorer/` — [SqlEditor](../../frontend/src/components/explorer/SqlEditor.tsx),
[SchemaBrowser](../../frontend/src/components/explorer/SchemaBrowser.tsx),
plus the saved-query dialogs covered in the [deep dive](saved-queries-and-models.md#11-frontend).

`frontend/src/lib/explorer/` — [sqlCompletions](../../frontend/src/lib/explorer/sqlCompletions.ts) /
[sqlCompletionSources](../../frontend/src/lib/explorer/sqlCompletionSources.ts) (schema-aware
autocomplete), [schemaTree](../../frontend/src/lib/explorer/schemaTree.ts),
[runShortcut](../../frontend/src/lib/explorer/runShortcut.ts),
[queryTabs](../../frontend/src/lib/explorer/queryTabs.ts) (up to 10 query tabs per user and
workspace, the Open in Explorer hand-off, and History),
[internalTables](../../frontend/src/lib/explorer/internalTables.ts).

> ### The editor holds many queries; the server accepts one
> The editor is a scratchpad, but the whole buffer used to be sent — so the server's
> single-statement rule 400'd it, and selecting the one query you wanted changed nothing
> because the selection was never read.
> [`sqlStatements.ts`](../../frontend/src/lib/explorer/sqlStatements.ts) is a real lexer
> (`''` doubling, `E'…'` escapes, quoted and backticked identifiers, `--` and block
> comments, `$$`/`$tag$` dollar quoting — so a `;` inside a literal never splits) and
> `resolveRunTarget` picks **selection → statement under the caret → whole buffer**.
>
> This is a safety property, not a convenience: **every client-side gate reads the exact
> string the fetch will send.** Classifying the buffer while executing one statement is the
> shape that once let a declined `DROP` run. The destructive-confirm dialog captures its own
> target, so moving the caret behind the modal cannot swap the statement out from under it.
> The classifier lives in [`statementClass.ts`](../../frontend/src/lib/explorer/statementClass.ts)
> so the gate and the splitter are testable together. **The client gate is UX; the server
> gate is the boundary.** The two classifiers are pinned to each other by
> [`shared/explorer_statement_class_golden.json`](../../shared/explorer_statement_class_golden.json),
> read by a Go test and a vitest test: change a class in the fixture and in both
> classifiers, or one side goes red. Both strip comments exactly once, keeping the body of
> a MySQL `/*!…*/` executable comment, before reading the verb.

---

## 12. Every route

Auth-required and workspace-scoped ([main.go:1435–1605](../../api-gateway/cmd/server/main.go:1435)).

| Method | Path | Role |
|---|---|---|
| `POST` | `/explorer/query` | member; write classes escalate |
| `POST` | `/explorer/documents/find` · `/aggregate` | member (read-only; `$out`/`$merge` refused) |
| `POST` | `/sql/generate` — *no `/explorer` prefix* | member (quota-gated) |
| `POST` | `/explorer/connections/:id/tables/recommend` | member |
| `GET` | `/explorer/connections/:id/schema-index` | member |
| `POST` | `/explorer/connections/:id/schema-index/refresh` | member |
| `POST` | `/explorer/connections/:id/silver-sql` | viewer (writes nothing) |
| `POST` | `/explorer/tables/retrieve` | member |
| `POST` | `/explorer/nl/resolve-tables` | member |
| `POST` | `/explorer/nl/resolve-columns` | member |
| `POST` | `/explorer/nl/next-steps` | member |
| `GET` | `/explorer/export.csv` | member (legacy) |
| `POST` | `/explorer/export` | member |
| `GET`·`POST` | `/explorer/saved` | member |
| `GET`·`PATCH`·`DELETE` | `/explorer/saved/:id` | creator or admin |
| `GET` | `/explorer/saved/:id/versions` | member |
| `GET` | `/explorer/version-retention` | member |
| `PUT` | `/explorer/version-retention` | admin |
| `POST` | `/explorer/saved/:id/pending/approve` · `/reject` | **admin** |
| `GET` | `/explorer/schedules` | viewer |
| `PUT` | `/explorer/saved/:id/materialization` | admin |
| `POST` | `/explorer/saved/:id/run` | admin |
| `GET` | `/explorer/saved/:id/runs` | admin |
| `GET`·`POST`·`PUT`·`DELETE` | `/explorer/saved/:id/schedule` | admin |
| `POST` | `/explorer/saved/:id/schedule/pause` · `/resume` | admin |
| `PUT` | `/explorer/saved/:id/freshness` | admin |
| `GET` | `/explorer/saved/:id/checks` | viewer |
| `POST` | `/explorer/saved/:id/checks` | creator or admin; a `custom_sql` check on a scheduled model becomes a proposal |
| `POST` | `/explorer/saved/:id/checks/test` | viewer (records nothing) |
| `PUT`·`DELETE` | `/explorer/saved/:id/checks/:checkId` | creator or admin |
| `POST` | `/explorer/saved/:id/checks/pending/:pendingId/approve` · `/reject` | **admin** |
| `GET` | `/explorer/saved/:id/runs/:runId/checks` | viewer |
| `GET` | `/explorer/freshness` | viewer |
| `GET` | `/explorer/running` | viewer |
| `GET` | `/explorer/saved/:id/upstreams` | viewer |
| `GET` | `/explorer/asset-graph` | viewer |
| `POST` | `/internal/explorer/models/:id/run` | S2S only |
| `POST` | `/internal/explorer/models/:id/run-failed` · `/downstream` | S2S only |
| `POST` | `/internal/explorer/freshness/sweep` | S2S only |

---

## 13. Known gaps

Open items — none are regressions, all are known:

| ID | Gap | Est. |
|---|---|---|
| **DX-LimitDowngrade** | An explicit `LIMIT` above the cap is silently downgraded with no notice (§3) | ~2h |
| **DX-SqlGenResilience** | No timeout/backoff on NL→SQL; a transient 429 or cold start hangs the spinner (§4) | ~half-day |
| **DX-CornerCaseCoverage** | SQL re-qualification regex never driven live (§6) | ~half-day |

**Closed in-repo, not yet deployed.** `DX-VersionRace` and `DX-VersionRetention` are
implemented and passing locally but have not run on prod, so treat them as fixed in `main` and
unproven in production until PRODUCT_STATUS says otherwise. Concurrent edits are now refused
with a 409 rather than a 500, and version history has an opt-in per-workspace retention policy
that defaults to keep-forever — design and the trap to avoid are in
[§6](saved-queries-and-models.md#6-version-history-diff-and-restore).

Deliberately not built — recorded so they aren't re-litigated as oversights:
`incremental` materialization, a stored dependency graph, an LLM fallback for table
extraction, two triggers on one model, and enforced second-person approval. Reasons in the
[deep dive's §14](saved-queries-and-models.md#14-deliberately-not-built).
