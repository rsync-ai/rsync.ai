"""The API-spec size limit and its refusals (generator-trust P4b).

THE BUG. A spec over the old 2 MB cap was dropped without a word: the fetch
returned None, discovery moved on, and the generator built a connector from
the docs instead — fewer endpoints, guessed auth — while the run read as a
success. A pasted spec over the request cap got a bare ``request_too_large``.

THE CLASS. A refusal that is either silent or unreadable. Every way a spec
arrives (pasted, by URL, found by discovery) now goes through
``src/utils/spec_limits.py``, which refuses with the spec's size and the limit,
and refuses a spec whose parse would not fit in the memory free right now.

Then the limit by format: JSON up to 30 MB, YAML and XML up to 5 MB, because
a YAML parse peaks at ~45x the spec and a JSON one at ~3.5-5x. And the parse
moved off the event loop, one large spec at a time, so a 25 MB spec neither
freezes tool-generator nor runs beside another one.

This file pins the module itself: the exact wording, that the wording survives
the error scrubber the api-gateway applies, the byte count, the cgroup memory
read (v2 and v1), the streaming read that stops at its format's limit, the
YAML fallback refused over the YAML limit, and ``parse_spec`` — the worker
thread, the one-at-a-time gate and the memory check made when a parse gets its
turn. The routes that raise these refusals are pinned next to the routes.
"""
from __future__ import annotations

import asyncio
import json
import threading
import time

import pytest

from src.utils import spec_limits
from src.utils.masking import scrub_error_for_llm
from src.utils.spec_limits import (
    ANY_SPEC_MAX_BYTES,
    JSON_SPEC_MAX_BYTES,
    LARGE_PARSE_BYTES,
    SPEC_MAX_BYTES,
    SpecMemoryError,
    SpecRefusedError,
    SpecTooLargeError,
    check_parse_memory,
    check_spec_size,
    display_url,
    looks_like_json,
    not_json_message,
    parse_spec,
    read_capped,
    refuse_yaml_fallback,
    too_large_message,
)

_CHUNK = 64 * 1024
_GITHUB_SPEC = (
    "https://raw.githubusercontent.com/github/rest-api-description/main/"
    "descriptions/api.github.com/api.github.com.json"
)


@pytest.fixture
def no_cgroup(monkeypatch, tmp_path):
    """No readable memory limit, whatever host runs the suite."""
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", str(tmp_path / "absent"))


def _cgroup_v2(root, *, limit, current, inactive_file=0):
    root.mkdir(parents=True, exist_ok=True)
    (root / "memory.max").write_text(f"{limit}\n")
    (root / "memory.current").write_text(f"{current}\n")
    (root / "memory.stat").write_text(f"anon 1\nfile 2\ninactive_file {inactive_file}\nactive_file 3\n")
    return str(root)


def _cgroup_v1(root, *, limit, usage, inactive_file=0):
    mem = root / "memory"
    mem.mkdir(parents=True, exist_ok=True)
    (mem / "memory.limit_in_bytes").write_text(f"{limit}\n")
    (mem / "memory.usage_in_bytes").write_text(f"{usage}\n")
    (mem / "memory.stat").write_text(f"cache 1\ntotal_inactive_file {inactive_file}\n")
    return str(root)


# --------------------------------------------------------------------------- #
# The limit and the wording
# --------------------------------------------------------------------------- #


def test_the_limits_are_five_megabytes_and_thirty_for_json():
    assert SPEC_MAX_BYTES == 5_000_000
    assert JSON_SPEC_MAX_BYTES == 30_000_000
    assert ANY_SPEC_MAX_BYTES == 30_000_000


def test_a_pasted_yaml_spec_is_told_its_size_both_limits_and_the_json_way_out():
    assert too_large_message(6_200_000, is_json=False) == (
        "The API spec is 6.2 MB; rsync.ai reads YAML or XML specs up to 5 MB and "
        "JSON specs up to 30 MB. Use the API's JSON spec if it publishes one, or "
        "trim this one to the endpoints you need and try again."
    )


def test_a_fetched_spec_names_the_url_without_its_credentials():
    url = "https://bob:hunter2@api.example.com:8443/v3/openapi.json?token=abc123#paths"
    assert too_large_message(12_000_000, source_url=url, is_json=False) == (
        "The API spec at https://api.example.com:8443/v3/openapi.json is 12 MB; "
        "rsync.ai reads YAML or XML specs up to 5 MB and JSON specs up to 30 MB. "
        "Use the API's JSON spec if it publishes one, or download this one, trim it "
        "to the endpoints you need, and paste it instead."
    )


