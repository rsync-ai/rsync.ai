#!/usr/bin/env python3
"""Offline unit tests for the BigQuery warehouse adapter's OPTIMIZED paths.

Two optimizations are pinned here (both run with stdlib only — no GCP, no
google-cloud-bigquery, via the fakes in ``_bq_fakes``):

  READ  — ``export`` must paginate. Keyset mode (``cursor_column``) emits a
          ``WHERE col > @cursor ORDER BY col LIMIT n`` query and returns
          ``next_cursor`` / ``has_more`` so the orchestrator resumes to EOF
          instead of capping at ``LIMIT``. Offset mode is the fallback.

  WRITE — the destination bulk path is flag-gated (``RSYNC_BQ_LOAD_JOB`` /
          ``bulk_load`` param). When enabled it uses a BigQuery load job;
          when a load job fails it FALLS BACK to streaming inserts (never a
          silent drop), reporting the method actually used.

Run standalone (no pytest needed):
    python3 test_bigquery_adapter.py
Or under pytest in the connector image:
    pytest test_bigquery_adapter.py -q
"""
import contextlib
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)  # for `import _bq_fakes`
# Walk up to locate warehouse_adapters.py (lives in shared/mcp-connectors/public).
_d = _HERE
for _ in range(8):
    if os.path.exists(os.path.join(_d, "warehouse_adapters.py")):
        sys.path.insert(0, _d)
        break
    _d = os.path.dirname(_d)

import warehouse_adapters  # noqa: E402
import _bq_fakes  # noqa: E402

CONFIG = {"project_id": "proj", "dataset_id": "ds"}


@contextlib.contextmanager
def env(**kw):
    saved = {k: os.environ.get(k) for k in kw}
    try:
        for k, v in kw.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        yield
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


# ============================ READ: export pagination ========================

def test_export_keyset_returns_next_cursor_and_has_more():
    """A full page in keyset mode yields next_cursor (last row's key) + has_more."""
    rows = [{"id": 1, "v": "a"}, {"id": 2, "v": "b"}, {"id": 3, "v": "c"}]
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows)
    out = adapter.export(CONFIG, {"table": "t", "cursor_column": "id", "limit": 3})
    assert out["success"] is True, out
    assert out["paging_mode"] == "keyset", out
    assert out["next_cursor"] == 3, out            # last row's id
    assert out["has_more"] is True, out            # full page => maybe more
    sql = client.queries[-1]
    assert "ORDER BY" in sql and "`id`" in sql, sql
    assert "LIMIT 3" in sql, sql


def test_export_keyset_continuation_filters_on_cursor_value():
    """Passing cursor_value builds a WHERE col > @cursor predicate (parameterized)."""
    rows = [{"id": 5}]
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows)
    out = adapter.export(CONFIG, {"table": "t", "cursor_column": "id",
                                  "cursor_value": 4, "limit": 10})
    assert out["success"] is True, out
    sql = client.queries[-1]
    assert "WHERE" in sql and ">" in sql, sql
    cfg = client.query_configs[-1]
    params = getattr(cfg, "query_parameters", [])
    assert params and params[0].value == 4, (sql, params)


def test_export_keyset_terminal_page_stops():
    """A short page (fewer than limit) means EOF: has_more False. The last key
    still comes back as next_cursor, the high-water mark of the sweep."""
    rows = [{"id": 1}, {"id": 2}]
    adapter, _ = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows)
    out = adapter.export(CONFIG, {"table": "t", "cursor_column": "id", "limit": 100})
    assert out["has_more"] is False, out
    assert out["next_cursor"] == 2, out
    assert out["total_records"] == 2, out


def test_export_offset_fallback_without_cursor_column():
    """No stable paging asked for => plain offset mode, and no next_cursor, so
    the executor steps the offset itself."""
    rows = [{"a": 1}, {"a": 2}]
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows)
    out = adapter.export(CONFIG, {"table": "t", "limit": 2, "offset": 4})
    assert out["paging_mode"] == "offset", out
    assert out["has_more"] is True, out            # full page
    assert out["next_cursor"] is None, out
    assert client.queries[-1] == "SELECT * FROM `proj.ds.t` LIMIT 2 OFFSET 4", client.queries


F = _bq_fakes._SchemaField
# proj.ds.t: a REPEATED field, a RECORD and a GEOGRAPHY cannot be ordered by.
BQ_TABLES = {"proj.ds.t": _bq_fakes._Table("proj.ds.t", schema=[
    F("id", "INTEGER", "REQUIRED"),
    F("tags", "STRING", "REPEATED"),
    F("addr", "RECORD", "NULLABLE"),
    F("name", "STRING", "NULLABLE"),
    F("area", "GEOGRAPHY", "NULLABLE"),
])}
BQ_NOW = ("UNIX_MILLIS", [{"f0_": 1700000000000}])
BQ_PIN = "FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS(1700000000000)"


