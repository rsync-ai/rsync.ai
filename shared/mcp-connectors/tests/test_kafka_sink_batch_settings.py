"""kafka-mcp-sink start_sink: the destination's CDC batch settings reach the worker.

A relational or warehouse destination sets its CDC batch shape with
`destination_config.batch_max_rows`, `batch_max_mb` and `batch_interval_seconds`.
start_sink must pass them to the Go worker untouched, and must refuse a bad value
itself, before spawning: the worker refuses the same values at startup
(batchSettingsError) and would exit before it could report why.

Every case comes from the worker's own testdata/batch_setting_cases.json, which
cdc_batch_apply_test.go also runs, so the two sides cannot drift apart.
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_SINK_DIR = os.path.abspath(os.path.join(
    _HERE, "..", "internal", "kafka-mcp-sink", "versions", "v1.0.0"
))
_SHARED_CASES = os.path.abspath(os.path.join(
    _HERE, "..", "internal", "kafka-mcp-sink", "worker-src", "cmd", "kafka-sink-worker",
    "testdata", "batch_setting_cases.json",
))


def _load_sink_connector():
    saved_path = list(sys.path)
    saved_base = sys.modules.pop("base_connector", None)
    sys.path.insert(0, _SINK_DIR)
    try:
        spec = importlib.util.spec_from_file_location(
            "kafka_mcp_sink_connector_batch_settings", os.path.join(_SINK_DIR, "connector.py")
        )
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module
    finally:
        sys.path[:] = saved_path
        sys.modules.pop("base_connector", None)
        if saved_base is not None:
            sys.modules["base_connector"] = saved_base


sink = _load_sink_connector()

with open(_SHARED_CASES, encoding="utf-8") as _f:
    SHARED = json.load(_f)


class _FakeProcess:
    pid = 424243

    def poll(self):
        return None


@pytest.fixture
def connector(monkeypatch):
    c = sink.KafkaMCPSinkConnector()
    # The supervisor thread would try to respawn the fake worker; stop it.
    c._supervisor_stop.set()
    spawned = []

    def fake_spawn(worker_config):
        spawned.append(worker_config)
        return _FakeProcess()

    monkeypatch.setattr(c, "_spawn_worker_process", fake_spawn)
    monkeypatch.setattr(c, "_wait_for_worker_ready", lambda port, timeout=5: None)
    monkeypatch.setattr(c, "_is_worker_alive", lambda worker_id: bool(c.workers.get(worker_id)))
    c.spawned = spawned
    return c


def _start(c, destination_config, consumer_group="g-batch"):
    return c.start_sink({"config": {
        "topics": ["cdc.shop.orders"],
        "consumer_group": consumer_group,
        "destination_connector": "postgresql",
        "destination_version": "v1.0.0",
        "destination_config": destination_config,
        "sink_mode": "cdc",
    }})


def test_shared_settings_match_the_connector():
    assert [tuple(s.values()) for s in SHARED["settings"]] == [
        tuple(s) for s in sink.BATCH_SETTINGS
    ], "update the shared cases, the worker's cdc_batch_apply.go and connector.py together"
    kinds = {"accepted": 0, "unset": 0, "refused": 0}
    for case in SHARED["cases"]:
        expect = case["expect"]
        kinds["accepted" if isinstance(expect, int) and not isinstance(expect, bool) else expect] += 1
    assert kinds["accepted"] >= 6 and kinds["unset"] >= 3 and kinds["refused"] >= 15, kinds
    assert {c["key"] for c in SHARED["cases"]} == {s[0] for s in sink.BATCH_SETTINGS}


@pytest.mark.parametrize("case", SHARED["cases"], ids=[c["name"] for c in SHARED["cases"]])
def test_start_sink_agrees_with_the_worker_on_every_shared_case(connector, case):
    dest = {"host": "db", case["key"]: case["value"]}
    res = _start(connector, dest)
    if case["expect"] == "refused":
        assert res.get("success") is False, res
        assert res.get("status") == "invalid_config", res
        setting = next(s for s in SHARED["settings"] if s["key"] == case["key"])
        assert (
            f"{setting['key']} must be a whole number{setting['unit']} "
            f"from {setting['lo']} to {setting['hi']}"
        ) in res["error"], res["error"]
        assert "Clear the setting to use the default." in res["error"]
        assert connector.spawned == [], "a worker was spawned with a refused batch setting"
        assert connector._reserved_ports == set()
    else:
        assert res.get("success") is True, res
        assert len(connector.spawned) == 1
        # Forwarded as given; the worker reads it the way its Go test proves.
        assert connector.spawned[0]["destination_config"] == dest


def test_all_three_settings_travel_together(connector):
    dest = {"host": "db", "batch_max_rows": "5000", "batch_max_mb": 64, "batch_interval_seconds": "30"}
    res = _start(connector, dest)
    assert res.get("success") is True, res
    assert connector.spawned[0]["destination_config"] == dest


def test_the_first_bad_setting_is_the_one_named(connector):
    res = _start(connector, {"batch_max_rows": 10, "batch_max_mb": 0, "batch_interval_seconds": 999})
    assert res.get("status") == "invalid_config", res
    assert res["error"].startswith("batch_max_mb must be a whole number of MB from 1 to 1024 (got '0')"), res["error"]


def test_refusal_echo_is_bounded(connector):
    res = _start(connector, {"batch_max_rows": "x" * 500})
    assert res.get("success") is False
    assert len(res["error"]) < 200, res["error"]


def test_no_batch_setting_starts_the_worker_on_its_defaults(connector):
    for dest in ({"host": "db"}, None):
        connector.spawned.clear()
        res = _start(connector, dest, consumer_group=f"g-{dest is None}")
        assert res.get("success") is True, res
        assert connector.spawned[0]["destination_config"] == dest