def test_a_json_spec_over_its_limit_is_told_the_json_limit():
    assert too_large_message(31_000_000, is_json=True) == (
        "The API spec is 31 MB; rsync.ai reads JSON specs up to 30 MB. "
        "Trim it to the endpoints you need and try again."
    )


def test_a_json_stream_cut_off_at_the_limit_says_larger_than():
    assert too_large_message(None, source_url=_GITHUB_SPEC, is_json=True) == (
        f"The API spec at {_GITHUB_SPEC} is larger than 30 MB, the most rsync.ai "
        "reads of a JSON spec. Download it, trim it to the endpoints you need, and "
        "paste it instead."
    )


def test_a_spec_of_unknown_format_is_told_the_larger_limit():
    """A declared length, or a request body, refused before a byte of the spec
    is seen: only the larger limit is certain."""
    assert too_large_message(42_000_000, source_url="https://api.example.com/openapi.json") == (
        "The API spec at https://api.example.com/openapi.json is 42 MB; rsync.ai reads "
        "specs up to 30 MB. Download it, trim it to the endpoints you need, and paste "
        "it instead."
    )
    assert too_large_message(None) == (
        "The API spec is larger than 30 MB, the most rsync.ai reads. "
        "Trim it to the endpoints you need and try again."
    )


@pytest.mark.parametrize(
    "size, is_json, shown",
    [(SPEC_MAX_BYTES + 1, False, "5,000,000 bytes"), (JSON_SPEC_MAX_BYTES + 1, True, "30,000,000 bytes")],
)
def test_one_byte_over_reads_as_bytes_not_as_megabytes_against_the_same(size, is_json, shown):
    """5,000,001 bytes rounds to "5.0 MB": a refusal reading "5.0 MB; up to 5 MB"
    looks like a spec AT the limit being refused."""
    msg = too_large_message(size, is_json=is_json)
    assert f"{size:,} bytes" in msg and f"up to {shown}" in msg, msg


def test_malformed_json_is_told_where_it_breaks():
    try:
        json.loads('{"a": 1\n "b": 2}')
    except json.JSONDecodeError as e:
        error = e
    assert not_json_message(12_000_000, error) == (
        "The API spec is 12 MB and is not valid JSON (line 2, column 2: Expecting a "
        "comma). rsync.ai reads JSON specs up to 30 MB but YAML specs only up to 5 MB. "
        "Fix the JSON and try again."
    )


def _decode_error(text):
    try:
        json.loads(text)
    except json.JSONDecodeError as e:
        return e
    raise AssertionError(f"{text!r} is valid JSON")


@pytest.mark.parametrize("url", ["not a url", "/relative/spec.json", "http://[::1"])
def test_an_unusable_url_is_not_echoed(url):
    assert display_url(url) == "the given URL"


@pytest.mark.parametrize(
    "msg",
    [
        too_large_message(6_200_000, is_json=False),
        too_large_message(SPEC_MAX_BYTES + 1, is_json=False),
        too_large_message(None, source_url=_GITHUB_SPEC, is_json=False),
        too_large_message(12_000_000, source_url="https://api.stripe.com/openapi/spec3.json", is_json=False),
        too_large_message(31_000_000, is_json=True),
        too_large_message(JSON_SPEC_MAX_BYTES + 1, is_json=True),
        too_large_message(None, source_url=_GITHUB_SPEC, is_json=True),
        too_large_message(42_000_000, source_url="https://api.stripe.com/openapi/spec3.json"),
        too_large_message(None),
        not_json_message(SPEC_MAX_BYTES + 1, ValueError("Expecting value")),
        # json's own words: "Expecting ',' delimiter" lost its comma to the
        # scrubber, which takes anything quoted for a literal.
        not_json_message(12_000_000, _decode_error('{"a": 1' + "\n" * 123_456 + ' "b": 2}')),
        not_json_message(12_000_000, _decode_error('{"a" 1}')),
        not_json_message(12_000_000, _decode_error('{"a": 1,}')),
        not_json_message(12_000_000, _decode_error('{"a": 1} x')),
        not_json_message(12_000_000, _decode_error('{"a": "\\q"}')),
    ],
)
def test_the_refusal_survives_the_error_scrubber_unchanged(msg):
    """The api-gateway scrubs a failure before the user sees it (outcomeOf ->
    llmscrub.Scrub). A size the scrubber took for an id, or a URL it took for a
    token, would reach the user as ``[redacted]``."""
    assert scrub_error_for_llm(msg) == msg