def test_export_sweep_orders_every_column_at_one_snapshot():
    """KI-WAREHOUSE-BATCH-PAGING-UNORDERED: the executor asks for stable paging
    and never names a column. BigQuery enforces no key, so the page orders by
    every orderable column, pinned to the point in time the sweep started."""
    rows = [{"id": 1, "name": "a"}, {"id": 2, "name": "b"}, {"id": 3, "name": "c"}]
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows,
                                             tables=BQ_TABLES, answers=[BQ_NOW])
    out = adapter.export(CONFIG, {"table": "t", "use_keyset_paging": True, "limit": 3})
    assert out["success"] is True, out
    assert out["paging_mode"] == "snapshot", out
    assert out["next_cursor"] == {"snapshot": 1700000000000, "offset": 3}, out
    assert out["has_more"] is True and out["columns"] == ["id", "name"], out
    assert client.queries[-1] == f"SELECT * FROM `proj.ds.t` {BQ_PIN} ORDER BY `id`, `name` LIMIT 3 OFFSET 0", client.queries


def test_export_sweep_continues_at_the_cursors_snapshot():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=[{"id": 4}],
                                             tables=BQ_TABLES, answers=[BQ_NOW])
    out = adapter.export(CONFIG, {"table": "t", "use_keyset_paging": True, "limit": 3,
                                  "cursor": {"snapshot": 1700000000000, "offset": 3}})
    assert out["success"] is True and out["has_more"] is False, out
    assert not any("UNIX_MILLIS" in q for q in client.queries), client.queries
    assert client.queries[-1] == f"SELECT * FROM `proj.ds.t` {BQ_PIN} ORDER BY `id`, `name` LIMIT 3 OFFSET 3", client.queries


def test_export_sweep_that_cannot_time_travel_steps_the_offset():
    # A view or an external table refuses FOR SYSTEM_TIME AS OF: the page is
    # read unpinned and the executor steps the offset.
    rows = [{"id": 1, "name": "a"}]
    adapter, client = _bq_fakes.make_adapter(
        warehouse_adapters, query_rows=rows, tables=BQ_TABLES,
        answers=[BQ_NOW, ("FOR SYSTEM_TIME", RuntimeError("400 time travel is not supported on views"))])
    out = adapter.export(CONFIG, {"table": "t", "cursor_column": "name", "limit": 1})
    assert out["success"] is True, out
    assert out["paging_mode"] == "offset" and out["next_cursor"] is None, out
    assert client.queries[-1] == "SELECT * FROM `proj.ds.t` ORDER BY `name`, `id` LIMIT 1 OFFSET 0", client.queries


# ============================ WRITE: bulk load gating ========================

def _rows(n):
    return [{"id": i, "v": f"r{i}"} for i in range(n)]


def test_load_flag_off_uses_streaming_even_for_large_batch():
    """Default (flag off): even a large batch streams — no load job attempted."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    with env(RSYNC_BQ_LOAD_JOB=None):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(50),
                                    "staging_threshold_rows": 10})
    assert out["success"] is True, out
    assert out["method"] == "streaming", out
    assert client.loaded == [], "no load job should run when flag is off"
    assert len(client.inserted) == 1, client.inserted


def test_load_flag_on_large_batch_uses_load_job():
    """Flag on + batch over threshold => bulk NDJSON load job."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    with env(RSYNC_BQ_LOAD_JOB="true"):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(50),
                                    "staging_threshold_rows": 10})
    assert out["success"] is True, out
    assert out["method"] == "load_job", out
    assert len(client.loaded) == 1, client.loaded
    assert out.get("fell_back") is False, out


def test_load_bulk_param_forces_load_job_even_small_batch():
    """bulk_load=True forces the load job regardless of size/env flag."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    with env(RSYNC_BQ_LOAD_JOB=None):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(2), "bulk_load": True})
    assert out["method"] == "load_job", out
    assert len(client.loaded) == 1, client.loaded


def test_load_job_failure_falls_back_to_streaming():
    """A failing load job must fall back to streaming inserts (never silent drop)."""
    boom = RuntimeError("load job blew up")
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, load_job_raises=boom)
    with env(RSYNC_BQ_LOAD_JOB="true"):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(50),
                                    "staging_threshold_rows": 10})
    assert out["success"] is True, out
    assert out["method"] == "streaming", out
    assert out["fell_back"] is True, out
    assert len(client.inserted) == 1, "rows must still land via streaming"
    assert out["rows_loaded"] == 50, out


def test_load_job_uses_readable_binary_file_handle_not_write_only():
    """Regression (found via LIVE BigQuery testing 2026-07-03): the load-job temp
    file must be opened in a READABLE binary mode. google-cloud-bigquery rejects
    a write-only ('wb') handle with 'Cannot upload files opened in text mode', so
    a 'wb' regression makes EVERY bulk load job throw and silently degrade to
    streaming — a dead-on-arrival bulk path (and a hard 403 on free-tier
    BigQuery, since streaming is billing-only). The fake now enforces that same
    contract, so this test fails if the handle regresses to write-only.
    """
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    with env(RSYNC_BQ_LOAD_JOB="true"):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(3), "bulk_load": True})
    assert out["method"] == "load_job", out            # NOT "streaming"
    assert out.get("fell_back") is False, out
    assert "load_job_error" not in out, out            # no swallowed load-job error
    assert len(client.loaded) == 1, client.loaded
    assert client.inserted == [], "must not fall back to streaming inserts"


# =================== hardening (cold-review findings) =======================

def test_export_rejects_unsafe_cursor_column():
    """#1 identifier injection: a cursor_column that is not a plain identifier
    must be rejected up front (never interpolated into the SQL)."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=[{"id": 1}])
    out = adapter.export(CONFIG, {"table": "t",
                                  "cursor_column": "id`; DROP TABLE x; --",
                                  "limit": 10})
    assert out["success"] is False, out
    assert "cursor" in out.get("error", "").lower(), out
    # the injected text must never have reached a query
    assert all("DROP TABLE" not in q for q in client.queries), client.queries


