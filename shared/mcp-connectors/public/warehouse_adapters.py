#!/usr/bin/env python3
"""
Warehouse adapters (runtime plugins) for non-DBAPI warehouses.

Why this exists:
- `connector_database.py.j2` is designed around DB-API / cursor-style databases.
- Some "data warehouses" (notably BigQuery) are NOT DB-API compatible in the way
  our generic template assumes.

Instead of hardcoding BigQuery-only methods into the generic template, we provide a
small adapter registry here. The generated connector can ask for an adapter by
`connector_type` and delegate:
- test_connection
- discover_schema
- export
- import_data

To add support for a new non-DBAPI warehouse, implement an adapter and register it
in `ADAPTERS` without touching the generic template.
"""

from __future__ import annotations

import concurrent.futures
import json
import logging
import os
import re
import time
from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Protocol

# Stable batch paging (KI-WAREHOUSE-BATCH-PAGING-UNORDERED). It sits next to
# this file under public/; an image that ships this file without it (one that
# never exports from BigQuery) gets a clear error instead of an import crash.
try:
    import sql_paging
except ImportError:  # pragma: no cover
    sql_paging = None

# The canonical column-type map BigQuery discovery reports columns in, shipped
# the same way (canonical_types.py); without it a column's type is "string".
try:
    import canonical_types
except ImportError:  # pragma: no cover
    canonical_types = None


def _param_on(params: Dict[str, Any], key: str, default: bool) -> bool:
    """A boolean request flag, whether it arrives as a bool or as text."""
    value = params.get(key)
    if value is None:
        return default
    if isinstance(value, bool):
        return value
    return str(value).strip().lower() in ("1", "true", "yes", "on")


def _bigquery_column(field: Any) -> Dict[str, Any]:
    """A discovered BigQuery column: canonical ``type`` (the discover contract),
    the BigQuery type as ``source_type``. A REPEATED field is an array: json."""
    raw = str(getattr(field, "field_type", None) or "STRING").upper()
    mode = str(getattr(field, "mode", None) or "NULLABLE").upper()
    if mode == "REPEATED":
        canonical, source_type = "json", f"ARRAY<{raw}>"
    else:
        canonical = (canonical_types.canonicalize_type(raw, dialect="bigquery")
                     if canonical_types is not None else "string")
        source_type = raw
    return {"name": field.name, "type": canonical, "source_type": source_type,
            "nullable": mode != "REQUIRED"}


def _bigquery_external_files(bq_table: Any) -> Optional[Dict[str, Any]]:
    """Where an external table's rows live: its source URIs as BigQuery holds them,
    and a hive table's partition prefix. None for any other table type.

    The Lineage page links a lake pipeline's table folder to the warehouse table
    that reads it from these (api-gateway lake_external_tables.go). They come from
    the same get_table read as the columns: no query, no bytes billed."""
    if str(getattr(bq_table, "table_type", None) or "").upper() != "EXTERNAL":
        return None
    ext = getattr(bq_table, "external_data_configuration", None)
    uris = [str(u) for u in (getattr(ext, "source_uris", None) or []) if u]
    hive = getattr(ext, "hive_partitioning", None)
    prefix = str(getattr(hive, "source_uri_prefix", None) or "")
    files: Dict[str, Any] = {"source_uris": uris}
    if prefix:
        files["source_uri_prefix"] = prefix
    return files


class DataWarehouseAdapter(Protocol):
    def test_connection(self, config: Dict[str, Any]) -> Dict[str, Any]: ...
    def discover_schema(self, config: Dict[str, Any], params: Optional[Dict[str, Any]] = None) -> Dict[str, Any]: ...
    def list_namespaces(self, config: Dict[str, Any]) -> Dict[str, Any]: ...
    def export(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]: ...
    def import_data(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]: ...
    # Optional warehouse-native operations (prefer bulk load + merge semantics)
    def load(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]: ...
    def merge(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]: ...


