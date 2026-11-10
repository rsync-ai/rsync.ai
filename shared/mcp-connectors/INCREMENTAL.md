# Incremental sync — connector contract

Every source connector in this repo follows the same incremental-sync
contract. This file is the single source of truth: implement the four
pieces below and your connector will automatically resume from a
watermark on every scheduled re-run, instead of re-fetching the entire
source on every run.

The reference implementation lives in
`public/shopify-admin-graphql/versions/v1.0.0/connector.py`. New
connectors typically need 30–50 lines of code.

## Why this matters

Without this contract, a scheduled pipeline does a FULL refetch of all
source data on every run. At 1M-orders + hourly schedule that's 24M API
calls / day. With this contract it's ~one round trip per run after the
initial backfill.

## The contract — five pieces

### 1. Executor → connector (export params)

The orchestrator calls `export(params)` once per page per run. It may
include these incremental-sync params (all optional):

| Param              | Type                | Meaning                                                                 |
| ------------------ | ------------------- | ----------------------------------------------------------------------- |
| `since`            | ISO-8601 string     | Resume marker. Connector must filter rows updated AFTER this time.      |
| `updated_since`    | ISO-8601 string     | Alias for `since`. Same value as `since`. Read whichever you prefer.    |
| `modified_since`   | ISO-8601 string     | Alias for `since`. (Cloud-storage-style naming.)                        |
| `modified_after`   | ISO-8601 string     | Alias for `since`. (Some SaaS APIs use this spelling.)                  |
| `cursor`           | opaque              | Existing pagination resumption token. Untouched by incremental.         |
| `incremental_field`| string (optional)   | Hint from the user; connector picks its own default when absent.        |
| `since_cursor`     | opaque (optional)   | The PK high-water mark as of the **start of the delta window**. DB sources only — see §5. Do NOT confuse with `cursor`. |
| `incremental_lag_seconds` | number (optional) | DB sources only: how far behind the database clock a sweep's watermark is held (default 5; `0` turns the hold off). Also read from the connection config. See §5. |

The executor emits all four `since` aliases with the same value
(see `exportReq.Params["since"]` in `executeBatchDataTransfer`,
[backend-orchestrator/internal/agents/executor/executor.go](../../backend-orchestrator/internal/agents/executor/executor.go)).
Pick the one whose name matches your source's native API and ignore the
others.

### 2. Connector → executor (export response)

| Field            | Type                  | Meaning                                                            |
| ---------------- | --------------------- | ------------------------------------------------------------------ |
| `success`        | bool                  | Existing.                                                          |
| `data`           | list of records       | Existing.                                                          |
| `columns`        | list of strings       | Existing.                                                          |
| `row_count`      | int                   | Existing.                                                          |
| `has_more`       | bool                  | Existing.                                                          |
| `next_cursor`    | opaque (optional)     | Existing pagination cursor. A DB source sends a scalar for a one-column key, a list for a composite key (with `cursor_columns`), or `{"locator": …}` for a row address (§6). |
| `paging_mode`    | string (optional)     | `keyset`, `locator` or `offset`. `offset` tells the executor to step the offset and drop any cursor it held. |
| **`stats.watermark`** | `{field, value[, snapshot]}` | **NEW.** `field` is a stable string (e.g. `"last_modified"`). `value` is the max of the row-updated-at column observed across this batch, ISO-8601. A DB source's first page of a sweep reports the table's max read *before* the page instead, with `snapshot: true` (§5). |
| `max_watermark`  | ISO-8601 (optional)   | Same as `stats.watermark.value`, surfaced flat for client tools/tests. The executor only reads `stats.watermark`. |