def test_export_date_cursor_uses_date_param_type():
    """#4 a plain datetime.date cursor must bind as DATE (not TIMESTAMP), else a
    live DATE-typed keyset column raises a type-mismatch."""
    import datetime as _dt
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_rows=[{"d": 1}])
    adapter.export(CONFIG, {"table": "t", "cursor_column": "d",
                            "cursor_value": _dt.date(2020, 1, 1), "limit": 10})
    p = client.query_configs[-1].query_parameters[0]
    assert p.type_ == "DATE", p.type_
    # a full datetime still binds as TIMESTAMP
    adapter.export(CONFIG, {"table": "t", "cursor_column": "d",
                            "cursor_value": _dt.datetime(2020, 1, 1, 3, 4, 5), "limit": 10})
    p2 = client.query_configs[-1].query_parameters[0]
    assert p2.type_ == "TIMESTAMP", p2.type_


def test_export_keyset_missing_cursor_in_rows_fails_loud():
    """#3 if a full page's rows lack the cursor column, next_cursor would be
    None while has_more is True — silent truncation/loop. Fail loud instead."""
    rows = [{"other": 1}, {"other": 2}]           # full page, no 'id' key
    adapter, _ = _bq_fakes.make_adapter(warehouse_adapters, query_rows=rows)
    out = adapter.export(CONFIG, {"table": "t", "cursor_column": "id", "limit": 2})
    assert out["success"] is False, out
    assert "paging key is missing" in out.get("error", ""), out


def test_load_job_result_raises_but_committed_no_double_write():
    """#5 if load_job.result() raises AFTER the job committed, must NOT fall back
    to streaming (that would double-write). Report load_job success."""
    boom = RuntimeError("client-side poll timeout")
    adapter, client = _bq_fakes.make_adapter(
        warehouse_adapters, load_job_raises=boom, load_job_committed=True)
    with env(RSYNC_BQ_LOAD_JOB="true"):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(50),
                                    "staging_threshold_rows": 10})
    assert out["success"] is True, out
    assert out["method"] == "load_job", out
    assert out["fell_back"] is False, out
    assert client.inserted == [], "must NOT double-write via streaming"
    assert out["rows_loaded"] == 50, out


def test_export_query_mode_flags_truncation():
    """#7 a verbatim query truncated at max_results must be flagged, not silent."""
    adapter, _ = _bq_fakes.make_adapter(warehouse_adapters,
                                        query_rows=[{"id": i} for i in range(5)])
    out = adapter.export(CONFIG, {"query": "SELECT * FROM t", "limit": 5})
    assert out["paging_mode"] == "query", out
    assert out.get("truncated") is True, out       # hit the cap => maybe more
    # a short read is not truncated
    adapter2, _ = _bq_fakes.make_adapter(warehouse_adapters,
                                         query_rows=[{"id": 1}])
    out2 = adapter2.export(CONFIG, {"query": "SELECT * FROM t", "limit": 5})
    assert out2.get("truncated") is False, out2


# Bug class: a verbatim read whose only check is the gateway's keyword gate, so
# BigQuery runs whatever the gate passes (KI-WAREHOUSE-READS-RELY-ON-THE-GATE).
# The SQL text below is a plain SELECT on purpose: it stands for a statement the
# gate misread, and only BigQuery's own parse (the dry run's statement type)
# decides whether a job runs.
_NOT_A_READ = ["INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE_TABLE",
               "CREATE_TABLE", "CREATE_TABLE_AS_SELECT", "DROP_TABLE", "ALTER_TABLE",
               "CREATE_FUNCTION", "CREATE_PROCEDURE", "CALL", "EXPORT_DATA", "LOAD_DATA",
               "SCRIPT", "", None]

# executeWithRetry (executor.go) re-issues a call whose error contains any of these.
_ORCHESTRATOR_RETRY_WORDS = ["timeout", "connection", "network", "rate limit",
                             "http 408", "http 429", "http 5"]


