#!/usr/bin/env python3
"""Claim-check payload gzip: on by default, opt out with RSYNC_CLAIM_CHECK_GZIP=false.

This connector is the only component that turns the structured claim-check payload
into bytes and back: the orchestrator writes through upload_for_staging and the
Kafka sink reads through read_from_staging, both over MCP, and neither sees the
object. So compression lives entirely here.

  WRITE (upload_for_staging): gzip the JSON body unless the flag says false.
  READ  (read_from_staging): always magic-byte autodetect (0x1f 0x8b), so an object
    written before a flip, either way, still reads back.

Offline: an in-memory S3 stand-in replaces the boto3 client, so the real upload and
read code runs with no MinIO, no boto3 and no network.

Run: pytest -q test_claim_check_gzip.py
"""
import gzip
import io
import json

import pytest

import connector

GZIP_MAGIC = b"\x1f\x8b"


class _FakeS3:
    """The calls the connector makes, kept in memory."""

    def __init__(self):
        self.store = {}

    def put_object(self, Bucket=None, Key=None, Body=None, **kwargs):
        self.store[(Bucket, Key)] = (Body, kwargs)
        return {}

    def get_object(self, Bucket=None, Key=None, **kwargs):
        body, _ = self.store[(Bucket, Key)]
        return {"Body": io.BytesIO(body)}


# Shaped like a real claim-check body: a wrapper plus a rows array, with the values
# a JSON round trip most often gets wrong.
PAYLOAD = {
    "table": "orders",
    "pipeline_id": "pl-1",
    "execution_id": "ex-1",
    "primary_keys": ["id"],
    "row_count": 3,
    "data": [
        {"id": 1, "name": "münchen ☃", "note": "a,b\"c\nd\there", "amount": "123.45",
         "active": True, "ratio": 0.1 + 0.2, "meta": {"k": ["v", 1, None]}, "raw": None},
        {"id": 2, "name": None, "note": "", "amount": None,
         "active": False, "ratio": -1.5e10, "meta": None, "raw": "trailing "},
        {"id": 3, "name": "x" * 5000, "note": "big", "amount": "0.00",
         "active": None, "ratio": 3.141592653589793, "meta": [1, 2, 3], "raw": "z"},
    ],
}

CFG = {"config": {"bucket": "pipeline-data", "endpoint_url": "http://fake:9000",
                  "access_key_id": "k", "secret_access_key": "s"}}

PLAIN = json.dumps(PAYLOAD, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def _server():
    fake = _FakeS3()
    srv = connector.MinioMCPServer()
    srv._client = lambda cfg: fake
    return srv, fake


def _upload(key):
    srv, fake = _server()
    up = srv.upload_for_staging({**CFG, "data": PAYLOAD, "key": key})
    assert up["success"] is True, up
    body, kwargs = fake.store[("pipeline-data", key)]
    return srv, up, body, kwargs


def test_unset_flag_gzips(monkeypatch):
    """The default is the case this change is about: nobody sets the flag."""
    monkeypatch.delenv("RSYNC_CLAIM_CHECK_GZIP", raising=False)
    srv, up, body, kwargs = _upload("staging/default.json")

    assert body[:2] == GZIP_MAGIC
    assert kwargs.get("ContentEncoding") == "gzip"
    assert up["gzip"] is True
    assert up["bytes"] == len(body) < len(PLAIN)
    assert json.loads(gzip.decompress(body).decode("utf-8")) == PAYLOAD

    rd = srv.read_from_staging({**CFG, "claim_check_url": up["claim_check_url"]})
    assert rd["success"] is True and rd["data"] == PAYLOAD


@pytest.mark.parametrize("val", ["", "true", "TRUE", " yes "])
def test_empty_or_truthy_flag_gzips(monkeypatch, val):
    # docker-compose renders an unset ${RSYNC_CLAIM_CHECK_GZIP:-...} as "" when an
    # operator blanks it, which must not quietly turn compression off.
    monkeypatch.setenv("RSYNC_CLAIM_CHECK_GZIP", val)
    _, up, body, _ = _upload("staging/on.json")
    assert body[:2] == GZIP_MAGIC and up["gzip"] is True


@pytest.mark.parametrize("val", ["false", "False", "0", "no", "off", " false "])
def test_opt_out_writes_plain_json(monkeypatch, val):
    monkeypatch.setenv("RSYNC_CLAIM_CHECK_GZIP", val)
    srv, up, body, kwargs = _upload("staging/plain.json")

    assert body == PLAIN
    assert "ContentEncoding" not in kwargs
    assert up["gzip"] is False and up["bytes"] == len(PLAIN)

    rd = srv.read_from_staging({**CFG, "claim_check_url": up["claim_check_url"]})
    assert rd["success"] is True and rd["data"] == PAYLOAD


@pytest.mark.parametrize("flag", ["true", "false"])
def test_reader_ignores_the_flag(monkeypatch, flag):
    """Objects staged before a flip, in either direction, must still read back."""
    monkeypatch.setenv("RSYNC_CLAIM_CHECK_GZIP", flag)
    srv, fake = _server()
    fake.store[("pipeline-data", "staging/legacy.json")] = (PLAIN, {})
    fake.store[("pipeline-data", "staging/new.json")] = (gzip.compress(PLAIN), {"ContentEncoding": "gzip"})

    for key, stored in (("legacy", len(PLAIN)), ("new", len(gzip.compress(PLAIN)))):
        rd = srv.read_from_staging({**CFG, "claim_check_url": f"s3://pipeline-data/staging/{key}.json"})
        assert rd["success"] is True and rd["data"] == PAYLOAD, key
        assert rd["bytes"] == stored, key
