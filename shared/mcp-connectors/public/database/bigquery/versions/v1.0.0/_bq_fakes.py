"""Offline test doubles for ``google-cloud-bigquery``.

The BigQuery client library is NOT installed in the unit-test environment
(intentionally — these tests must run with stdlib only). ``BigQueryWarehouseAdapter``
touches the client only through two seams: ``_client(config)`` returns a
``bigquery.Client`` and ``_bq()`` returns the ``bigquery`` module. Tests swap
both seams for the fakes below, so no network / credentials / real library.

Fakes record every call (queries, load jobs, streaming inserts, param configs)
so tests can assert the SQL text, the load-job config, and the routing decision.
"""

from __future__ import annotations

import concurrent.futures
import threading
import types
from typing import Any, Dict, List, Optional


# --- module-level enums / config objects mirroring google.cloud.bigquery -----

class _SourceFormat:
    NEWLINE_DELIMITED_JSON = "NEWLINE_DELIMITED_JSON"
    CSV = "CSV"


class _WriteDisposition:
    WRITE_APPEND = "WRITE_APPEND"
    WRITE_TRUNCATE = "WRITE_TRUNCATE"
    WRITE_EMPTY = "WRITE_EMPTY"


class _LoadJobConfig:
    def __init__(self, source_format=None, write_disposition=None, **kw):
        self.source_format = source_format
        self.write_disposition = write_disposition
        self.extra = kw


class _QueryJobConfig:
    def __init__(self, query_parameters=None, **kw):
        self.query_parameters = list(query_parameters or [])
        self.extra = kw


class _ScalarQueryParameter:
    def __init__(self, name, type_, value):
        self.name = name
        self.type_ = type_
        self.value = value

    def __repr__(self):  # pragma: no cover - debug aid
        return f"ScalarQueryParameter({self.name!r},{self.type_!r},{self.value!r})"


class _SchemaField:
    def __init__(self, name, field_type=None, mode=None, **kw):
        self.name = name
        self.field_type = field_type
        self.mode = mode


class _Table:
    def __init__(self, ref, schema=None, num_rows=None, table_type="TABLE",
                 source_uris=None, source_uri_prefix=None):
        self.ref = ref
        self.schema = list(schema or [])
        # Table.num_rows: BigQuery's stored row count (table metadata, no query).
        self.num_rows = num_rows
        # Table.table_type, and an external table's ExternalConfig: its source
        # URIs and, for a hive table, HivePartitioningOptions.source_uri_prefix.
        self.table_type = table_type
        self.external_data_configuration = None
        if table_type == "EXTERNAL":
            hive = (types.SimpleNamespace(source_uri_prefix=source_uri_prefix)
                    if source_uri_prefix else None)
            self.external_data_configuration = types.SimpleNamespace(
                source_uris=list(source_uris or []), hive_partitioning=hive)


class FakeBigQueryModule:
    """Stand-in for the ``google.cloud.bigquery`` module object."""

    SourceFormat = _SourceFormat
    WriteDisposition = _WriteDisposition
    LoadJobConfig = _LoadJobConfig
    QueryJobConfig = _QueryJobConfig
    ScalarQueryParameter = _ScalarQueryParameter
    SchemaField = _SchemaField
    Table = _Table


# --- client / job doubles ----------------------------------------------------