def test_export_query_runs_only_what_bigquery_parses_as_one_select():
    for statement_type in _NOT_A_READ:
        adapter, client = _bq_fakes.make_adapter(warehouse_adapters,
                                                 query_rows=[{"a": 1}],
                                                 statement_type=statement_type)
        out = adapter.export(CONFIG, {"query": "SELECT a FROM t", "limit": 5})
        assert out["success"] is False, (statement_type, out)
        assert client.dry_runs == ["SELECT a FROM t"], (statement_type, client.dry_runs)
        assert client.queries == [], f"{statement_type!r} ran as a read: {client.queries}"
        # The gateway's ClassifyExecutionError answers "runs read-only" with
        # permission_denied; the orchestrator must not re-issue it.
        err = out["error"].lower()
        assert "runs read-only" in err, out
        assert not [w for w in _ORCHESTRATOR_RETRY_WORDS if w in err], out


def test_export_query_reads_one_job_under_a_time_limit():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters,
                                             query_rows=[{"a": 1}, {"a": 2}],
                                             query_job_running_polls=3)
    out = adapter.export(CONFIG, {"query": "WITH s AS (SELECT 1 AS a) SELECT a FROM s",
                                  "limit": 5})
    assert out["success"] is True and out["data"] == [{"a": 1}, {"a": 2}], out
    sql = "WITH s AS (SELECT 1 AS a) SELECT a FROM s"
    assert client.dry_runs == [sql] and client.queries == [sql], (client.dry_runs, client.queries)
    limit_s = warehouse_adapters.BigQueryWarehouseAdapter.READ_TIMEOUT_SECONDS
    assert limit_s == 25, limit_s  # under the orchestrator's 30 s per-call cap
    job = client.jobs[0]
    # Waited on jobs.get polls with a pause between them; result() only fetches
    # the rows of the finished job, so it never waits.
    assert len(job.done_timeouts) == 4 and len(client.clock.pauses) == 3, (
        job.done_timeouts, client.clock.pauses)
    assert job.result_timeouts == [None], job.result_timeouts
    # BigQuery's own limit ends the job should the client's cancel not land.
    assert client.query_configs[0].extra.get("job_timeout_ms") == (limit_s + 5) * 1000
    assert client.query_kwargs[0].get("job_retry", "unset") is None, client.query_kwargs


# Bug class: a read wait whose limit the client library stretches. QueryJob.result
# (timeout=t) waits in getQueryResults calls BigQuery holds up to 10 s and checks t
# only between them, so a 25 s wait could end at 30 s or later, past the
# orchestrator's per-call cap. The fake job waits that way when result() is used.
def test_export_query_cancels_a_read_that_runs_out():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_job_running_polls=None)
    out = adapter.export(CONFIG, {"query": "SELECT a FROM t", "limit": 5})
    assert out["success"] is False, out
    assert client.jobs[0].cancelled is True, "a read that ran out must be cancelled"
    assert len(client.queries) == 1, client.queries
    limit_s = warehouse_adapters.BigQueryWarehouseAdapter.READ_TIMEOUT_SECONDS
    assert client.clock.now == limit_s, f"the read waited {client.clock.now}s, limit {limit_s}s"
    assert client.jobs[0].result_timeouts == [], "a running job is waited on with jobs.get"
    assert max(client.clock.pauses) <= 1.0, client.clock.pauses
    # The gateway classifies "cancel" as a timeout; the orchestrator must not
    # re-issue a read that would only run out again.
    err = out["error"].lower()
    assert "cancel" in err and "25" in err, out
    assert not [w for w in _ORCHESTRATOR_RETRY_WORDS if w in err], out


# ===================== destination namespace routing ========================
#
# The sink (kafka-sink-worker ``addNamespaceParam``) bares the target table and
# forwards the pipeline's ``destination_namespace`` separately, setting BOTH
# ``namespace`` and ``db_or_schema`` to the same value — so honouring either key
# alone is compliant. Empty and the literal "default" both mean "no real
# namespace" (the sink's ``isRealNamespace``). BigQuery's namespace analog is the
# DATASET. Before the fix this adapter read neither key, so every write resolved
# ``config["dataset_id"]`` and landed in the connection's dataset: rows "loaded",
# silently mis-routed, and invisible to any row count.

def test_namespace_param_routes_import_data_to_that_dataset():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.import_data(CONFIG, {"table": "t", "data": _rows(2),
                                       "namespace": "ns"})
    assert out["success"] is True, out
    assert [i["fq"] for i in client.inserted] == ["proj.ns.t"], client.inserted


def test_namespace_param_routes_load_to_that_dataset():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    with env(RSYNC_BQ_LOAD_JOB=None):
        out = adapter.load(CONFIG, {"table": "t", "data": _rows(2),
                                    "namespace": "ns"})
    assert out["success"] is True, out
    assert [i["fq"] for i in client.inserted] == ["proj.ns.t"], client.inserted


def test_namespace_param_routes_merge_to_that_dataset():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.merge(CONFIG, {"table": "t", "data": _rows(2),
                                 "key_fields": ["id"], "namespace": "ns"})
    assert out["success"] is True, out
    merges = [q for q in client.queries if q.lstrip().startswith("MERGE")]
    assert merges, client.queries
    assert "`proj.ns.t`" in merges[0], merges[0]
    assert "proj.ds.t" not in merges[0], merges[0]
    # the staging table follows the target dataset, not the connection's
    assert all(f["fq"].startswith("proj.ns._rsync_staging_") for f in client.loaded), \
        client.loaded


