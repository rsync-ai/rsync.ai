#!/usr/bin/env python3
"""Offline unit tests for the sample-data source connector.

Runs with either pytest (`pytest test_connector.py`) or plain stdlib
(`python3 test_connector.py`) — no third-party dependencies, matching the
connector itself.
"""

import json
import os
import select
import socket
import subprocess
import sys
import time
import urllib.request

import connector
from connector import TABLES, SampleDataMCPServer

HERE = os.path.dirname(os.path.abspath(__file__))
SRV = SampleDataMCPServer()


def test_test_connection_ok():
    res = SRV.test_connection({})
    assert res["success"] is True
    assert res["version"] == connector.CONNECTOR_VERSION


def test_validate_config_needs_nothing():
    res = SRV.validate_config({})
    assert res["success"] is True and res["valid"] is True
    assert res["errors"] == []


def test_discover_schema_shape():
    res = SRV.discover_schema({})
    assert res["success"] is True
    assert res["total_tables"] == 3
    names = {t["name"] for t in res["tables"]}
    assert names == {"customers", "orders", "products"}
    for t in res["tables"]:
        assert t["row_count"] == len(TABLES[t["name"]]["rows"])
        assert t["primary_keys"] == TABLES[t["name"]]["primary_key"]
        # every column carries name/type/nullable
        for col in t["columns"]:
            assert set(col.keys()) == {"name", "type", "nullable"}
            # A bare "numeric" means PostgreSQL's unconstrained NUMERIC, which
            # lands as text on most destinations and trips an assessment warning.
            assert col["type"] != "numeric", f"{t['name']}.{col['name']}"


def test_export_full_table_signals_eof():
    res = SRV.export({"table": "customers"})
    assert res["success"] is True
    assert res["row_count"] == 12
    assert len(res["data"]) == 12
    assert res["has_more"] is False  # whole table fits in the default page
    assert res["columns"] == [c["name"] for c in TABLES["customers"]["columns"]]
    # rows are plain dicts carrying every column
    assert set(res["data"][0].keys()) == set(res["columns"])


def test_export_pagination_and_eof_boundary():
    orders = TABLES["orders"]["rows"]
    assert len(orders) == 20

    # first page of 5 -> more remain
    p1 = SRV.export({"table": "orders", "limit": 5, "offset": 0})
    assert p1["row_count"] == 5 and p1["has_more"] is True
    assert p1["data"][0]["order_id"] == orders[0]["order_id"]

    # a full page that lands exactly on the end -> no more
    p_end = SRV.export({"table": "orders", "limit": 5, "offset": 15})
    assert p_end["row_count"] == 5 and p_end["has_more"] is False

    # a short final page -> row_count < limit AND has_more False (double EOF signal)
    p_short = SRV.export({"table": "orders", "limit": 7, "offset": 18})
    assert p_short["row_count"] == 2 and p_short["has_more"] is False

    # offset past the end -> empty page, EOF
    p_past = SRV.export({"table": "orders", "limit": 5, "offset": 100})
    assert p_past["row_count"] == 0 and p_past["has_more"] is False and p_past["data"] == []


def test_export_unknown_table_fails_cleanly():
    res = SRV.export({"table": "not_a_table"})
    assert res["success"] is False
    assert "Unknown table" in res["error"]


def test_export_tolerates_schema_qualified_and_bad_paging():
    # schema-qualified / quoted names resolve to the bare table
    res = SRV.export({"table": '"public"."products"', "limit": "oops", "offset": None})
    assert res["success"] is True
    assert res["row_count"] == 10  # falls back to default limit, offset 0


def test_demo_data_is_referentially_consistent():
    """Guards the datasets so the demo keeps joining cleanly in Explorer."""
    customer_ids = {c["customer_id"] for c in TABLES["customers"]["rows"]}
    product_ids = {p["product_id"] for p in TABLES["products"]["rows"]}
    for o in TABLES["orders"]["rows"]:
        assert o["customer_id"] in customer_ids, f"order {o['order_id']} → missing customer"
        assert o["product_id"] in product_ids, f"order {o['order_id']} → missing product"