class FakeClock:
    """``time.monotonic`` and ``time.sleep`` of the adapter's read wait: time moves
    only when the code under test pauses, or when a fake wait stands in for one."""

    def __init__(self):
        self.now = 0.0
        self.pauses: List[float] = []

    def __call__(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.pauses.append(seconds)
        self.now += seconds


class FakeJob:
    def __init__(self, rows=None, raises: Optional[Exception] = None,
                 state: Optional[str] = None, error_result: Optional[Any] = None,
                 num_dml_affected_rows: Optional[int] = None,
                 statement_type: Optional[str] = None,
                 running_polls: Optional[int] = 0, clock: Optional[FakeClock] = None):
        self._rows = rows if rows is not None else []
        self._raises = raises
        # Server-side job status, inspected by the adapter after result() raises
        # to distinguish a truly-failed load job from one that committed but
        # whose result() call timed out client-side.
        self.state = state
        self.error_result = error_result
        # QueryJob.num_dml_affected_rows: None for DDL / SELECT.
        self.num_dml_affected_rows = num_dml_affected_rows
        # QueryJob.statement_type: what BigQuery parsed the SQL as ("SELECT",
        # "DELETE", "SCRIPT", ...); a dry run reports it without running.
        self.statement_type = statement_type
        # result(timeout=) waits seen, and whether cancel() was called.
        self.result_timeouts: List[Any] = []
        self.cancelled = False
        # How many done() polls answer "still running" before DONE; None: never DONE.
        self._running_polls = running_polls
        self._clock = clock
        self.done_timeouts: List[Any] = []

    def done(self, retry=None, timeout=None, reload=True):
        # jobs.get: answers at once with whether the job is DONE.
        self.done_timeouts.append(timeout)
        # A wait that polls without pausing never moves the fake clock: fail, not hang.
        assert len(self.done_timeouts) < 1000, "polled jobs.get 1000 times: the wait does not pause"
        if self._running_polls is None:
            return False
        return len(self.done_timeouts) > self._running_polls

    def reload(self):
        # No-op: fake state is already "server-side" truth.
        return None

    def cancel(self, *a, **kw):
        self.cancelled = True
        return True

    def result(self, max_results=None, timeout=None, **kw):
        self.result_timeouts.append(timeout)
        if self._running_polls is None:
            # A job that never finishes, waited on the way google-cloud-bigquery
            # 3.42.2 QueryJob.result waits: in getQueryResults calls BigQuery holds
            # up to 10 s each, the timeout checked only between calls. So
            # result(timeout=t) raises on the first 10 s step past t.
            steps = 1 if timeout is None else int(timeout // 10) + 1
            if self._clock is not None:
                self._clock.now += 10 * steps
            raise concurrent.futures.TimeoutError()
        if self._raises is not None:
            raise self._raises
        if max_results is None:
            return list(self._rows)
        return list(self._rows)[:max_results]


class FakeClient:
    """Records every interaction so tests can assert routing + SQL + configs."""

    def __init__(self, query_rows: Optional[List[Dict[str, Any]]] = None,
                 load_job_raises: Optional[Exception] = None,
                 load_job_committed: bool = False,
                 insert_errors: Optional[List[Any]] = None,
                 tables: Optional[Dict[str, Any]] = None,
                 answers: Optional[List[Any]] = None,
                 query_job_raises: Optional[Exception] = None,
                 dml_affected_rows: Optional[int] = None,
                 statement_type: Optional[str] = "SELECT",
                 query_job_running_polls: Optional[int] = 0,
                 clock: Optional[FakeClock] = None):
        self._query_rows = query_rows if query_rows is not None else []
        # A query job whose result() raises (a failed statement, or a client
        # wait that ran out), and the DML row count a query job reports.
        self._query_job_raises = query_job_raises
        self._dml_affected_rows = dml_affected_rows
        # The statement type a dry run of any SQL reports.
        self._statement_type = statement_type
        # How long a query job runs, in done() polls (None: it never finishes),
        # and the clock its waits move.
        self._query_job_running_polls = query_job_running_polls
        self.clock = clock if clock is not None else FakeClock()
        # (SQL substring, rows or an Exception to raise): a snapshot query or a
        # pinned page gets its own answer, every other query gets query_rows.
        self._answers = list(answers or [])
        self._load_job_raises = load_job_raises
        # When True, a raising load job still reports server-side success
        # (state DONE, no error_result) — simulating result() timing out after
        # the load actually committed.
        self._load_job_committed = load_job_committed
        self._insert_errors = insert_errors or []
        self._tables = tables or {}
        # get_table raises the Exception mapped to a table id here.
        self.get_table_raises: Dict[str, Exception] = {}
        # call logs
        self.queries: List[str] = []
        self.dry_runs: List[str] = []                 # SQL planned, never run
        self.query_configs: List[Any] = []
        self.query_kwargs: List[Dict[str, Any]] = []   # extra client.query() kwargs
        self.jobs: List[FakeJob] = []                  # every query job returned
        self.loaded: List[Dict[str, Any]] = []       # {fq, content, job_config}
        self.inserted: List[Dict[str, Any]] = []      # {fq, rows}
        self.created_tables: List[Any] = []
        self.deleted_tables: List[str] = []
        # list_datasets answers with these ids; the projects asked are logged.
        self.datasets: List[str] = []
        self.listed_projects: List[str] = []
        # list_tables answers with these ids; the datasets asked are logged.
        self.table_ids: List[str] = []
        self.listed_datasets: List[str] = []
        # Per-dataset answers ("proj.ds" -> ids, or an Exception to raise); a
        # dataset not named here gets table_ids.
        self.tables_by_dataset: Dict[str, Any] = {}
        # list_datasets raises this when set.
        self.list_datasets_raises: Optional[Exception] = None
        # Every table id get_table was asked for, and the timeout it was given.
        self.fetched_tables: List[str] = []
        self.get_table_timeouts: List[Any] = []
        # get_table for a table id here waits until its Event is set (a slow
        # metadata read); bounded so a broken test fails instead of hanging.
        self.get_table_waits: Dict[str, threading.Event] = {}

    # -- query path --
    def query(self, sql, job_config=None, **kw):
        if job_config is not None and getattr(job_config, "extra", {}).get("dry_run"):
            # A dry run plans the SQL and runs nothing: logged apart from queries.
            self.dry_runs.append(sql)
            return FakeJob(statement_type=self._statement_type)
        self.queries.append(sql)
        self.query_configs.append(job_config)
        self.query_kwargs.append(kw)
        for needle, answer in self._answers:
            if needle in sql:
                if isinstance(answer, Exception):
                    raise answer
                job = FakeJob(rows=answer)
                self.jobs.append(job)
                return job
        job = FakeJob(rows=self._query_rows, raises=self._query_job_raises,
                      num_dml_affected_rows=self._dml_affected_rows,
                      running_polls=self._query_job_running_polls, clock=self.clock)
        self.jobs.append(job)
        return job

    # -- streaming insert path --
    def insert_rows_json(self, fq, data):
        self.inserted.append({"fq": fq, "rows": list(data)})
        return list(self._insert_errors)

    # -- bulk load-job path --
    def load_table_from_file(self, fileobj, fq, job_config=None):
        # Faithfully mirror google-cloud-bigquery's real upload contract: the
        # stream MUST be opened in a READABLE binary mode (rb / r+b / w+b). The
        # real client rejects a write-only ("wb") handle with "Cannot upload
        # files opened in text mode". The previous fake accepted ANY handle,
        # which let a real "wb" regression pass the mocks and only fail against
        # live BigQuery — enforce it here so a unit test catches it.
        mode = getattr(fileobj, "mode", None)
        if mode is not None and ("b" not in mode or ("r" not in mode and "+" not in mode)):
            raise ValueError(
                "Cannot upload files opened in text mode:  "
                "use open(filename, mode='rb') or open(filename, mode='r+b')"
            )
        try:
            fileobj.seek(0)
            content = fileobj.read()
        except Exception:
            content = None
        self.loaded.append({"fq": fq, "content": content, "job_config": job_config})
        if self._load_job_raises is not None and self._load_job_committed:
            # result() will raise, but the server-side job actually committed.
            return FakeJob(raises=self._load_job_raises, state="DONE", error_result=None)
        return FakeJob(raises=self._load_job_raises)

    # -- schema evolution helpers (best-effort no-ops) --
    def get_table(self, fq, timeout=None):
        self.fetched_tables.append(fq)
        self.get_table_timeouts.append(timeout)
        if fq in self.get_table_waits:
            self.get_table_waits[fq].wait(5)
        if fq in self.get_table_raises:
            raise self.get_table_raises[fq]
        return self._tables.get(fq, _Table(fq, schema=[]))

    def update_table(self, table, fields):
        self._tables[getattr(table, "ref", None)] = table
        return table

    def create_table(self, table, exists_ok=False):
        self.created_tables.append(table)
        return table

    def delete_table(self, fq, not_found_ok=False):
        self.deleted_tables.append(fq)

    def list_tables(self, dataset):
        self.listed_datasets.append(dataset)
        ids = self.tables_by_dataset.get(dataset, self.table_ids)
        if isinstance(ids, Exception):
            raise ids
        return [types.SimpleNamespace(table_id=t) for t in ids]

    def list_datasets(self, project=None):
        self.listed_projects.append(project)
        if self.list_datasets_raises is not None:
            raise self.list_datasets_raises
        return [types.SimpleNamespace(dataset_id=d) for d in self.datasets]


def make_adapter(warehouse_adapters, *, query_rows=None, load_job_raises=None,
                 load_job_committed=False, insert_errors=None, tables=None, answers=None,
                 query_job_raises=None, dml_affected_rows=None, statement_type="SELECT",
                 query_job_running_polls=0):
    """Build a BigQueryWarehouseAdapter wired to fakes (both seams and the read
    wait's clock swapped)."""
    adapter = warehouse_adapters.BigQueryWarehouseAdapter()
    clock = FakeClock()
    client = FakeClient(query_rows=query_rows, load_job_raises=load_job_raises,
                        load_job_committed=load_job_committed,
                        insert_errors=insert_errors, tables=tables, answers=answers,
                        query_job_raises=query_job_raises,
                        dml_affected_rows=dml_affected_rows,
                        statement_type=statement_type,
                        query_job_running_polls=query_job_running_polls, clock=clock)
    fake_bq = FakeBigQueryModule()
    # Shadow the two seams with instance attributes (dataclass has no __slots__).
    adapter._client = lambda config: client          # type: ignore[assignment]
    adapter._bq = lambda: fake_bq                     # type: ignore[assignment]
    adapter._clock = clock                            # type: ignore[assignment]
    adapter._pause = clock.sleep                      # type: ignore[assignment]
    return adapter, client