The executor folds each page's `stats.watermark` into the one it keeps
for the sweep (`foldSweepWatermark`,
[batch_sweep_watermark.go](../../backend-orchestrator/internal/agents/executor/batch_sweep_watermark.go)):
a `snapshot` is kept for the whole sweep, anything else folds to the max
over the pages and never below the `since` the sweep started with. It
writes the kept value to `pipeline_checkpoints.position.watermark` along
with a derived `mode` (`checkpointPosition["watermark"]` in
[`executeBatchDataTransfer`](../../backend-orchestrator/internal/agents/executor/executor.go)).
On the next run the per-table checkpoint READ translates that mode +
watermark.value into the `incrementalSince` variable, which becomes
the `since` param.

> **Emit the watermark even when `since` is absent.** This is not optional
> and it is the single most important line in this document. See §5 — a
> connector that emits `stats.watermark` only when `since` is present can
> never bootstrap, because the executor only sends `since` once a watermark
> is already in the checkpoint. Every DB source shipped with that deadlock
> until #724.

### 3. Edge-case behavior the connector MUST handle

| Case                                                | Required behavior                                                                                                              |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `since` absent / empty                              | Full fetch — **but still emit `stats.watermark` if the incremental field exists on the table** (§5). Filtering is conditional on `since`; *reporting* is not. |
| `since` malformed (not ISO-8601)                    | Log a warning, treat as if absent (full fetch). Never crash.                                                                   |
| 0 rows returned & `since` was present               | Return `stats.watermark.value = since`. (Don't clear the checkpoint just because nothing changed.)                             |
| 0 rows returned & no `since`                        | Omit `stats.watermark` entirely. Lets the executor keep its current checkpoint state.                                          |
| Multi-page batch within one export() call           | `value` must be the max across ALL pages, not just the last.                                                                   |
| DB source, first page of a sweep (no `cursor`, offset 0) | Read `max(<field>)` (above `since` when given) **before** the page and report it with `snapshot: true` (§5). If that read fails, report the page max as usual. |
| Resource has no updated-at concept (e.g. a singleton) | Omit `stats.watermark`. Executor preserves prior checkpoint state for this resource.                                           |
| `variables.query` (or equivalent) already set       | Combine with AND. Don't overwrite the user's existing filter.                                                                  |

### 4. Mode wiring (no connector code, executor handles it)

The executor uses `watermark.field` to choose a checkpoint `mode`:

| `watermark.field`   | Resulting mode        | Replay path             |
| ------------------- | --------------------- | ----------------------- |
| `"last_modified"`   | `cloud_incremental`   | `position.modified_since` → `since` |
| anything else       | `db_incremental`      | `position.watermark.value` → `since` |

For pure-API sources (Shopify, Stripe, HubSpot, …) use `"last_modified"`.
For relational sources (Postgres, MySQL) use the actual column name
(`"updated_at"`, `"updated_at_ts"`, etc.).

### 5. The delta predicate (DB sources) — three cursors, and why

Relational sources page by primary key *and* filter by watermark, so they
juggle three separate cursor values. Confusing them is what broke
incremental for every DB source until
#724.

| Value            | Lives in                          | Means                                                                 |
| ---------------- | --------------------------------- | ---------------------------------------------------------------------- |
| `cursor`         | export param, per **page**        | This run's paging position. Advances within a single sweep and resets when the sweep completes. |
| `since_cursor`   | export param + `position.since_cursor` | The PK high-water mark **as of the start of the delta window** — i.e. the highest PK the previous completed sweep saw. |
| `pk_high_water`  | `position.pk_high_water`          | Monotonic max PK ever observed, carried across runs (`maxCursorValue`). |

The predicate a DB connector must build is:

```sql
WHERE pk > :cursor                              -- intra-run paging
  AND ( updated_at > :since OR pk > :since_cursor )   -- the delta window
```

**Why the `OR`, and why the PK floor is not AND-ed onto the whole
predicate.** The old code AND-ed the keyset cursor across the entire
condition. That is correct for paging and fatal for deltas: an UPDATE does
not change its row's primary key, so an updated row sits *below* the PK
floor and is filtered out before the watermark ever gets to match it. The
result was an append-only pipeline that looked like it worked — new rows
landed, updates silently never did. The `OR` lets a row qualify on
*either* leg: changed since the watermark, or newer than anything the last
sweep saw.

**`table_complete`** (`position.table_complete`) is written by the
checkpoint save as `sourceRowCount < exportBatchSize` — a short page means
the sweep reached the end of the table, so the next run starts a fresh
delta window instead of resuming mid-sweep. Checkpoints written before
#724 have no such field;
the read path treats a missing `table_complete` as "unknown" and the value
self-heals on the first full sweep after deploy. No migration, no backfill.

**Bootstrapping.** The connector must emit `stats.watermark` whenever the
incremental field **exists on the table**, regardless of whether `since`
was sent. The two rules are independent:

- *filter* on `since` → only when `since` is present;
- *report* `stats.watermark` → whenever the column exists.

A table with no `updated_at` still emits nothing and correctly stays on
the keyset path. Executor side, see `executeBatchDataTransfer` in
[executor.go](../../backend-orchestrator/internal/agents/executor/executor.go)
(the checkpoint read that sets `incrementalSince`, and the checkpoint
write at `checkpointPosition["watermark"]`). Reference implementation:
`export()` in [postgresql `connector.py`](public/postgresql/versions/v1.0.0/connector.py)
(the `delta_parts` predicate and the `watermark_col` emission). Tests:
`tests/test_db_incremental_delta_predicate.py`.

**The sweep's watermark is read before its first page.** A sweep pages by
key, but `since` is a timestamp, and the two are not ordered together. If
row 1 is read on page 1 and updated at 10:04 while the sweep is still
reading, and page 3 then reads a row updated at 10:05, a watermark of
10:05 puts row 1's update below the next sweep's `since`, and it is lost
for good (KI-DB-BATCH-PAGING-CAN-SKIP-ROWS, cause 3). So on a sweep's
first page (no `cursor`, offset 0) the database connectors read
`MAX(<field>)` (above `since`, when given) **before** the page query and
report that as the watermark with `snapshot: true`. Every row updated
after the read is above it, whatever its key; the executor keeps the
snapshot for the whole sweep, including a half resumed from a checkpoint
(`seedSweepWatermark`).

- **Late commits.** A transaction can stamp `updated_at` before the read
  and commit after it. When the snapshot is within
  `incremental_lag_seconds` (default 5) of the database clock, it is held
  back to `now - lag`: the same query reads the server's UTC and session
  clocks, and aware values are compared with the first, naive ones with
  the second. A transaction that commits more than the lag after it
  stamped its row can still be missed.
- **Duplicates, not gaps.** Rows updated during a sweep are read again by
  the next one, and so are rows inside the lag window. Delivery is
  at-least-once; destinations that upsert by key absorb it. An idle table
  still converges to empty sweeps.
- **Cost.** One `MAX()` per sweep, over the rows above `since` (an index
  on the field makes it a lookup), plus a `WHERE 1 = 0` probe for the
  column's real spelling.
- **Fallback.** If either read fails, the connector rolls the failed
  statement back and reports the page max. The executor then folds page
  maxima and never moves the watermark backwards, which is what it did
  before the snapshot existed.

The block that does this (`_watermark_snapshot`, `_hold_back_watermark`,
`_incremental_lag_seconds`) sits between `# BEGIN/END watermark snapshot
block` markers, identical in `connector_database.py.j2` and the
PostgreSQL, MySQL, Oracle and SQL Server connectors. Tests:
`tests/test_db_watermark_snapshot.py`.

### 6. Paging a DB sweep — a stable order, one shared module

Each page is its own query on its own connection, so nothing holds a
snapshot between pages. A page therefore starts where the last one ended
only if the order is total and survives writes between the queries.
[`public/sql_paging.py`](public/sql_paging.py) picks the most stable
order a table offers, in this order:

1. a caller-named `cursor_column`;
2. the primary key — every column of it, compared as a row value on
   PostgreSQL and expanded (`a >= ? AND (a > ? OR b > ?)`, index-friendly) elsewhere; the
   same comparison is the `pk > :since_cursor` leg of §5;
3. the narrowest unique index whose columns are all NOT NULL;
4. the row's address: PostgreSQL `ctid` on a plain table, Oracle `ROWID`
   on a heap table, SQLite `rowid`;
5. `ORDER BY` every orderable column, with OFFSET — a row deleted
   mid-sweep can shift one row past a page boundary;
6. plain OFFSET, the old behaviour, when nothing above can be read.

Catalog reads come from views a SELECT-only user can see. A failed read
is rolled back and the next step is tried; it never fails the export.
With no key (steps 4-6) there is no `since_cursor` to place, so the §5
delta is dropped and the sweep re-reads the table: extra rows are
recoverable, missing ones are not.

**Adding a database.** The `connector_database.py.j2` template and the
PostgreSQL, MySQL, Oracle and SQL Server connectors make the same
`read_page` call and choose a dialect by the connection's driver module
(`dialect_for`). A driver the
module does not know gets `GENERIC`, which pages exactly as exports did
before. Stable paging for a new database is one call in `sql_paging.py`:

```python
register(Dialect(name="mydb", quote='"', paramstyle="qmark",
                 page_syntax="limit", discover=_discover_mydb), "mydb_driver")
```

`_discover_mydb(ctx)` returns a plan or `None`; usually it is
`ctx.first(ctx.nominated_plan, <key query>, <ordered columns>)`, and
`ctx.first` rolls back a step that raises and tries the next. The module
docstring has the details; `_discover_sqlserver` is a short example. The
connector's Dockerfile ships the module beside the connector
(`COPY --from=shared sql_paging.py /app/sql_paging.py`); a generated
connector's Dockerfile already does. Tests: `tests/test_batch_paging_stable_order.py`.

**Warehouses.** Snowflake, Databricks, Redshift, ClickHouse and BigQuery
enforce no primary or unique key, so their dialects trust none: a sweep
takes step 5 above, ordered by the named `cursor_column` first, then (on
ClickHouse) the table's sorting key when it is plain columns, then every
column the engine can order. Where the engine can read a table as of a
past point, the sweep's first page takes that point and every later page
reads at it, so the offsets count rows of one fixed table:

| Engine | Point | Page source |
|---|---|---|
| Snowflake | `CURRENT_TIMESTAMP()` in epoch ms | `AT(TIMESTAMP => TO_TIMESTAMP_LTZ(<ms>, 3))` |
| Databricks | latest Delta version (`DESCRIBE HISTORY`) | `VERSION AS OF <v>` |
| BigQuery | `CURRENT_TIMESTAMP()` in epoch ms | `FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS(<ms>)` |

Those pages report `paging_mode: "snapshot"` and, on a full page,
`next_cursor` `{"snapshot": <point>, "offset": <n>}`, which the executor
hands back as `cursor` and never takes for a key high-water. A point the
engine refuses (a view, an external table, past retention) is dropped:
on the first page the sweep steps OFFSET as `paging_mode: "offset"`; on a
later page it goes on unpinned from the cursor's offset. Redshift and
ClickHouse have no point to take, so a row deleted mid-sweep can shift
one row past a page boundary. A cursor of any other shape re-reads the
sweep from the top: rows twice, never none. Every page is a full sort,
and none of these exports reads `since` yet, so every run re-reads the
table. BigQuery and ClickHouse clients are not DB-API; `QueryCursor`
wraps a `run(sql, params)` function so they page through the same
`export_page`. Tests: `tests/test_warehouse_batch_paging.py`.

## Per-source filter-syntax reference

Each connector has to translate the abstract `since` value into the
filter dialect of the underlying API. Use this table when adding a new
source:

| Source            | Filter syntax                                                                                       | Example                                                          |
| ----------------- | --------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| Shopify GraphQL   | `query: "updated_at:>YYYY-MM-DDTHH:MM:SSZ"`                                                         | `updated_at:>2026-05-22T16:54:36Z`                               |
| Stripe REST       | `created[gt]=<epoch>` (Stripe uses `created`; only `Event` exposes `updated`)                       | `created[gt]=1716397476`                                         |
| HubSpot REST v3   | `hs_lastmodifieddate` property + `filterGroups[].filters[]` (POST `/crm/v3/objects/{type}/search`)  | `{ propertyName: "hs_lastmodifieddate", operator: "GTE", value: "1716397476000" }` |
| Salesforce REST   | SOQL `WHERE LastModifiedDate >= 2026-05-22T16:54:36Z`                                               | as shown                                                         |
| Postgres / MySQL  | `WHERE <updated_at_col> > $1` (parametric, type-aware bind)                                         | `WHERE updated_at > '2026-05-22 16:54:36'`                       |
| Notion REST       | Database query `filter: { property: "Last edited time", date: { after: <iso> } }`                   | as shown                                                         |
| Github GraphQL v4 | `search(query: "updated:>2026-05-22T16:54:36Z type:issue ...")`                                     | as shown                                                         |
| Linear GraphQL    | `issues(filter: { updatedAt: { gt: "2026-05-22T16:54:36Z" } })`                                     | as shown                                                         |
| Slack Web API     | `oldest=<epoch>` on `conversations.history` / `conversations.replies`                               | `oldest=1716397476.000000`                                       |
| Pipedrive REST    | `since_timestamp=<unix>` query param on `deals/leads/persons`                                       | as shown                                                         |

## Per-source adoption status

> **Versions in this column are `latest.json.current_version`** — the dir the
> Docker build context actually points at, *not* the highest `versions/`
> directory. This table drifted for weeks claiming `postgresql v1.0.14` while
> the code that ran was `v1.0.0`; check `latest.json` before editing a row.

| Source                       | Implemented in | Notes                                                                                |
| ---------------------------- | -------------- | ------------------------------------------------------------------------------------ |
| `shopify-admin-graphql`      | v1.0.0         | Reference impl. `products` / `orders` / `customers` incremental. `inventory_items` and `collections` are full-refetch carve-outs (Shopify Admin 2024-10 GraphQL schema doesn't expose a `query` arg on those fields). `shop` is a singleton (no watermark). |
| `postgresql`                 | v1.0.0         | DB watermark via `WHERE <incremental_field> > $1` (default field `updated_at`), combined with the keyset cursor per the **§5 delta predicate** (`pk > cursor AND (updated_at > since OR pk > since_cursor)`) — *not* a flat AND, which is what made it append-only. Returns `stats.watermark = {field, value}` with `db_incremental` mode, emitted **whenever the field exists on the table**, `since` present or not. Edge cases per §3 handled: 0 rows + since echoes prior watermark; malformed since logs warning and falls back to full fetch. |
| `mysql`                      | v1.0.0         | Same shape as Postgres, including the §5 delta predicate and unconditional watermark emission. Mirrors `_normalize_since` + `_max_field_value` helpers verbatim so MySQL→PG round-trip preserves watermark format. |
| `oracle`                     | v1.0.0         | Same shape as Postgres. Bind syntax `:1`-style; `ROWNUM`/`OFFSET … FETCH` paging.                                                                                          |
| `sqlserver`                  | v1.0.0         | Same shape as Postgres. Note the server class in `connector.py` is (historically) named `MysqlMCPServer` — that is a copy-paste artifact, not a wiring error. |
| `stripe` (dir is `public/stripe`, **not** `stripe-rest`) | v1.0.0 (partial, **one claim unverified**) | `customers/charges/invoices/payment_intents` carry `supports_incremental: True` and `incremental_param: "created[gte]"`; `subscriptions/products/prices/payment_methods` are honestly `False` until Stripe's `Event` polling path lands (4 True / 4 False, `connector.py:534-695`). **Caveat:** `export()` forwards the executor's ISO-8601 `since` to that param verbatim (`connector.py:1407-1409`) and there is no epoch conversion anywhere in the file — Stripe's `created[gte]` takes **epoch seconds**, so the filter is likely ignored. An earlier revision of this table claimed a `_to_stripe_epoch` helper did the conversion; **no such helper exists.** Not fixed in #724 — proving it needs a live Stripe key. |
| `nse-india`                  | v1.0.0 (run locally on real NSE, 2026-11-04) | 7 walks: six by day on `trade_date` (`bhavcopy_daily`, `bhavcopy_history`, `delivery_daily`, `index_daily`, `participant_oi_daily`, `participant_vol_daily`) and `corporate_actions` by year on `ex_date` (the current year is read again each run, so its watermark stops at the last finished year). The 5 snapshot tables are a full refresh each run. One file per calendar day, so `since` is a date: the walk resumes the day after it; with no `since`, or one that is not a date, it starts at the table's first day (`connector.py:1002`). A call that runs out of time under the shared ~1 req/s pace returns an empty page and the table resumes next run, so a first backfill takes many runs (each one is cut at the executor's 30-minute wait and marked failed; the checkpoints keep what it loaded). The connection has no settings. A page holds at most 5 days that have a file (30 for the small files) and skips days without one, because the executor ends a table at the first page with no rows. A day whose file repeats an earlier day's data counts as a day with no file. `stats.watermark` is the last day emitted **in full** (a day split across pages counts once its last row is out; a day with no file never counts), floored at `since` so an empty page echoes it (`connector.py:1517`). Pages by `next_cursor` = `YYYY-MM-DD:<rows already sent from that day>` (`connector.py:1514`); an `offset` without a cursor is refused (`connector.py:1432`). |
| `upstox`                     | v1.0.0 (offline fake only, **not run on Upstox**) | `historical_candle` only (`incremental_field: timestamp`): the path's two dates are the window — `from_date` = the run's `since`, `to_date` = today (UTC); with no `since`, the last 365 days (`base_connector.py date_range_params`). A `from_date` in the connection config never moves it. One window per run, no chunking. `user_profile` and `market_holidays` are a full refresh each run. |
| `hubspot-rest-v3`            | v1.0.0 — **runtime-generated, not in this repo** | No `hubspot` directory exists under `shared/mcp-connectors/`; the only artifact is the prod image `mcp-hubspot-rest-v3:v1.0.0` (see BACKLOG `F-RUNTIMEGEN-HARDEN`). The previously-recorded behavior — all resources `supports_incremental: False` because HubSpot's GET listing endpoints silently ignore `updated_since`, with real incremental needing `POST /crm/v3/objects/{type}/search` + `filterGroups[].filters[]` (`hs_lastmodifieddate`, `GTE`, `<epoch_ms>`) — **cannot be verified from this tree.** Treat it as a note, not a citation. |
| `salesforce-rest`            | pending (no connector yet) | SOQL `WHERE LastModifiedDate >= …`.                                      |
| `notion-rest`                | pending        | Database query filter on "Last edited time".                                         |
| `github-graphql-v4`          | pending        | Search query `updated:>…`.                                                           |
| `linear-graphql`             | pending        | `issues(filter: { updatedAt: { gt: … } })`.                                          |
| `slack-web-api`              | pending        | `oldest` Unix-epoch parameter on `conversations.history`.                            |
| `pipedrive`                  | pending        | `since_timestamp` query parameter.                                                   |

## Shopify-specific carve-outs (reference impl)

Reading the reference impl is the fastest way to internalize the
contract. Key files:

- `public/shopify-admin-graphql/versions/v1.0.0/connector.py` — module-
  level docstring describes the contract; `export()` reads `since`,
  composes a `query` filter, returns `stats.watermark`.
- `_INCREMENTAL_CAPABLE` near the top maps resource → has-incremental-
  filter? to gracefully degrade resources Shopify doesn't expose a
  `query` arg for.
- `_normalize_iso_timestamp()` accepts any reasonable ISO-8601 shape
  and re-emits the Shopify-search-string-safe form.
- `_compose_query_filter()` AND-combines an existing user filter with
  the `updated_at:>…` predicate.
- `_max_updated_at()` scans the rows for the running max `updatedAt`,
  used to populate `stats.watermark.value`.

### Three quirks worth knowing about Shopify

1. **`updated_at:>` filter granularity is one DAY, not one second.**
   Verified against `development-store-keujtwka` 2026-05-22 across
   `orders` / `products` / `customers`: the time component
   (`THH:MM:SSZ`) is silently truncated by Shopify's search index, so
   `updated_at:>2026-05-14T08:36:51Z` and `updated_at:>2026-05-14`
   return the IDENTICAL row set (every row whose calendar day ≥
   2026-05-14, regardless of time-of-day). In steady state this means
   each scheduled run re-fetches every row touched on the same calendar
   day as the watermark. For most stores that's a small, bounded set
   (a few dozen rows) and the incremental savings vs full-refetch are
   still 1–3 orders of magnitude on real data. On upsert sinks this is
   a no-op overhead. **Append-only sinks (raw S3/Parquet) would
   silently re-write the same-day rows on every run** — de-dup on PK
   in the next layer, or advance the persisted watermark to
   `<day>+1` when targeting append-only storage. A future PR can move
   incremental sync onto Shopify's webhook/Event stream to recover
   second-level granularity.

2. **`updated_at:>` is inclusive on the day boundary.** A consequence
   of (1): `updated_at:>2026-05-22` returns rows from 2026-05-22, not
   from 2026-05-23. Plan watermark math accordingly — never expect
   strict `>` semantics from Shopify's search.

3. **Multi-page max-watermark requires `sortKey: UPDATED_AT`.** Shopify
   default-sorts by `id`. Without an explicit `sortKey`, the last page's
   `max(updatedAt)` is NOT the global max — saving it as the checkpoint
   would cause the next run to refetch rows that earlier pages already
   covered. The reference impl pins `sortKey: UPDATED_AT, reverse: false`
   on `products` / `orders` / `customers` queries (the three
   incremental-capable resources) so the LAST page's max IS the global
   max. Future connectors built on order-unstable APIs MUST do the
   same — either sort by the watermark field server-side or have the
   executor max across pages client-side.

### Bulk Operations and incremental

Shopify's Bulk Operations API (`bulkOperationRunQuery`) requires the
inner query's top-level connection field have NO arguments — including
`query`. So the bulk-export path in this connector always does a full
refetch regardless of `since`. For very large stores (1M+ rows) with
hourly schedules this is still an order-of-magnitude better than
paginated GraphQL, but it does NOT respect the watermark. A future PR
can move bulk + incremental onto Shopify's webhook/Event API (which
streams updates rather than polling).

## Adoption checklist for new connectors

When adding incremental support to a new source connector:

1. Read `since` / `updated_since` / `modified_since` / `modified_after`
   from `params` (any one is fine — they carry the same value).
2. Translate the value into your source's native filter syntax (see the
   per-source table above).
3. Iterate over rows and track the max value of the source's update-
   timestamp column.
4. Add `stats.watermark = {"field": "last_modified", "value": <max>}`
   to the export response. (Use a different `field` only if you want
   `db_incremental` mode — see the mode-wiring table above.)
5. Handle the five edge cases in the table above (especially the
   zero-rows-but-have-a-since case, which is non-obvious).
6. Add a row to the "Per-source adoption status" table in this file.
7. Verify against a real instance: full fetch first run, then 0 rows
   on a second immediate run, then 1 row after creating a new record.

The reference impl in `public/shopify-admin-graphql/versions/v1.0.0/connector.py`
shows the pattern in ~60 lines (the `_incremental` helpers plus the
~30 lines of glue in `export()`).