# Transport: `python connector.py` the way the orchestrator and the image start it.

def _env(**extra):
    env = {k: v for k, v in os.environ.items() if k not in ("MCP_HTTP_MODE", "DOCKER_CONTAINER", "MCP_PORT", "PORT")}
    env.update(extra)
    return env


def _call(rid, op, arguments):
    return {"jsonrpc": "2.0", "id": rid, "method": "tools/call",
            "params": {"name": f"sample-data_{op}", "arguments": arguments}}


def test_stdio_mode_answers_one_json_line_per_request():
    # The orchestrator's fallback (server_manager.go) starts the connector with
    # MCP_HTTP_MODE=false and no DOCKER_CONTAINER, writes one request per line
    # to stdin and reads one JSON value back from stdout.
    stdin = "\n".join([
        json.dumps(_call(1, "test_connection", {"config": {}})),
        "{not json",
        "",
        json.dumps(_call(2, "export", {"table": "customers", "limit": 2, "offset": 0})),
        json.dumps(_call(3, "drop_table", {})),
    ]) + "\n"
    proc = subprocess.run([sys.executable, "connector.py"], cwd=HERE, input=stdin, capture_output=True,
                          text=True, timeout=20, env=_env(MCP_HTTP_MODE="false"))
    assert proc.returncode == 0, proc.stderr
    out = [json.loads(line) for line in proc.stdout.splitlines()]  # nothing but JSON on stdout
    assert [r["id"] for r in out] == [1, None, 2, 3]
    by_id = {r["id"]: r for r in out}
    assert by_id[1]["result"]["success"] is True
    assert by_id[None]["error"]["code"] == -32700
    assert by_id[2]["result"]["row_count"] == 2 and by_id[2]["result"]["has_more"] is True
    assert by_id[3]["error"]["code"] == -32601


def test_stdio_mode_answers_while_stdin_stays_open():
    # The orchestrator keeps one process per connector and writes each call to
    # its open stdin (client.go executeViaStdio): an answer that waits for stdin
    # to close never arrives.
    proc = subprocess.Popen([sys.executable, "connector.py"], cwd=HERE, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                            env=_env(MCP_HTTP_MODE="false"))
    try:
        for rid in (1, 2):
            proc.stdin.write(json.dumps(_call(rid, "get_capabilities", {})) + "\n")
            proc.stdin.flush()
            ready, _, _ = select.select([proc.stdout], [], [], 10)
            assert ready, f"no answer to request {rid} within 10 s"
            assert json.loads(proc.stdout.readline())["id"] == rid
    finally:
        proc.kill()
        proc.wait(timeout=10)
        for pipe in (proc.stdin, proc.stdout, proc.stderr):
            pipe.close()


def test_http_mode_still_serves_mcp_when_asked():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    proc = subprocess.Popen([sys.executable, "connector.py"], cwd=HERE, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env=_env(MCP_HTTP_MODE="true", MCP_PORT=str(port)))
    try:
        body = json.dumps(_call(7, "get_capabilities", {})).encode()
        deadline = time.monotonic() + 10
        while True:
            try:
                req = urllib.request.Request(f"http://127.0.0.1:{port}/mcp", data=body,
                                             headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(req, timeout=2) as resp:
                    answer = json.loads(resp.read())
                break
            except OSError:
                assert proc.poll() is None and time.monotonic() < deadline, proc.stderr.read() if proc.poll() is not None else "no answer"
                time.sleep(0.1)
        assert answer["id"] == 7 and answer["result"]["capabilities"]["supports_source"] is True
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        proc.stdout.close()
        proc.stderr.close()


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except AssertionError as e:
                failures += 1
                print(f"FAIL {name}: {e}")
    raise SystemExit(1 if failures else 0)