def test_db_or_schema_alone_is_honoured_like_namespace():
    """Either key alone is the whole contract — addNamespaceParam sets both."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.import_data(CONFIG, {"table": "t", "data": _rows(1),
                                       "db_or_schema": "ns2"})
    assert out["success"] is True, out
    assert [i["fq"] for i in client.inserted] == ["proj.ns2.t"], client.inserted


def test_empty_and_default_namespace_fall_back_to_the_connection_dataset():
    for ns in ("", "   ", "default", "DEFAULT"):
        adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
        out = adapter.import_data(CONFIG, {"table": "t", "data": _rows(1),
                                           "namespace": ns})
        assert out["success"] is True, (ns, out)
        assert [i["fq"] for i in client.inserted] == ["proj.ds.t"], (ns, client.inserted)


def test_an_already_qualified_table_wins_over_the_namespace():
    """A dataset- or fully-qualified table names its own dataset; the namespace
    must not override it (the sink bares the table so this cannot collide)."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    adapter.import_data(CONFIG, {"table": "other.t", "data": _rows(1),
                                 "namespace": "ns"})
    adapter.import_data(CONFIG, {"table": "p2.other.t", "data": _rows(1),
                                 "namespace": "ns"})
    assert [i["fq"] for i in client.inserted] == ["proj.other.t", "p2.other.t"], \
        client.inserted