def test_the_memory_refusal_survives_the_error_scrubber_unchanged(monkeypatch, tmp_path):
    monkeypatch.setattr(
        spec_limits, "_CGROUP_ROOT", _cgroup_v2(tmp_path, limit=1_000_000_000, current=900_000_000)
    )
    with pytest.raises(SpecMemoryError) as err:
        check_parse_memory(4_000_000, is_json=False)
    assert scrub_error_for_llm(err.value.message) == err.value.message


# --------------------------------------------------------------------------- #
# The size limit — parse_spec checks it before the parser is called
# --------------------------------------------------------------------------- #


def _parse(text):
    """``parse_spec`` with ``len`` as the parser: the length when the spec is read."""
    return asyncio.run(parse_spec(text, len))


def test_no_spec_is_not_refused():
    assert _parse("") == 0


def test_a_spec_over_the_limit_is_refused_413(no_cgroup):
    with pytest.raises(SpecTooLargeError) as err:
        _parse("a" * (SPEC_MAX_BYTES + 1))
    assert err.value.status_code == 413
    assert err.value.code == "spec_too_large"
    assert isinstance(err.value, SpecRefusedError)
    assert err.value.message == too_large_message(SPEC_MAX_BYTES + 1, is_json=False)


def test_a_yaml_spec_over_five_megabytes_is_refused_but_the_same_size_of_json_is_read(no_cgroup):
    yaml_text = "openapi: 3.0.0\n" + "#" * 6_000_000
    json_text = '  {"openapi": "3.0.0", "x": "' + "a" * 6_000_000 + '"}'
    with pytest.raises(SpecTooLargeError) as err:
        _parse(yaml_text)
    assert err.value.message.startswith("The API spec is 6.0 MB; rsync.ai reads YAML or XML specs up to 5 MB")
    assert _parse(json_text) == len(json_text)


def test_a_json_spec_over_thirty_megabytes_is_refused(no_cgroup):
    with pytest.raises(SpecTooLargeError) as err:
        _parse("[" + " " * JSON_SPEC_MAX_BYTES + "]")
    assert err.value.message == too_large_message(JSON_SPEC_MAX_BYTES + 2, is_json=True)


def test_a_json_spec_at_thirty_megabytes_is_read(no_cgroup):
    assert _parse("[" + " " * (JSON_SPEC_MAX_BYTES - 2) + "]") == JSON_SPEC_MAX_BYTES


@pytest.mark.parametrize(
    "text, expected",
    [
        ('{"openapi": "3.0.0"}', True),
        (' \n\t[1]', True),
        ("openapi: 3.0.0", False),
        ("<edmx:Edmx/>", False),
        ("\ufeff{}", False),  # json.loads refuses a BOM too, so YAML's limit
        ("   ", False),
    ],
)
def test_json_is_known_by_its_first_non_space_character(text, expected):
    assert looks_like_json(text) is expected


def test_the_fetch_check_is_size_only(monkeypatch, tmp_path):
    """A fetch refuses on size; the memory check waits for parse_spec, where
    the memory is used — after any other large parse has given it back."""
    monkeypatch.setattr(
        spec_limits, "_CGROUP_ROOT", _cgroup_v2(tmp_path, limit=1_000_000_000, current=990_000_000)
    )
    check_spec_size("openapi: 3.0.0\n" + "#" * 4_000_000)  # would need ~200 MB
    with pytest.raises(SpecTooLargeError):
        check_spec_size("openapi: 3.0.0\n" + "#" * SPEC_MAX_BYTES)


def test_a_spec_at_the_limit_is_read(no_cgroup):
    assert _parse("a" * SPEC_MAX_BYTES) == SPEC_MAX_BYTES