@dataclass
class BigQueryWarehouseAdapter:
    """
    BigQuery adapter using google-cloud-bigquery client.

    Config inputs (preferred):
    - project_id (required for most operations)
    - dataset_id (required for list tables + unqualified table names)
    - service_account_json (optional, secret): full JSON string
    - credentials_path (optional): path to SA JSON file; also supports GOOGLE_APPLICATION_CREDENTIALS
    - location (optional)
    """

    # How long a verbatim read (``export`` with ``query``) may run before its job
    # is cancelled: under the orchestrator's 30 s cap on one MCP call
    # (mcp/client.go httpCallCap), so a slow read ends here, cleanly, instead of
    # the call timing out and being re-issued.
    READ_TIMEOUT_SECONDS = 25

    # How long discover_schema may spend reading per-table metadata (columns,
    # row counts) when the caller asks for it with include_table_details: under
    # the same 30 s cap, after the dataset and table listing. A table not read
    # in time is returned by name, marked "partial".
    DISCOVER_DETAILS_BUDGET_SECONDS = 20
    DISCOVER_DETAILS_WORKERS = 8

    # The clock and the pause of that wait; tests swap in a fake clock.
    _clock = staticmethod(time.monotonic)
    _pause = staticmethod(time.sleep)

    def _client(self, config: Dict[str, Any]):
        try:
            from google.cloud import bigquery  # type: ignore
        except Exception as e:
            raise Exception(
                f"google-cloud-bigquery is not installed: {e}. "
                "Install with: pip install google-cloud-bigquery"
            )

        project_id = (config.get("project_id") or "").strip() or None
        location = (config.get("location") or "").strip() or None

        service_account_json = config.get("service_account_json") or config.get("credentials_json")
        credentials_path = (config.get("credentials_path") or "").strip()
        if not credentials_path:
            credentials_path = (os.getenv("GOOGLE_APPLICATION_CREDENTIALS") or "").strip()

        credentials = None
        if service_account_json:
            try:
                from google.oauth2 import service_account  # type: ignore

                info = json.loads(service_account_json) if isinstance(service_account_json, str) else service_account_json
                credentials = service_account.Credentials.from_service_account_info(info)
            except Exception as e:
                raise Exception(f"Invalid service_account_json: {e}")
        elif credentials_path:
            try:
                from google.oauth2 import service_account  # type: ignore

                credentials = service_account.Credentials.from_service_account_file(credentials_path)
            except Exception as e:
                raise Exception(f"Invalid credentials_path: {e}")

        return bigquery.Client(project=project_id, credentials=credentials, location=location)

    def _bq(self):
        try:
            from google.cloud import bigquery  # type: ignore
        except Exception as e:
            raise Exception(
                f"google-cloud-bigquery is not installed: {e}. "
                "Install with: pip install google-cloud-bigquery"
            )
        return bigquery

    def _ensure_table_has_columns(self, client, table_fq: str, sample_row: Dict[str, Any]) -> None:
        """
        Best-effort additive schema evolution:
        - Add missing columns as NULLABLE with inferred types
        - Never attempts narrowing type changes
        """
        bq = self._bq()
        table = client.get_table(table_fq)
        existing = {f.name: f for f in (table.schema or [])}
        new_fields = list(table.schema or [])

        def infer_type(v: Any) -> str:
            # Conservative, compatible defaults
            if v is None:
                return "STRING"
            if isinstance(v, bool):
                return "BOOL"
            if isinstance(v, int) and not isinstance(v, bool):
                return "INT64"
            if isinstance(v, float):
                return "FLOAT64"
            if isinstance(v, (dict, list)):
                # BigQuery has native JSON type; if not supported in some envs, STRING still works.
                return "JSON"
            return "STRING"

        for k, v in (sample_row or {}).items():
            if not k or k in existing:
                continue
            t = infer_type(v)
            new_fields.append(bq.SchemaField(name=str(k), field_type=t, mode="NULLABLE"))

        if len(new_fields) != len(table.schema or []):
            table.schema = new_fields
            client.update_table(table, ["schema"])

    def _ensure_soft_delete_columns(self, client, table_fq: str) -> None:
        bq = self._bq()
        table = client.get_table(table_fq)
        existing = {f.name for f in (table.schema or [])}
        new_fields = list(table.schema or [])
        changed = False

        if "_rsync_deleted" not in existing:
            new_fields.append(bq.SchemaField(name="_rsync_deleted", field_type="BOOL", mode="NULLABLE"))
            changed = True
        if "_rsync_synced_at" not in existing:
            new_fields.append(bq.SchemaField(name="_rsync_synced_at", field_type="TIMESTAMP", mode="NULLABLE"))
            changed = True

        if changed:
            table.schema = new_fields
            client.update_table(table, ["schema"])

    def _config_with_namespace(self, params: Dict[str, Any], config: Dict[str, Any],
                               table: str) -> Dict[str, Any]:
        """Return a config copy whose ``dataset_id`` is overridden by an explicit
        ``namespace`` param when the table is bare.

        BigQuery's namespace analog is the DATASET, not a schema — that is the only
        difference from the snowflake/databricks/redshift helper of the same name.
        The sink bares the table and forwards the pipeline's destination namespace
        separately as ``namespace``/``db_or_schema`` (kafka-sink-worker
        ``addNamespaceParam``, taken for every warehouse destination). Without this
        the write paths resolved ``config["dataset_id"]`` unconditionally, so a
        pipeline's ``destination_namespace`` was silently discarded and rows landed
        in the connection's dataset — "loaded" but mis-routed, which no row count
        can detect.

        ``export`` deliberately does NOT use this: a destination namespace must
        never retarget a source read.
        """
        ns = (params.get("namespace") or params.get("db_or_schema")
              or params.get("destination_namespace") or "").strip()
        # Empty and the literal "default" both mean "no real namespace" (the sink's
        # isRealNamespace); they must leave the connection's dataset untouched.
        if not ns or ns.lower() == "default":
            return config
        # Only a bare table takes the namespace; a dataset-qualified or fully
        # qualified name already names its own dataset and must win.
        if "." in str(table).strip("`"):
            return config
        qcfg = dict(config)
        qcfg["dataset_id"] = ns
        return qcfg

    def _qualify_table(self, config: Dict[str, Any], table: str) -> str:
        raw = (table or "").strip().strip("`")
        if not raw:
            raise ValueError("Missing table")

        if raw.count(".") == 2:
            return raw

        project_id = (config.get("project_id") or "").strip()
        dataset_id = (config.get("dataset_id") or "").strip()

        if raw.count(".") == 1:
            if not project_id:
                raise ValueError("Missing project_id for dataset.table reference")
            return f"{project_id}.{raw}"

        if not project_id or not dataset_id:
            raise ValueError("Missing project_id/dataset_id for unqualified table reference")
        return f"{project_id}.{dataset_id}.{raw}"

    def test_connection(self, config: Dict[str, Any]) -> Dict[str, Any]:
        try:
            client = self._client(config)
            job = client.query("SELECT 1 AS ok")
            _ = list(job.result(max_results=1))
            return {"success": True, "message": "Connection successful"}
        except Exception as e:
            return {"success": False, "error": str(e)}

    def discover_schema(self, config: Dict[str, Any], params: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        import time
        from datetime import datetime

        params = params or {}
        try:
            max_tables = int(params.get("max_tables", 100))
        except Exception:
            max_tables = 100
        if max_tables <= 0:
            max_tables = 100

        start_ms = int(time.time() * 1000)
        result: Dict[str, Any] = {
            "schema_version": "2.0",
            "discovered_at": datetime.utcnow().isoformat() + "Z",
            "connector_type": "bigquery",
            "connector_version": os.getenv("MCP_CONNECTOR_VERSION") or os.getenv("CONNECTOR_VERSION") or None,
            "database_version": "BigQuery",
            "total_tables_available": 0,
            "total_tables_discovered": 0,
            "discovery_duration_ms": 0,
            "overall_status": "success",
            "warnings_objects": [],
            "warnings_messages": [],
            "tables": [],
        }

        started = time.monotonic()

        def warn(msg: str, severity: str) -> Dict[str, Any]:
            obj = {"category": "catalog_error", "severity": severity, "message": msg, "subsystem": "tables"}
            result["warnings_objects"].append(obj)
            result["warnings_messages"].append(msg)
            result["overall_status"] = "partial_success"
            return obj

        def done() -> Dict[str, Any]:
            result["discovery_duration_ms"] = int(time.time() * 1000) - start_ms
            return result

        dataset_id = (config.get("dataset_id") or "").strip()
        project_id = (config.get("project_id") or "").strip()
        if not project_id:
            result["overall_status"] = "partial_success"
            msg = "Missing project_id; cannot list tables."
            result["warnings_objects"].append({"category": "config", "severity": "error", "message": msg})
            result["warnings_messages"].append(msg)
            return done()

        try:
            client = self._client(config)
            if dataset_id:
                datasets = [dataset_id]
            else:
                # No dataset named (the Explorer asks this way, to show where
                # models write as well as where pipelines land): every dataset
                # the credentials can list, each table under its own dataset.
                datasets = sorted({str(ds.dataset_id) for ds in client.list_datasets(project_id)
                                   if getattr(ds, "dataset_id", None)})
        except Exception as e:
            warn(f"BigQuery discovery failed: {e}", "error")
            return done()

        listed: List[tuple] = []
        unreadable: List[Dict[str, Any]] = []
        for ds in datasets:
            try:
                ids = [t.table_id for t in client.list_tables(f"{project_id}.{ds}")]
            except Exception as e:
                if dataset_id:
                    warn(f"BigQuery discovery failed: {e}", "error")
                    return done()
                # One dataset the credentials cannot read does not hide the rest.
                unreadable.append(warn(f"Could not list tables in dataset {ds}: {e}", "warning"))
                continue
            listed.extend((ds, table_id) for table_id in ids)
        if unreadable and not listed:
            # Nothing could be read at all: a failure, so the reason is shown.
            for obj in unreadable:
                obj["severity"] = "error"

        result["total_tables_available"] = len(listed)
        out: List[Dict[str, Any]] = [
            {
                "name": table_id,
                # The table's namespace (namespace_model table_namespace
                # "dataset"), in the field every source fills with its
                # schema or database: the executor places a table under
                # the connection's Scope by it, and a pick reaches export
                # as "dataset.table" (_qualify_table).
                "schema": ds,
                "endpoint": f"{project_id}.{ds}.{table_id}",
                "discovery_status": "complete",
            }
            for ds, table_id in listed[:max_tables]
        ]
        # Per-table metadata only when asked for: pipeline discovery does not
        # ask, so a BigQuery-source pipeline's column types are unchanged.
        if _param_on(params, "include_table_details", False) and out:
            budget = float(self.DISCOVER_DETAILS_BUDGET_SECONDS)
            self._read_table_details(
                client, out, warn,
                columns=_param_on(params, "include_columns", True),
                row_counts=_param_on(params, "include_row_counts", True),
                budget=budget,
                remaining=max(0.0, started + budget - time.monotonic()),
            )
        result["tables"] = out
        result["total_tables_discovered"] = len(out)
        return done()

    def _read_table_details(self, client, tables: List[Dict[str, Any]], warn, *,
                            columns: bool, row_counts: bool, budget: float,
                            remaining: float) -> None:
        """Fill each table's columns and row count from its metadata (get_table:
        no query, no bytes billed), several at a time, for at most ``remaining``
        seconds. A table not read is marked "partial" and named in a warning."""
        pool = concurrent.futures.ThreadPoolExecutor(max_workers=self.DISCOVER_DETAILS_WORKERS)
        try:
            futures = {pool.submit(client.get_table, t["endpoint"], timeout=budget): t for t in tables}
            finished, _ = concurrent.futures.wait(futures, timeout=remaining)
        finally:
            # Do not wait for a slow read: it ends on its own timeout.
            pool.shutdown(wait=False, cancel_futures=True)

        failed: List[str] = []
        late: List[str] = []
        for future, table in futures.items():
            label = f"{table['schema']}.{table['name']}"
            if future not in finished:
                table["discovery_status"] = "partial"
                late.append(label)
                continue
            try:
                bq_table = future.result()
            except Exception as e:
                table["discovery_status"] = "partial"
                failed.append(f"{label}: {e}")
                continue
            if columns:
                table["columns"] = [_bigquery_column(f) for f in (getattr(bq_table, "schema", None) or [])]
            num_rows = getattr(bq_table, "num_rows", None)
            if row_counts and num_rows is not None:
                table["row_count"] = int(num_rows)
            external = _bigquery_external_files(bq_table)
            if external is not None:
                table["table_type"] = "external"
                table.update(external)

        for msg in failed[:10]:
            warn(f"Could not read table {msg}", "warning")
        if len(failed) > 10:
            warn(f"Could not read {len(failed) - 10} more tables", "warning")
        if late:
            more = f" and {len(late) - 10} more" if len(late) > 10 else ""
            warn(f"Columns not read within {budget:g} s for {len(late)} table(s): "
                 f"{', '.join(late[:10])}{more}", "warning")

    def list_namespaces(self, config: Dict[str, Any]) -> Dict[str, Any]:
        """List the project's datasets: the level metadata.json's
        namespace_model.table_namespace names. "current" is the configured
        dataset_id, "" when there is none.
        """
        project_id = (config.get("project_id") or "").strip()
        if not project_id:
            return {"success": False, "error": "Missing project_id; cannot list datasets."}
        try:
            client = self._client(config)
            names = [getattr(ds, "dataset_id", None) for ds in client.list_datasets(project_id)]
            return {
                "success": True,
                "namespaces": sorted({str(n) for n in names if n}),
                "current": (config.get("dataset_id") or "").strip(),
            }
        except Exception as e:
            return {"success": False, "error": f"Listing namespaces failed: {e}"}

    # A BigQuery unquoted column identifier: letter/underscore then word chars.
    # cursor_column is caller/pipeline-config controlled and gets interpolated
    # into the query text (BQ has no bind-parameter form for identifiers), so it
    # MUST match this before it can touch the SQL — see _safe_identifier.
    _IDENTIFIER_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")

    @classmethod
    def _safe_identifier(cls, name: str) -> Optional[str]:
        """Return ``name`` iff it is a plain BigQuery column identifier, else None.

        Stripping backticks is NOT sanitization (it only affects the ends), so
        an unsafe value is rejected outright rather than escaped.
        """
        n = (name or "").strip().strip("`").strip()
        return n if cls._IDENTIFIER_RE.match(n) else None

    @staticmethod
    def _param_type(value: Any) -> str:
        """BigQuery scalar-parameter type for a Python cursor value."""
        import datetime as _dt

        if isinstance(value, bool):
            return "BOOL"
        if isinstance(value, int):
            return "INT64"
        if isinstance(value, float):
            return "FLOAT64"
        # datetime is a subclass of date — check the narrower type first so a
        # plain date binds as DATE (a DATE column rejects a TIMESTAMP parameter).
        if isinstance(value, _dt.datetime):
            return "TIMESTAMP"
        if isinstance(value, _dt.date):
            return "DATE"
        return "STRING"

    def export(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]:
        """One page of a table, or a verbatim ``query``/``sql`` as a single page.

        A table pages through ``sql_paging``: BigQuery never enforces a primary
        key, so a sweep orders by every orderable column (``cursor_column``
        first) and pins its pages to one ``FOR SYSTEM_TIME AS OF`` point
        (KI-WAREHOUSE-BATCH-PAGING-UNORDERED).

        Always returns ``paging_mode``, ``has_more`` and ``next_cursor`` alongside
        ``data`` / ``total_records``.
        """
        table = (params.get("table") or params.get("object") or "").strip()
        raw_query = (params.get("query") or params.get("sql") or "").strip()
        limit = int(params.get("limit", 10000) or 10000)

        raw_cursor = (
            params.get("cursor_column")
            or params.get("order_by")
            or params.get("primary_key")
            or ""
        )
        if isinstance(raw_cursor, (list, tuple)):
            raw_cursor = raw_cursor[0] if raw_cursor else ""
        raw_cursor = str(raw_cursor).strip()
        cursor_column = ""
        if raw_cursor:
            # cursor_column is interpolated into the SQL identifier position, so
            # it must be a plain identifier — reject anything else outright.
            cursor_column = self._safe_identifier(raw_cursor)
            if cursor_column is None:
                return {"success": False,
                        "error": f"Unsafe cursor_column identifier: {raw_cursor!r}"}

        if not table and not raw_query:
            return {"success": False, "error": "Missing 'table' (or 'query') parameter"}

        client = self._client(config)
        if raw_query:
            read, refused = self._read_query(client, raw_query, limit)
            if refused is not None:
                return refused
            columns, rows = read
            data = [dict(r) for r in rows]
            if not columns and data:
                columns = list(data[0].keys())
            # max_results caps a verbatim query: surface that as `truncated`
            # instead of dropping rows silently. `columns` is the SELECT's order,
            # the one order that survives the rows becoming a Go map
            # (KI-DELEGATED-QUERY-COLUMN-ORDER).
            return {
                "success": True,
                "data": data,
                "columns": columns,
                "total_records": len(data),
                "paging_mode": "query",
                "has_more": False,
                "next_cursor": None,
                "truncated": len(data) == limit,
            }

        if sql_paging is None:
            return {"success": False,
                    "error": "sql_paging.py is missing from this connector image; rebuild it"}
        fq = self._qualify_table(config, table)
        if any(ch in fq for ch in "`\\"):
            return {"success": False, "error": f"Unsafe table identifier: {table!r}"}
        cursor = sql_paging.QueryCursor(lambda sql, binds: self._run_query(client, sql, binds))
        return sql_paging.export_page(client, cursor, sql_paging.BIGQUERY, fq, f"`{fq}`", params,
                                      limit=limit, nominated=cursor_column or None)

    def _read_query(self, client, sql: str, limit: int):
        """((columns, rows), None) of a verbatim read, or (None, failure) when it
        may not run. The columns are the result schema's, so a read that matches
        no rows still names them.

        BigQuery, not the gateway's keyword gate, decides what is a read: a dry
        run parses the SQL and plans it without running anything, and only a
        single ``SELECT`` goes on to run. DML, DDL, a script, ``CALL`` and
        ``EXPORT DATA`` are refused before any job exists
        (KI-WAREHOUSE-READS-RELY-ON-THE-GATE). The read then runs as one job
        (``job_retry=None``) for at most ``READ_TIMEOUT_SECONDS``: a wait that runs
        out cancels the job, and ``job_timeout_ms`` ends it server-side a little
        later should the cancel not land. Neither failure message contains
        "timeout": the orchestrator re-issues a call whose error does
        (``executeWithRetry``), and a re-issued read would only run out again.
        The wait is ``_wait_for_job``; ``result`` is called only once the job is
        DONE, to fetch its rows."""
        bq = self._bq()
        plan = client.query(sql, job_config=bq.QueryJobConfig(dry_run=True, use_query_cache=False))
        statement_type = getattr(plan, "statement_type", None)
        if statement_type != "SELECT":
            return None, {"success": False,
                          "error": "A BigQuery read runs read-only: only a single SELECT may run, "
                                   f"and BigQuery parses this as {statement_type or 'an unknown statement'}"}
        timeout = self.READ_TIMEOUT_SECONDS
        job = client.query(sql, job_config=bq.QueryJobConfig(job_timeout_ms=(timeout + 5) * 1000),
                           job_retry=None)
        if not self._wait_for_job(job, timeout):
            return None, {"success": False,
                          "error": f"The BigQuery read did not finish within {timeout}s; the job was cancelled"}
        result = job.result(max_results=limit)
        columns = [f.name for f in (getattr(result, "schema", None) or [])]
        return (columns, list(result)), None

    def _wait_for_job(self, job, timeout: Optional[float]) -> bool:
        """Wait until ``job`` is DONE, for at most ``timeout`` seconds (None: no
        limit); False, the job cancelled, when the time ran out. A wait that
        raises cancels the job before the error goes on: a job whose wait
        failed is reported as failed, so it must not go on to run unseen.

        The wait polls ``jobs.get`` (``job.done``), which answers at once, against
        our own clock. ``job.result(timeout=)`` is no limit: BigQuery holds each of
        its ``getQueryResults`` calls up to 10 s and the timeout is checked only
        between calls, so a 2 s wait returned after 12 s (measured live,
        google-cloud-bigquery 3.42.2). Cancelling is best effort; the job's
        ``job_timeout_ms`` still ends it server-side."""
        deadline = None if timeout is None else self._clock() + timeout
        pause = 0.1
        try:
            # One jobs.get answers in well under 2 s; a slower one is retried by the library.
            while not job.done(timeout=2):
                left = None if deadline is None else deadline - self._clock()
                if left is not None and left <= 0:
                    self._cancel_job(job)
                    return False
                self._pause(pause if left is None else min(pause, left))
                pause = min(pause * 2, 1.0)
        except Exception:
            self._cancel_job(job)
            raise
        return True

    @staticmethod
    def _cancel_job(job) -> None:
        try:
            job.cancel()
        except Exception:  # noqa: BLE001 - job_timeout_ms still ends it
            pass

    def _run_query(self, client, sql: str, binds: Optional[List[Any]]):
        """(column names, dict rows) of ``sql``; bind i is ``@p<i>``."""
        job_config = None
        if binds:
            bq = self._bq()
            job_config = bq.QueryJobConfig(query_parameters=[
                bq.ScalarQueryParameter(f"p{i}", self._param_type(v), v)
                for i, v in enumerate(binds, start=1)
            ])
        job = client.query(sql, job_config=job_config) if job_config is not None else client.query(sql)
        result = job.result()
        columns = [f.name for f in (getattr(result, "schema", None) or [])]
        return columns, [dict(r) for r in result]

    def execute(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]:
        """Run exactly ONE statement verbatim — the models feature's DDL/DML.

        ``timeout_seconds`` is the job's ``job_timeout_ms``, so BigQuery itself
        cancels the job when it runs over, and also the client's wait
        (``_wait_for_job``, which ends when the limit says); a wait that runs
        out or fails cancels the job too rather than leave it running. When
        both ``project_id`` and ``dataset_id`` are configured they are the
        job's ``default_dataset``, so an unqualified table name resolves the
        way ``_qualify_table`` resolves it. Never retried: ``job_retry=None``
        stops the client re-running a failed job (DDL is not idempotent).
        ``rows_affected`` is ``num_dml_affected_rows`` — None for DDL."""
        sql = ""
        for key in ("query", "sql", "statement"):
            v = params.get(key)
            if isinstance(v, str) and v.strip():
                sql = v.strip()
                break
        if not sql:
            return {"success": False, "error": "Missing 'query' (or 'sql'/'statement') parameter"}
        timeout = params.get("timeout_seconds")
        try:
            timeout = int(timeout) if not isinstance(timeout, bool) else 0
        except (TypeError, ValueError):
            timeout = 0
        timeout = timeout if timeout > 0 else None

        try:
            client = self._client(config)
            bq = self._bq()
            job_kwargs: Dict[str, Any] = {}
            if timeout:
                job_kwargs["job_timeout_ms"] = timeout * 1000
            project_id = (config.get("project_id") or "").strip()
            dataset_id = (config.get("dataset_id") or "").strip()
            if project_id and dataset_id:
                job_kwargs["default_dataset"] = f"{project_id}.{dataset_id}"
            job = client.query(sql, job_config=bq.QueryJobConfig(**job_kwargs), job_retry=None)
            if not self._wait_for_job(job, timeout):
                return {"success": False,
                        "error": f"Statement did not finish within {timeout}s; the job was cancelled"}
            # DONE: raises the job's own error, waits for nothing.
            job.result()
        except Exception as e:  # noqa: BLE001
            return {"success": False, "error": str(e) or "The BigQuery request failed"}
        rows_affected = getattr(job, "num_dml_affected_rows", None)
        if not isinstance(rows_affected, int) or isinstance(rows_affected, bool):
            rows_affected = None
        return {"success": True, "executed": True, "rows_affected": rows_affected}

    def import_data(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]:
        table = (params.get("table") or "").strip()
        data = params.get("data") or []
        if not table:
            return {"success": False, "error": "Target table not specified"}
        if not isinstance(data, list) or not data:
            return {"success": True, "rows_inserted": 0, "message": "No data to import"}

        client = self._client(config)
        fq = self._qualify_table(self._config_with_namespace(params, config, table), table)
        errors = client.insert_rows_json(fq, data)
        if errors:
            return {"success": False, "error": "BigQuery insert_rows_json failed", "details": errors}
        return {"success": True, "rows_inserted": len(data)}

    @staticmethod
    def _env_true(name: str) -> bool:
        return (os.getenv(name) or "").strip().lower() in ("1", "true", "yes", "on")

    def _bq_load_job_enabled(self, params: Dict[str, Any]) -> bool:
        """Gate for the bulk NDJSON load-job path.

        An explicit ``bulk_load`` param wins (True forces the load job, False
        disables it); otherwise the ``RSYNC_BQ_LOAD_JOB`` env flag decides.
        Default-off (safe) — mirrors postgresql's ``RSYNC_PG_BULK_COPY``.
        """
        bl = (params or {}).get("bulk_load")
        if bl is not None:
            return bool(bl)
        return self._env_true("RSYNC_BQ_LOAD_JOB")

    def _stream_insert(self, client, fq: str, data: List[Dict[str, Any]]) -> Dict[str, Any]:
        """Row-wise streaming write (``insert_rows_json``). Fails loud on errors."""
        errors = client.insert_rows_json(fq, data)
        if errors:
            return {"success": False, "error": "BigQuery insert_rows_json failed",
                    "details": errors, "method": "streaming"}
        return {"success": True, "rows_loaded": len(data), "method": "streaming"}

    def _load_job_append(self, client, fq: str, data: List[Dict[str, Any]]) -> int:
        """Bulk append via a BigQuery load job from a temporary NDJSON file."""
        import tempfile
        import json as _json

        bq = self._bq()
        job_config = bq.LoadJobConfig(
            source_format=bq.SourceFormat.NEWLINE_DELIMITED_JSON,
            write_disposition=bq.WriteDisposition.WRITE_APPEND,
        )
        rows_written = 0
        # w+b (readable+writable): google-cloud-bigquery's load_table_from_file
        # rejects a write-only ("wb") handle ("Cannot upload files opened in text
        # mode"); it must be able to READ the staged NDJSON back after seek(0).
        with tempfile.NamedTemporaryFile(mode="w+b", suffix=".jsonl", delete=True) as f:
            for row in data:
                if not isinstance(row, dict):
                    continue
                # ensure_ascii=False: astral-plane chars (emoji, U+1F600+) must be
                # written as raw UTF-8, not \uXXXX surrogate-pair escapes — BigQuery's
                # NDJSON loader does not recombine surrogate pairs and would replace
                # each half with U+FFFD, silently corrupting the value.
                f.write((_json.dumps(row, default=str, ensure_ascii=False) + "\n").encode("utf-8"))
                rows_written += 1
            f.flush()
            f.seek(0)
            job = client.load_table_from_file(f, fq, job_config=job_config)
            try:
                job.result()
            except Exception:
                # result() can raise (e.g. a client-side poll timeout) AFTER the
                # load job has already committed. Re-check server-side job state
                # before propagating: if it actually succeeded, do NOT signal
                # failure — otherwise the caller's streaming fallback would
                # double-write these rows. Only re-raise on a genuine failure.
                if self._load_job_succeeded(job):
                    return rows_written
                raise
        return rows_written

    @staticmethod
    def _load_job_succeeded(job) -> bool:
        """True iff a BigQuery load job committed cleanly (server-side truth)."""
        try:
            reload_fn = getattr(job, "reload", None)
            if callable(reload_fn):
                reload_fn()
        except Exception:
            return False
        return (getattr(job, "error_result", None) is None
                and getattr(job, "state", None) == "DONE")

    def load(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]:
        """Bulk append load with an optimized, flag-gated fast path.

        Routing (``method`` reports what actually ran):
        - load-job (bulk) when ``_bq_load_job_enabled`` AND (``bulk_load`` is True
          OR the batch exceeds ``staging_threshold_rows``);
        - streaming (``insert_rows_json``) otherwise.

        A load-job failure NEVER silently drops rows — it falls back to streaming
        and sets ``fell_back=True`` so the caller can see the degraded path.
        """
        table = (params.get("table") or "").strip()
        data = params.get("data") or []
        if not table:
            return {"success": False, "error": "Target table not specified"}
        if not isinstance(data, list) or not data:
            return {"success": True, "rows_loaded": 0, "method": "streaming",
                    "fell_back": False, "message": "No data to load"}

        client = self._client(config)
        fq = self._qualify_table(self._config_with_namespace(params, config, table), table)

        # Best-effort additive schema evolution; never fail the load for it.
        try:
            self._ensure_table_has_columns(client, fq, data[0] if isinstance(data[0], dict) else {})
        except Exception:
            pass

        threshold_rows = int(params.get("staging_threshold_rows") or params.get("threshold_rows") or 1000)
        force = params.get("bulk_load") is True
        use_load_job = self._bq_load_job_enabled(params) and (force or len(data) > threshold_rows)

        if not use_load_job:
            out = self._stream_insert(client, fq, data)
            out["fell_back"] = False
            return out

        try:
            rows_written = self._load_job_append(client, fq, data)
            return {"success": True, "rows_loaded": rows_written,
                    "method": "load_job", "fell_back": False}
        except Exception as load_job_error:
            # Never silent-drop: degrade to streaming inserts. But make the
            # degradation VISIBLE — a silently-swallowed load-job failure hides
            # config/library bugs (a write-only file handle once made EVERY bulk
            # load job fail here and silently fall back to streaming). Surface
            # the reason in the log and in the result so callers can see it.
            logging.getLogger(__name__).warning(
                "BigQuery bulk load job failed (%s: %s); falling back to "
                "streaming inserts", type(load_job_error).__name__, load_job_error,
            )
            out = self._stream_insert(client, fq, data)
            out["fell_back"] = True
            out["load_job_error"] = f"{type(load_job_error).__name__}: {load_job_error}"
            return out

    def merge(self, config: Dict[str, Any], params: Dict[str, Any]) -> Dict[str, Any]:
        """
        Apply CDC-style upserts/deletes via BigQuery MERGE.

        Inputs:
        - table: target table (required)
        - data: list of rows to apply (required)
        - key_fields / primary_keys: list of PK columns (required)
        - Rows may include `_rsync_deleted` boolean for soft deletes.
        """
        import time

        table = (params.get("table") or "").strip()
        data = params.get("data") or []
        key_fields = params.get("key_fields") or params.get("primary_keys") or params.get("primary_key_fields") or []
        if isinstance(key_fields, str):
            key_fields = [key_fields]
        key_fields = [str(k) for k in key_fields if k]

        if not table:
            return {"success": False, "error": "Target table not specified"}
        if not isinstance(data, list) or not data:
            return {"success": True, "rows_merged": 0, "message": "No data to merge"}
        if not key_fields:
            return {"success": False, "error": "Missing primary key fields for merge (key_fields/primary_keys)"}

        client = self._client(config)
        target_fq = self._qualify_table(self._config_with_namespace(params, config, table), table)

        # Ensure soft delete columns exist on target.
        try:
            self._ensure_soft_delete_columns(client, target_fq)
        except Exception:
            pass

        # Create staging table (same dataset) and load into it.
        # Use dataset from target_fq (project.dataset.table)
        parts = target_fq.split(".")
        if len(parts) != 3:
            return {"success": False, "error": f"Invalid target table reference: {target_fq}"}
        project_id, dataset_id, _ = parts
        staging_name = f"_rsync_staging_{int(time.time() * 1000)}"
        staging_fq = f"{project_id}.{dataset_id}.{staging_name}"

        bq = self._bq()
        # Infer schema from the first row; ensure keys + soft-delete fields exist.
        sample = data[0] if isinstance(data[0], dict) else {}
        if isinstance(sample, dict):
            for k in key_fields:
                sample.setdefault(k, None)
            sample.setdefault("_rsync_deleted", False)
            sample.setdefault("_rsync_synced_at", None)

        # Create staging table with inferred schema by creating empty table then patching schema.
        try:
            client.create_table(bq.Table(staging_fq), exists_ok=True)
        except Exception:
            # Best-effort: table may already exist or permissions may prevent explicit create.
            pass
        try:
            self._ensure_table_has_columns(client, staging_fq, sample if isinstance(sample, dict) else {})
            self._ensure_soft_delete_columns(client, staging_fq)
        except Exception:
            pass

        # Load data into staging (truncate).
        job_config = bq.LoadJobConfig(
            source_format=bq.SourceFormat.NEWLINE_DELIMITED_JSON,
            write_disposition=bq.WriteDisposition.WRITE_TRUNCATE,
        )
        import tempfile
        import json as _json
        # w+b (readable+writable): google-cloud-bigquery's load_table_from_file
        # rejects a write-only ("wb") handle ("Cannot upload files opened in text
        # mode"); it must be able to READ the staged NDJSON back after seek(0).
        with tempfile.NamedTemporaryFile(mode="w+b", suffix=".jsonl", delete=True) as f:
            for row in data:
                if isinstance(row, dict):
                    # Ensure soft-delete columns are present for deterministic merge.
                    row.setdefault("_rsync_deleted", False)
                    row.setdefault("_rsync_synced_at", None)
                    # ensure_ascii=False: see _load_job_append — surrogate-pair escapes
                    # for astral chars get corrupted to U+FFFD by BigQuery's NDJSON loader.
                    f.write((_json.dumps(row, default=str, ensure_ascii=False) + "\n").encode("utf-8"))
            f.flush()
            f.seek(0)
            job = client.load_table_from_file(f, staging_fq, job_config=job_config)
            job.result()

        # Ensure target has additive columns from staging (best-effort).
        try:
            st = client.get_table(staging_fq)
            t = client.get_table(target_fq)
            target_cols = {f.name for f in (t.schema or [])}
            sample2 = {f.name: None for f in (st.schema or []) if f.name not in target_cols}
            if sample2:
                self._ensure_table_has_columns(client, target_fq, sample2)
        except Exception:
            pass

        # Build MERGE statement.
        def qcol(c: str) -> str:
            return f"`{c}`"

        on_cond = " AND ".join([f"T.{qcol(k)} = S.{qcol(k)}" for k in key_fields])

        # Columns to write: all staging columns except keys; always set _rsync_synced_at on apply.
        st_table = client.get_table(staging_fq)
        staging_cols = [f.name for f in (st_table.schema or [])]
        non_key_cols = [c for c in staging_cols if c not in key_fields]

        # For updates (non-deletes), set all non-key cols; also force _rsync_deleted false unless explicitly true.
        update_set = ", ".join([f"T.{qcol(c)} = S.{qcol(c)}" for c in non_key_cols if c != "_rsync_synced_at"])
        if update_set:
            update_set = update_set + ", "
        update_set += "T.`_rsync_synced_at` = CURRENT_TIMESTAMP()"

        delete_set = "T.`_rsync_deleted` = TRUE, T.`_rsync_synced_at` = CURRENT_TIMESTAMP()"

        insert_cols = ", ".join([qcol(c) for c in staging_cols if c != "_rsync_synced_at"] + ["`_rsync_synced_at`"])
        insert_vals = ", ".join([f"S.{qcol(c)}" for c in staging_cols if c != "_rsync_synced_at"] + ["CURRENT_TIMESTAMP()"])

        sql = f"""
MERGE `{target_fq}` T
USING `{staging_fq}` S
ON {on_cond}
WHEN MATCHED AND COALESCE(S.`_rsync_deleted`, FALSE) = TRUE THEN
  UPDATE SET {delete_set}
WHEN MATCHED AND COALESCE(S.`_rsync_deleted`, FALSE) = FALSE THEN
  UPDATE SET {update_set}
WHEN NOT MATCHED BY TARGET AND COALESCE(S.`_rsync_deleted`, FALSE) = FALSE THEN
  INSERT ({insert_cols}) VALUES ({insert_vals})
"""
        job = client.query(sql)
        job.result()

        # Best-effort cleanup.
        try:
            client.delete_table(staging_fq, not_found_ok=True)
        except Exception:
            pass

        # Tier B (idempotent): record the Kafka high-water offset AFTER the data
        # MERGE. Best-effort — duplicates on replay are absorbed by the idempotent
        # MERGE above, so a failed offset write only costs reprocessing, never data.
        try:
            # The offsets table is per-CONNECTION control-plane state keyed by
            # pipeline_id, so it deliberately stays in the CONNECTION's dataset even
            # when destination_namespace routes the DATA elsewhere: the sink's seed
            # call (callGetCDCOffsets) forwards no namespace, so an offsets table
            # that followed the data could never be read back and the restart
            # high-water seed would silently return empty. project_id/dataset_id
            # here are derived from target_fq, which IS namespace-aware.
            self._write_cdc_offsets(
                client,
                (config.get("project_id") or project_id or "").strip(),
                (config.get("dataset_id") or dataset_id or "").strip(),
                params,
            )
        except Exception:
            pass

        return {"success": True, "rows_merged": len(data)}

    _CDC_OFFSETS_TABLE = "_rsync_cdc_offsets"

    def _offsets_fq(self, config: Dict[str, Any], project_id: str = "", dataset_id: str = "") -> Optional[str]:
        project_id = (project_id or config.get("project_id") or "").strip()
        dataset_id = (dataset_id or config.get("dataset_id") or "").strip()
        if not project_id or not dataset_id:
            return None
        return f"{project_id}.{dataset_id}.{self._CDC_OFFSETS_TABLE}"

    def _write_cdc_offsets(self, client, project_id: str, dataset_id: str, params: Dict[str, Any]) -> None:
        """Persist per-partition Kafka high-water offsets (monotonic). No-op
        unless the caller (kafka-mcp-sink) supplied ``kafka_offset``."""
        ko = (params or {}).get("kafka_offset")
        if not ko:
            return
        offsets = ko if isinstance(ko, list) else [ko]
        offsets_fq = f"{project_id}.{dataset_id}.{self._CDC_OFFSETS_TABLE}"
        bq = self._bq()
        schema = [
            bq.SchemaField("pipeline_id", "STRING", mode="REQUIRED"),
            bq.SchemaField("topic", "STRING", mode="REQUIRED"),
            bq.SchemaField("kafka_partition", "INT64", mode="REQUIRED"),
            bq.SchemaField("last_offset", "INT64", mode="REQUIRED"),
            bq.SchemaField("updated_at", "TIMESTAMP", mode="NULLABLE"),
        ]
        try:
            client.create_table(bq.Table(offsets_fq, schema=schema), exists_ok=True)
        except Exception:
            pass
        for o in offsets:
            if not isinstance(o, dict):
                continue
            pid = str(o.get("pipeline_id") or "")
            topic = o.get("topic") or ""
            part = o.get("partition")
            off = o.get("offset")
            if not topic or part is None or off is None:
                continue
            sql = f"""
MERGE `{offsets_fq}` T
USING (SELECT @pid AS pipeline_id, @topic AS topic, @part AS kafka_partition, @off AS last_offset) S
ON T.pipeline_id = S.pipeline_id AND T.topic = S.topic AND T.kafka_partition = S.kafka_partition
WHEN MATCHED THEN UPDATE SET
  T.last_offset = GREATEST(T.last_offset, S.last_offset), T.updated_at = CURRENT_TIMESTAMP()
WHEN NOT MATCHED THEN
  INSERT (pipeline_id, topic, kafka_partition, last_offset, updated_at)
  VALUES (S.pipeline_id, S.topic, S.kafka_partition, S.last_offset, CURRENT_TIMESTAMP())
"""
            job_config = bq.QueryJobConfig(query_parameters=[
                bq.ScalarQueryParameter("pid", "STRING", pid),
                bq.ScalarQueryParameter("topic", "STRING", str(topic)),
                bq.ScalarQueryParameter("part", "INT64", int(part)),
                bq.ScalarQueryParameter("off", "INT64", int(off)),
            ])
            client.query(sql, job_config=job_config).result()

    def get_cdc_offsets(self, config: Dict[str, Any], params: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        """Return durable per-partition Kafka high-water offsets so the sink can
        seed its skip-map on restart (CDC exactly-once recovery)."""
        params = params or {}
        try:
            offsets_fq = self._offsets_fq(config)
            if not offsets_fq:
                return {"success": True, "offsets": []}
            pipeline_id = str(params.get("pipeline_id") or "")
            client = self._client(config)
            bq = self._bq()
            sql = (
                f"SELECT topic, kafka_partition, last_offset FROM `{offsets_fq}` "
                "WHERE pipeline_id = @pid"
            )
            job_config = bq.QueryJobConfig(query_parameters=[
                bq.ScalarQueryParameter("pid", "STRING", pipeline_id),
            ])
            rows = list(client.query(sql, job_config=job_config).result())
            offsets = [
                {"topic": r["topic"], "partition": int(r["kafka_partition"]), "offset": int(r["last_offset"])}
                for r in rows
            ]
            return {"success": True, "offsets": offsets}
        except Exception:
            # Offset table not provisioned yet => no prior progress.
            return {"success": True, "offsets": []}


ADAPTERS: Dict[str, Any] = {
    # Non-DBAPI warehouse adapters
    "bigquery": BigQueryWarehouseAdapter,
}


def get_data_warehouse_adapter(connector_type: str) -> Optional[DataWarehouseAdapter]:
    ct = (connector_type or "").strip().lower()
    cls = ADAPTERS.get(ct)
    if not cls:
        return None
    return cls()