def test_source_reads_ignore_a_destination_namespace():
    """``export`` is a SOURCE read — a destination namespace must never retarget
    it, or a pipeline writing to dataset X would start reading from X too."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters,
                                             query_rows=[{"id": 1}])
    out = adapter.export(CONFIG, {"table": "t", "limit": 10, "namespace": "ns"})
    assert out["success"] is True, out
    assert client.queries, "export must issue a query"
    assert any("proj.ds.t" in q for q in client.queries), client.queries
    assert not [q for q in client.queries if "proj.ns.t" in q], client.queries


def test_cdc_offsets_table_stays_in_the_connection_dataset():
    """The offsets table is per-CONNECTION control-plane state keyed by
    pipeline_id. The sink's seed call (callGetCDCOffsets) forwards NO namespace
    and ``get_cdc_offsets`` reads ``_offsets_fq(config)`` — the connection's
    dataset — so an offsets table that followed the DATA into the namespace
    could never be read back and the restart high-water seed would silently
    return empty. The data moves; the offsets must not."""
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.merge(CONFIG, {
        "table": "t", "data": _rows(1), "key_fields": ["id"], "namespace": "ns",
        "kafka_offset": [{"pipeline_id": "p1", "topic": "tp", "partition": 0,
                          "offset": 42}],
    })
    assert out["success"] is True, out
    offset_merges = [q for q in client.queries if "_rsync_cdc_offsets" in q]
    assert len(offset_merges) == 1, client.queries
    assert "`proj.ds._rsync_cdc_offsets`" in offset_merges[0], offset_merges[0]
    assert "proj.ns._rsync_cdc_offsets" not in offset_merges[0], offset_merges[0]
    # and the reader resolves the same place
    assert adapter._offsets_fq(CONFIG) == "proj.ds._rsync_cdc_offsets"


# ============================== discovery ===================================

def test_discover_schema_names_each_tables_dataset():
    # Every discovered table says which dataset it is in, in the "schema" field
    # every other source fills with its schema or database. The executor places
    # a table under the connection's Scope by that field: without it a Scope of
    # "Only these: <dataset>" left out every table (prod 2026-09-29).
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    client.table_ids = ["events", "users"]
    out = adapter.discover_schema(CONFIG, {})
    assert out["overall_status"] == "success", out
    assert client.listed_datasets == ["proj.ds"], client.listed_datasets
    got = [(t.get("schema"), t["name"], t["endpoint"]) for t in out["tables"]]
    assert got == [("ds", "events", "proj.ds.events"), ("ds", "users", "proj.ds.users")], got


def _fintech_client(client):
    client.datasets = ["fintech_silver", "fintech_bronze"]
    client.tables_by_dataset = {
        "proj.fintech_bronze": ["transactions"],
        "proj.fintech_silver": ["customers", "loans"],
    }


def test_discover_schema_without_a_dataset_lists_every_dataset_it_can_see():
    # The Explorer asks with dataset_id blanked, so a warehouse whose models
    # write to other datasets (bronze/silver/gold) shows all of them, each table
    # under its own dataset. Before: "Missing project_id/dataset_id", 0 tables.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    _fintech_client(client)
    out = adapter.discover_schema({"project_id": "proj", "dataset_id": ""}, {})
    assert out["overall_status"] == "success", out
    assert client.listed_projects == ["proj"], client.listed_projects
    assert client.listed_datasets == ["proj.fintech_bronze", "proj.fintech_silver"], client.listed_datasets
    got = [(t["schema"], t["name"], t["endpoint"]) for t in out["tables"]]
    assert got == [
        ("fintech_bronze", "transactions", "proj.fintech_bronze.transactions"),
        ("fintech_silver", "customers", "proj.fintech_silver.customers"),
        ("fintech_silver", "loans", "proj.fintech_silver.loans"),
    ], got
    assert out["total_tables_available"] == 3 and out["total_tables_discovered"] == 3, out
    # max_tables caps the whole listing, not each dataset.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    _fintech_client(client)
    out = adapter.discover_schema({"project_id": "proj"}, {"max_tables": 2})
    assert [t["name"] for t in out["tables"]] == ["transactions", "customers"], out["tables"]
    assert out["total_tables_available"] == 3, out


def test_discover_schema_reads_no_table_metadata_unless_asked():
    # Control: exactly what pipeline discovery sends (executor.go
    # discoverSchemaWithTotals). It must stay a listing — no per-table metadata
    # call, no columns, no row counts — so a BigQuery-source pipeline's column
    # types and drift baseline do not change.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.events": _bq_fakes._Table("proj.ds.events", schema=[
            _bq_fakes._SchemaField("id", "INT64", "REQUIRED")], num_rows=9)})
    client.table_ids = ["events"]
    out = adapter.discover_schema(CONFIG, {"include_columns": True, "include_row_counts": True,
                                           "include_relationships": True, "max_tables": 5000})
    assert out["overall_status"] == "success", out
    assert client.fetched_tables == [], client.fetched_tables
    assert "columns" not in out["tables"][0] and "row_count" not in out["tables"][0], out["tables"]


def test_discover_schema_table_details_give_columns_and_row_counts():
    F = _bq_fakes._SchemaField
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.events": _bq_fakes._Table("proj.ds.events", schema=[
            F("id", "INT64", "REQUIRED"),
            F("amount", "NUMERIC", "NULLABLE"),
            F("tags", "STRING", "REPEATED"),
            F("meta", "RECORD", None),
            F("raw", "BYTES", "NULLABLE"),
        ], num_rows=1234)})
    client.table_ids = ["events"]
    out = adapter.discover_schema(CONFIG, {"include_table_details": True,
                                           "include_columns": True, "include_row_counts": True})
    assert out["overall_status"] == "success", out
    assert client.fetched_tables == ["proj.ds.events"], client.fetched_tables
    assert all(t is not None for t in client.get_table_timeouts), client.get_table_timeouts
    table = out["tables"][0]
    assert table["discovery_status"] == "complete", table
    assert table["row_count"] == 1234, table
    assert table["columns"] == [
        {"name": "id", "type": "integer", "source_type": "INT64", "nullable": False},
        {"name": "amount", "type": "decimal", "source_type": "NUMERIC", "nullable": True},
        {"name": "tags", "type": "json", "source_type": "ARRAY<STRING>", "nullable": True},
        {"name": "meta", "type": "json", "source_type": "RECORD", "nullable": True},
        {"name": "raw", "type": "binary", "source_type": "BYTES", "nullable": True},
    ], table["columns"]
    # Each part only when asked for.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.events": _bq_fakes._Table("proj.ds.events", schema=[F("id", "INT64")], num_rows=7)})
    client.table_ids = ["events"]
    out = adapter.discover_schema(CONFIG, {"include_table_details": True,
                                           "include_columns": False, "include_row_counts": True})
    assert out["tables"][0]["row_count"] == 7 and "columns" not in out["tables"][0], out["tables"]


def test_discover_schema_a_table_whose_details_failed_is_partial():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.events": _bq_fakes._Table("proj.ds.events", schema=[
            _bq_fakes._SchemaField("id", "INT64")], num_rows=1)})
    client.table_ids = ["events", "users"]
    client.get_table_raises = {"proj.ds.users": RuntimeError("Access Denied: Table proj:ds.users")}
    out = adapter.discover_schema(CONFIG, {"include_table_details": True, "include_columns": True,
                                           "include_row_counts": True})
    by_name = {t["name"]: t for t in out["tables"]}
    assert by_name["events"]["discovery_status"] == "complete" and by_name["events"]["columns"], by_name
    assert by_name["users"]["discovery_status"] == "partial", by_name
    assert "columns" not in by_name["users"] and "row_count" not in by_name["users"], by_name
    assert out["overall_status"] == "partial_success", out
    warn = [w for w in out["warnings_objects"] if "ds.users" in w["message"]]
    assert warn and warn[0]["severity"] == "warning", out["warnings_objects"]


def test_discover_schema_table_details_say_where_an_external_tables_files_are():
    # The Lineage page links a lake pipeline's table folder to the BigQuery
    # external table over it from these. The URIs are BigQuery's own, unchanged
    # (prod fintech_bronze.transactions, read with bq show 2026-10-14); a table
    # that is not external says nothing, so a native table never looks like one.
    lake = "gs://rsync-ai-fintech-lake/bronze/card_transactions/fintech/public/transactions"
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.transactions": _bq_fakes._Table(
            "proj.ds.transactions", num_rows=0, table_type="EXTERNAL",
            source_uris=[lake + "/*.parquet"], source_uri_prefix=lake),
        "proj.ds.loose": _bq_fakes._Table(
            "proj.ds.loose", table_type="EXTERNAL", source_uris=["gs://b/loose/*.csv"]),
        "proj.ds.silver": _bq_fakes._Table("proj.ds.silver", num_rows=3),
    })
    client.table_ids = ["transactions", "loose", "silver"]
    out = adapter.discover_schema(CONFIG, {"include_table_details": True})
    by_name = {t["name"]: t for t in out["tables"]}
    assert by_name["transactions"]["table_type"] == "external", by_name
    assert by_name["transactions"]["source_uris"] == [lake + "/*.parquet"], by_name
    assert by_name["transactions"]["source_uri_prefix"] == lake, by_name
    assert by_name["loose"]["source_uris"] == ["gs://b/loose/*.csv"], by_name
    assert "source_uri_prefix" not in by_name["loose"], by_name
    for key in ("table_type", "source_uris", "source_uri_prefix"):
        assert key not in by_name["silver"], by_name
    # A listing with no details (pipeline discovery) never carries them.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, tables={
        "proj.ds.transactions": _bq_fakes._Table(
            "proj.ds.transactions", table_type="EXTERNAL", source_uris=[lake + "/*"])})
    client.table_ids = ["transactions"]
    out = adapter.discover_schema(CONFIG, {})
    assert "table_type" not in out["tables"][0] and "source_uris" not in out["tables"][0], out


def test_discover_schema_table_details_stop_at_the_time_budget():
    import threading
    import time as _time
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    client.table_ids = ["fast", "slow"]
    gate = threading.Event()
    client.get_table_waits = {"proj.ds.slow": gate}
    adapter.DISCOVER_DETAILS_BUDGET_SECONDS = 0.2  # type: ignore[attr-defined]
    started = _time.monotonic()
    try:
        out = adapter.discover_schema(CONFIG, {"include_table_details": True,
                                               "include_columns": True, "include_row_counts": True})
    finally:
        gate.set()
    assert _time.monotonic() - started < 2, "discovery waited for the slow table"
    by_name = {t["name"]: t for t in out["tables"]}
    assert by_name["fast"]["discovery_status"] == "complete", by_name
    assert by_name["slow"]["discovery_status"] == "partial", by_name
    assert any("ds.slow" in m for m in out["warnings_messages"]), out["warnings_messages"]


def test_discover_schema_an_unreadable_dataset_is_a_warning_not_a_failure():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    client.datasets = ["locked", "open"]
    client.tables_by_dataset = {"proj.locked": RuntimeError("Access Denied: Dataset proj:locked"),
                                "proj.open": ["t"]}
    out = adapter.discover_schema({"project_id": "proj"}, {})
    assert [(t["schema"], t["name"]) for t in out["tables"]] == [("open", "t")], out["tables"]
    assert out["overall_status"] == "partial_success", out
    warn = [w for w in out["warnings_objects"] if "locked" in w["message"]]
    assert warn and warn[0]["severity"] == "warning", out["warnings_objects"]
    # When no dataset could be read, nothing was discovered: an error, so the
    # caller shows the reason instead of an empty list.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    client.datasets = ["locked"]
    client.tables_by_dataset = {"proj.locked": RuntimeError("Access Denied: Dataset proj:locked")}
    out = adapter.discover_schema({"project_id": "proj"}, {})
    assert out["tables"] == [] and out["overall_status"] == "partial_success", out
    assert [w["severity"] for w in out["warnings_objects"]] == ["error"], out["warnings_objects"]


def test_discover_schema_a_failed_dataset_listing_is_an_error():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    client.list_datasets_raises = RuntimeError("Permission bigquery.datasets.list denied")
    out = adapter.discover_schema({"project_id": "proj"}, {})
    assert out["tables"] == [] and out["overall_status"] == "partial_success", out
    assert [w["severity"] for w in out["warnings_objects"]] == ["error"], out["warnings_objects"]
    assert "datasets.list denied" in out["warnings_messages"][0], out
    # And no project at all is still an error, with nothing listed.
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.discover_schema({"dataset_id": "ds"}, {})
    assert out["tables"] == [] and out["warnings_objects"][0]["severity"] == "error", out
    assert client.listed_projects == [] and client.listed_datasets == [], client.listed_datasets


def test_the_image_ships_the_type_map_discovery_uses():
    # Column types come from canonical_types.py; the adapter imports it guarded,
    # so an image without it would quietly type every column "string".
    with open(os.path.join(_HERE, "Dockerfile"), encoding="utf-8") as f:
        dockerfile = f.read()
    assert "COPY --from=shared canonical_types.py /app/canonical_types.py" in dockerfile
    assert warehouse_adapters.canonical_types is not None


# ============================ execute (models) ==============================

def test_execute_runs_one_job_with_a_server_side_timeout_and_default_dataset():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, dml_affected_rows=12)
    sql = "INSERT INTO t SELECT * FROM src"
    out = adapter.execute(CONFIG, {"query": sql, "timeout_seconds": 60})
    assert out["success"] is True, out
    assert out["rows_affected"] == 12, out
    assert client.queries == [sql], client.queries  # one statement, verbatim
    cfg = client.query_configs[0]
    assert cfg.extra.get("job_timeout_ms") == 60000, cfg.extra  # BigQuery cancels the job
    assert cfg.extra.get("default_dataset") == "proj.ds", cfg.extra  # unqualified names
    assert client.query_kwargs[0].get("job_retry", "unset") is None, client.query_kwargs
    # Waited on jobs.get; result() only reads the finished job's outcome.
    assert client.jobs[0].result_timeouts == [None], client.jobs[0].result_timeouts


def test_execute_ddl_reports_no_rows_affected_and_no_dataset_without_one():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.execute({"project_id": "proj"},
                          {"statement": "DROP TABLE IF EXISTS `proj.ds.t`"})
    assert out["success"] is True and out["rows_affected"] is None, out
    cfg = client.query_configs[0]
    assert "default_dataset" not in cfg.extra and "job_timeout_ms" not in cfg.extra, cfg.extra


# Bug class: a statement wait whose limit the client library stretches. The
# gateway asks for a limit 5 s inside its own run deadline so the warehouse stops
# the statement and says so first; QueryJob.result(timeout=t) waits in 10 s
# getQueryResults calls and checks t only between them, so a 3 s wait ran 10 s and
# the gateway gave up on a statement that was still running.
def test_execute_cancels_the_job_when_the_wait_runs_out():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_job_running_polls=None)
    out = adapter.execute(CONFIG, {"sql": "CREATE TABLE t AS SELECT 1 AS a",
                                   "timeout_seconds": 3})
    assert out["success"] is False and "3" in out["error"], out
    assert client.jobs[0].cancelled is True, "a timed-out job must be cancelled"
    assert len(client.queries) == 1, client.queries  # never retried
    assert client.clock.now == 3, f"the statement waited {client.clock.now}s, limit 3s"
    assert client.jobs[0].result_timeouts == [], "a running job is waited on with jobs.get"
    assert max(client.clock.pauses) <= 1.0, client.clock.pauses
    err = out["error"].lower()
    assert not [w for w in _ORCHESTRATOR_RETRY_WORDS if w in err], out


# A wait that fails (jobs.get unreachable) is reported as a failure, so the job
# must not go on to run unseen.
def test_execute_cancels_the_job_when_the_wait_fails():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters, query_job_running_polls=None)
    job_done = _bq_fakes.FakeJob.done

    def unreachable(self, *a, **kw):
        job_done(self, *a, **kw)
        raise ConnectionError("jobs.get: connection reset")
    _bq_fakes.FakeJob.done = unreachable
    try:
        out = adapter.execute(CONFIG, {"sql": "CREATE TABLE t AS SELECT 1 AS a",
                                       "timeout_seconds": 3})
    finally:
        _bq_fakes.FakeJob.done = job_done
    assert out == {"success": False, "error": "jobs.get: connection reset"}, out
    assert client.jobs[0].cancelled is True, "a job whose wait failed must be cancelled"


# No wait on a BigQuery job may use result(timeout=): its limit is stretched by
# up to 10 s. Waits poll jobs.get against the adapter's own clock instead.
def test_no_bigquery_wait_uses_result_timeout():
    import ast
    path = os.path.join(os.path.dirname(warehouse_adapters.__file__), "warehouse_adapters.py")
    with open(path, encoding="utf-8") as f:
        tree = ast.parse(f.read())
    stretched = [n.lineno for n in ast.walk(tree)
                 if isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)
                 and n.func.attr == "result" and any(k.arg == "timeout" for k in n.keywords)]
    assert stretched == [], f"result(timeout=) at warehouse_adapters.py lines {stretched}"


def test_execute_failure_returns_the_engine_error():
    adapter, client = _bq_fakes.make_adapter(
        warehouse_adapters, query_job_raises=RuntimeError("Not found: Table proj:ds.nope"))
    out = adapter.execute(CONFIG, {"query": "DELETE FROM nope WHERE TRUE"})
    assert out == {"success": False, "error": "Not found: Table proj:ds.nope"}, out
    assert len(client.queries) == 1 and client.jobs[0].cancelled is False


def test_execute_requires_a_statement():
    adapter, client = _bq_fakes.make_adapter(warehouse_adapters)
    out = adapter.execute(CONFIG, {"query": ""})
    assert out["success"] is False and client.queries == [], out


# ================================= runner ===================================

def _run():
    tests = [v for k, v in sorted(globals().items())
             if k.startswith("test_") and callable(v)]
    failed = 0
    for t in tests:
        try:
            t()
            print(f"PASS {t.__name__}")
        except Exception as e:  # noqa: BLE001
            failed += 1
            print(f"FAIL {t.__name__}: {type(e).__name__}: {e}")
    print(f"\n{len(tests) - failed}/{len(tests)} passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(_run())