def test_the_size_is_counted_in_bytes_not_characters(no_cgroup):
    """2.5 M characters of a two-byte letter is 5,000,002 bytes on the wire."""
    text = "é" * (SPEC_MAX_BYTES // 2 + 1)
    assert len(text) < SPEC_MAX_BYTES
    with pytest.raises(SpecTooLargeError) as err:
        _parse(text)
    assert "5,000,002 bytes" in err.value.message, err.value.message


# --------------------------------------------------------------------------- #
# refuse_yaml_fallback — text that failed json.loads is not read as YAML when large
# --------------------------------------------------------------------------- #


_MALFORMED = '{"a": 1\n "b": 2'  # no comma between the two members


def test_malformed_json_over_the_yaml_limit_is_not_read_as_yaml():
    text = _MALFORMED + " " * (SPEC_MAX_BYTES - len(_MALFORMED)) + "}"
    with pytest.raises(SpecTooLargeError) as err:
        refuse_yaml_fallback(text, _decode_error(text))
    assert err.value.message.startswith(
        "The API spec is 5,000,001 bytes and is not valid JSON (line 2, column 2: Expecting a comma)."
    ), err.value.message


def test_control_malformed_json_at_the_yaml_limit_may_be_read_as_yaml():
    text = _MALFORMED + " " * (SPEC_MAX_BYTES - len(_MALFORMED) - 1) + "}"
    assert len(text) == SPEC_MAX_BYTES
    refuse_yaml_fallback(text, _decode_error(text))


# --------------------------------------------------------------------------- #
# The memory check
# --------------------------------------------------------------------------- #


def test_a_yaml_parse_that_will_not_fit_is_refused_503(monkeypatch, tmp_path):
    # 1,000 MB limit, 900 MB used -> 100 MB free; 4 MB of YAML needs ~200 MB.
    monkeypatch.setattr(
        spec_limits, "_CGROUP_ROOT", _cgroup_v2(tmp_path, limit=1_000_000_000, current=900_000_000)
    )
    with pytest.raises(SpecMemoryError) as err:
        _parse("openapi: 3.0.0\n" + "#" * 4_000_000)
    assert err.value.status_code == 503
    assert err.value.message == (
        "Parsing this 4.0 MB API spec needs about 200 MB of memory and tool-generator "
        "has 100 MB free right now. Try again in a minute; if it keeps happening, "
        "raise tool-generator's memory limit."
    )


def test_the_same_size_of_json_fits(monkeypatch, tmp_path):
    # JSON parses at ~12x, so the same 4 MB needs ~48 MB of the 100 MB free.
    monkeypatch.setattr(
        spec_limits, "_CGROUP_ROOT", _cgroup_v2(tmp_path, limit=1_000_000_000, current=900_000_000)
    )
    text = '  {"openapi": "3.0.0", "x": "' + "a" * 4_000_000 + '"}'
    assert _parse(text) == len(text)


def test_page_cache_the_kernel_can_drop_counts_as_free(monkeypatch, tmp_path):
    # 960 MB used, 50 MB of it inactive page cache: 90 MB free, not 40 MB.
    root = _cgroup_v2(tmp_path, limit=1_000_000_000, current=960_000_000, inactive_file=50_000_000)
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", root)
    assert spec_limits.available_memory_bytes() == 90_000_000
    check_parse_memory(4_000_000, is_json=True)  # needs 48 MB


def test_no_memory_limit_means_no_memory_refusal(monkeypatch, tmp_path):
    root = tmp_path / "v2"
    _cgroup_v2(root, limit=0, current=900_000_000)
    (root / "memory.max").write_text("max\n")
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", str(root))
    assert spec_limits.available_memory_bytes() is None
    check_parse_memory(SPEC_MAX_BYTES, is_json=False)


def test_cgroup_v1_is_read(monkeypatch, tmp_path):
    root = _cgroup_v1(tmp_path, limit=1_000_000_000, usage=950_000_000, inactive_file=10_000_000)
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", root)
    assert spec_limits.available_memory_bytes() == 60_000_000
    with pytest.raises(SpecMemoryError):
        check_parse_memory(2_000_000, is_json=False)  # needs 100 MB


def test_cgroup_v1_unlimited_means_no_limit(monkeypatch, tmp_path):
    root = _cgroup_v1(tmp_path, limit=9_223_372_036_854_771_712, usage=1)
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", root)
    assert spec_limits.available_memory_bytes() is None


def test_no_cgroup_files_means_no_limit(no_cgroup):
    assert spec_limits.available_memory_bytes() is None


# --------------------------------------------------------------------------- #
# parse_spec — a worker thread, one large parse at a time
# --------------------------------------------------------------------------- #

_LARGE = "a" * LARGE_PARSE_BYTES  # not JSON: YAML's 50x, so 50 MB to parse


def _wait_for(event):
    async def waiter():
        while not event.is_set():
            await asyncio.sleep(0.005)

    return asyncio.wait_for(waiter(), 5)


def test_the_parse_runs_off_the_event_loop(no_cgroup):
    """The parse blocks until a coroutine on the loop releases it, which can
    only happen if the loop is free while the parse runs."""
    started, release = threading.Event(), threading.Event()

    def parse(text):
        started.set()
        assert release.wait(5), "the event loop did not run while the spec was parsed"
        return threading.current_thread() is threading.main_thread()

    async def main():
        task = asyncio.ensure_future(parse_spec('{"openapi": "3.0.0"}', parse))
        await _wait_for(started)
        release.set()
        return await task

    assert asyncio.run(main()) is False


def test_two_large_parses_run_one_at_a_time(no_cgroup):
    running, most, lock = 0, 0, threading.Lock()

    def parse(text):
        nonlocal running, most
        with lock:
            running += 1
            most = max(most, running)
        time.sleep(0.05)
        with lock:
            running -= 1
        return len(text)

    async def main():
        return await asyncio.gather(*(parse_spec(_LARGE, parse) for _ in range(3)))

    assert asyncio.run(main()) == [LARGE_PARSE_BYTES] * 3
    assert most == 1


def test_control_small_parses_are_not_held_up(no_cgroup):
    """Each parse waits for the other to start: both must run at once."""
    both = threading.Barrier(2, timeout=5)

    def parse(text):
        both.wait()
        return text

    async def main():
        return await asyncio.gather(parse_spec("{}", parse), parse_spec("[]", parse))

    assert asyncio.run(main()) == ["{}", "[]"]


def test_a_cancelled_caller_holds_the_gate_until_its_parse_ends(no_cgroup):
    """A thread cannot be stopped, so the first parse's memory is in use until
    it returns; the next large parse must wait for that, not for the caller."""
    started, release, order = threading.Event(), threading.Event(), []

    def first(text):
        started.set()
        release.wait(5)
        order.append("first")
        return 1

    def second(text):
        order.append("second")
        return 2

    async def main():
        a = asyncio.ensure_future(parse_spec(_LARGE, first))
        await _wait_for(started)
        a.cancel()
        with pytest.raises(asyncio.CancelledError):
            await a
        b = asyncio.ensure_future(parse_spec(_LARGE, second))
        await asyncio.sleep(0.05)
        assert not b.done() and order == []
        release.set()
        assert await asyncio.wait_for(b, 5) == 2
        assert order == ["first", "second"]

    asyncio.run(main())


def test_the_memory_check_is_made_when_a_large_parse_gets_its_turn(monkeypatch, tmp_path):
    """Memory free while the first parse runs is not free once it is the next
    one's turn: checked on arrival, the second parse would have been let in."""
    roomy = _cgroup_v2(tmp_path / "roomy", limit=1_000_000_000, current=0)
    tight = _cgroup_v2(tmp_path / "tight", limit=1_000_000_000, current=990_000_000)
    monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", roomy)
    started, release = threading.Event(), threading.Event()

    def first(text):
        started.set()
        release.wait(5)
        return 1

    async def main():
        a = asyncio.ensure_future(parse_spec(_LARGE, first))
        await _wait_for(started)
        b = asyncio.ensure_future(parse_spec(_LARGE, lambda t: pytest.fail("parsed without the memory")))
        await asyncio.sleep(0.05)
        monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", tight)
        release.set()
        assert await a == 1
        with pytest.raises(SpecMemoryError):
            await asyncio.wait_for(b, 5)

    asyncio.run(main())


def test_a_memory_refusal_gives_the_gate_back(monkeypatch, tmp_path):
    monkeypatch.setattr(
        spec_limits, "_CGROUP_ROOT", _cgroup_v2(tmp_path, limit=1_000_000_000, current=990_000_000)
    )

    async def main():
        with pytest.raises(SpecMemoryError):
            await parse_spec(_LARGE, lambda t: pytest.fail("parsed without the memory"))
        monkeypatch.setattr(spec_limits, "_CGROUP_ROOT", str(tmp_path / "absent"))
        return await asyncio.wait_for(parse_spec(_LARGE, len), 5)

    assert asyncio.run(main()) == LARGE_PARSE_BYTES


def test_a_spec_over_its_limit_is_refused_without_parsing(no_cgroup):
    text = "openapi: 3\n" + "#" * 6_000_000
    with pytest.raises(SpecTooLargeError) as err:
        asyncio.run(parse_spec(text, lambda t: pytest.fail("parsed")))
    assert err.value.message == too_large_message(len(text), is_json=False)


def test_a_json_spec_over_the_yaml_limit_is_parsed(no_cgroup):
    text = '{"openapi": "3.0.0", "x": "' + "a" * 6_000_000 + '"}'
    assert asyncio.run(parse_spec(text, json.loads))["openapi"] == "3.0.0"


def test_a_parse_error_reaches_the_caller(no_cgroup):
    with pytest.raises(json.JSONDecodeError):
        asyncio.run(parse_spec("{" + " " * LARGE_PARSE_BYTES, json.loads))


# --------------------------------------------------------------------------- #
# read_capped — the streaming fetch
# --------------------------------------------------------------------------- #


class _Resp:
    def __init__(self, body=b"", headers=None, charset="utf-8", endless=False, fill=b"x"):
        self._body = body
        self.headers = headers or {}
        self.charset_encoding = charset
        self._endless = endless
        self._fill = fill
        self.bytes_read = 0

    async def aiter_bytes(self):
        if self._endless:
            while True:
                self.bytes_read += _CHUNK
                yield self._fill * _CHUNK
        for i in range(0, len(self._body), _CHUNK):
            chunk = self._body[i:i + _CHUNK]
            self.bytes_read += len(chunk)
            yield chunk


def _read(resp, **kw):
    return asyncio.run(read_capped(resp, **kw))


def test_a_declared_length_over_every_limit_is_refused_before_reading():
    resp = _Resp(b"{}", headers={"content-length": "42000000"})
    with pytest.raises(SpecTooLargeError) as err:
        _read(resp, source_url="https://api.example.com/openapi.json")
    assert resp.bytes_read == 0
    assert err.value.message == too_large_message(
        42_000_000, source_url="https://api.example.com/openapi.json"
    )


def test_a_declared_yaml_length_over_the_yaml_limit_is_refused_at_the_first_chunk():
    body = b"openapi: 3.0.0\n"
    resp = _Resp(body, headers={"content-length": "6200000"})
    with pytest.raises(SpecTooLargeError) as err:
        _read(resp, source_url="https://api.example.com/openapi.yaml")
    assert resp.bytes_read == len(body)
    assert err.value.message == too_large_message(
        6_200_000, source_url="https://api.example.com/openapi.yaml", is_json=False
    )


def test_control_a_declared_json_length_over_the_yaml_limit_is_read():
    body = b'{"openapi": "3.0.0"}'
    assert _read(_Resp(body, headers={"content-length": "6200000"})) == body.decode()


def test_whitespace_alone_does_not_decide_the_format():
    body = b" " * _CHUNK + b"\n openapi: 3.0.0\n"
    resp = _Resp(body, headers={"content-length": "6200000"})
    with pytest.raises(SpecTooLargeError) as err:
        _read(resp)
    assert resp.bytes_read == len(body)
    assert err.value.message == too_large_message(6_200_000, is_json=False)


def test_an_undeclared_yaml_body_is_cut_off_at_the_yaml_limit():
    resp = _Resp(endless=True)
    with pytest.raises(SpecTooLargeError) as err:
        _read(resp)
    assert resp.bytes_read <= SPEC_MAX_BYTES + _CHUNK
    assert err.value.message == too_large_message(None, is_json=False)


def test_an_undeclared_json_body_is_cut_off_at_the_json_limit():
    resp = _Resp(endless=True, fill=b"[")
    with pytest.raises(SpecTooLargeError) as err:
        _read(resp)
    assert SPEC_MAX_BYTES + _CHUNK < resp.bytes_read <= JSON_SPEC_MAX_BYTES + _CHUNK
    assert err.value.message == too_large_message(None, is_json=True)


def test_a_body_within_the_limit_is_returned():
    assert _read(_Resp(b'{"openapi": "3.0.0"}', headers={"content-length": "20"})) == '{"openapi": "3.0.0"}'


def test_the_declared_charset_is_used():
    assert _read(_Resp("café".encode("latin-1"), charset="latin-1")) == "café"


def test_an_unknown_charset_falls_back_to_utf8():
    assert _read(_Resp("café".encode("utf-8"), charset="no-such-codec")) == "café"


def test_a_garbage_content_length_is_ignored():
    assert _read(_Resp(b"{}", headers={"content-length": "lots"})) == "{}"
