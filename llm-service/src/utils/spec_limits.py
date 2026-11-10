"""The size limits on an API spec the connector generator reads, and its refusals.

The same limits for every way a spec arrives: pasted inline, fetched from
``openapi_spec_url``, or found by discovery (well-known path, docs link). A spec
over its limit is REFUSED with a message that names its size and the limit —
never dropped quietly so generation falls through to scraping the docs and
builds a worse connector without saying why.

Two limits, because the two formats cost very different amounts to parse
(measured on generated specs: JSON peaks at ~3.5-5x its size and parses 47 MB
in 0.2 s in C; PyYAML's pure-Python loader peaks at ~45x and takes 9 s for
10 MB). A spec whose first non-space character is ``{`` or ``[`` is JSON and
may be up to JSON_SPEC_MAX_BYTES; anything else (YAML, OData XML) up to
SPEC_MAX_BYTES. Text that looks like JSON but is not valid JSON is read as YAML
only within SPEC_MAX_BYTES (``refuse_yaml_fallback``).

Parsing also runs under tool-generator's container memory limit, so a spec
within its size limit is checked against the memory free right now.
``parse_spec`` parses in a worker thread, so the event loop keeps serving other
requests, and lets one large spec parse at a time, with the memory check made
once it is that spec's turn.

Stdlib only — the lifecycle image (Dockerfile.oss) ships this file.
"""
from __future__ import annotations

import asyncio
import json
import os
import re
from typing import Callable, Optional, TypeVar
from urllib.parse import urlsplit


def _env_int(name: str, default: int) -> int:
    raw = (os.getenv(name, "") or "").strip()
    if raw == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


# A spec that is not JSON: YAML, OData XML.
SPEC_MAX_BYTES = _env_int("RSYNC_LLM_MAX_SPEC_BYTES", 5_000_000)
JSON_SPEC_MAX_BYTES = _env_int("RSYNC_LLM_MAX_JSON_SPEC_BYTES", 30_000_000)
ANY_SPEC_MAX_BYTES = max(SPEC_MAX_BYTES, JSON_SPEC_MAX_BYTES)

# From this size a parse waits for any other large parse to finish first.
LARGE_PARSE_BYTES = 1_000_000

# Peak parse memory per spec byte, rounded up from the measured ratios.
JSON_PARSE_FACTOR = 12
YAML_PARSE_FACTOR = 50

_CGROUP_ROOT = "/sys/fs/cgroup"
# cgroup v1 reports "no limit" as a huge page-aligned number, not a word.
_CGROUP_V1_UNLIMITED = 1 << 60


class SpecRefusedError(Exception):
    """A spec the generator will not read. ``message`` is written for the user."""

    status_code = 400
    code = "spec_refused"

    def __init__(self, message: str):
        super().__init__(message)
        self.message = message


class SpecTooLargeError(SpecRefusedError):
    status_code = 413
    code = "spec_too_large"


class SpecMemoryError(SpecRefusedError):
    status_code = 503
    code = "spec_memory"


class SpecUnreadableError(SpecRefusedError):
    """A pasted spec that does not parse. Refused rather than guessed around:
    falling back to network discovery generated a connector from whatever the
    docs host served, not from the document the user gave."""

    status_code = 422
    code = "spec_unreadable"


def _mb(n: int) -> str:
    """Decimal megabytes: a whole number as ``5 MB``, otherwise one decimal."""
    if n % 1_000_000 == 0:
        return f"{n // 1_000_000} MB"
    return f"{n / 1_000_000:.1f} MB"


def _size_and_limit(size: int, limit: int) -> tuple[str, str]:
    # 5,000,001 bytes reads "5.0 MB" against a "5 MB" limit, which looks like a
    # spec AT the limit being refused. Show bytes then — comma-grouped, so the
    # error scrubber does not take the digits for an id.
    if round(size / 1_000_000, 1) == round(limit / 1_000_000, 1):
        return f"{size:,} bytes", f"{limit:,} bytes"
    return _mb(size), _mb(limit)


def display_url(url: str) -> str:
    """``url`` without user info, query or fragment — where a credential would sit."""
    try:
        parts = urlsplit(url)
    except ValueError:
        return "the given URL"
    if not parts.scheme or not parts.hostname:
        return "the given URL"
    host = parts.hostname
    if parts.port:
        host = f"{host}:{parts.port}"
    return f"{parts.scheme}://{host}{parts.path}"


def limit_for(is_json: Optional[bool]) -> int:
    """The size limit for a JSON spec, for one that is not JSON, or — format
    unknown (None) — the larger of the two."""
    if is_json is None:
        return ANY_SPEC_MAX_BYTES
    return JSON_SPEC_MAX_BYTES if is_json else SPEC_MAX_BYTES


def too_large_message(
    size: Optional[int], *, source_url: Optional[str] = None, is_json: Optional[bool] = None
) -> str:
    """The refusal for a spec over ``limit_for(is_json)``. ``size`` is None when
    only "more than the limit" is known (a stream cut off at the limit)."""
    limit = limit_for(is_json)
    if size is None:
        size_s, limit_s = None, _mb(limit)
    else:
        size_s, limit_s = _size_and_limit(size, limit)
    if is_json is False:
        # The one case a user can fix without trimming: the same API's JSON spec.
        bound = f"is larger than {limit_s}" if size_s is None else f"is {size_s}"
        what = (
            f"{bound}; rsync.ai reads YAML or XML specs up to {limit_s} and JSON specs "
            f"up to {_mb(JSON_SPEC_MAX_BYTES)}"
        )
        if source_url:
            return (
                f"The API spec at {display_url(source_url)} {what}. Use the API's JSON spec "
                f"if it publishes one, or download this one, trim it to the endpoints you "
                f"need, and paste it instead."
            )
        return (
            f"The API spec {what}. Use the API's JSON spec if it publishes one, or trim "
            f"this one to the endpoints you need and try again."
        )
    kind = "JSON specs" if is_json else "specs"
    if size_s is None:
        what = f"is larger than {limit_s}, the most rsync.ai reads"
        if is_json:
            what += " of a JSON spec"
    else:
        what = f"is {size_s}; rsync.ai reads {kind} up to {limit_s}"
    if source_url:
        return (
            f"The API spec at {display_url(source_url)} {what}. Download it, trim it "
            f"to the endpoints you need, and paste it instead."
        )
    return f"The API spec {what}. Trim it to the endpoints you need and try again."


_QUOTED_DELIMITER = re.compile(r"'(.)' delimiter")
_DELIMITER_WORDS = {",": "a comma", ":": "a colon"}


def not_json_message(size: int, error: Exception) -> str:
    """The refusal for text over SPEC_MAX_BYTES that is not valid JSON."""
    size_s, limit_s = _size_and_limit(size, SPEC_MAX_BYTES)
    where = ""
    if isinstance(error, json.JSONDecodeError):
        # Positions comma-grouped so the error scrubber does not take them for
        # ids, and no quotes, which it takes for a literal: "Expecting ','
        # delimiter" would reach the user as "Expecting '[redacted]' delimiter".
        what = _QUOTED_DELIMITER.sub(lambda m: _DELIMITER_WORDS.get(m.group(1), "a delimiter"), error.msg)
        where = f" (line {error.lineno:,}, column {error.colno:,}: {what})"
    return (
        f"The API spec is {size_s} and is not valid JSON{where}. rsync.ai reads JSON "
        f"specs up to {_mb(JSON_SPEC_MAX_BYTES)} but YAML specs only up to {limit_s}. "
        f"Fix the JSON and try again."
    )


def _read_int(path: str) -> Optional[int]:
    try:
        with open(path, encoding="ascii") as fh:
            raw = fh.read().strip()
    except OSError:
        return None
    if raw == "max":
        return None
    try:
        return int(raw)
    except ValueError:
        return None


def _stat_value(path: str, key: str) -> int:
    try:
        with open(path, encoding="ascii") as fh:
            for line in fh:
                name, _, value = line.partition(" ")
                if name == key:
                    return int(value)
    except (OSError, ValueError):
        pass
    return 0


def available_memory_bytes() -> Optional[int]:
    """Memory this container can still use, or None when it has no limit (or
    none can be read). Page cache the kernel can drop counts as free."""
    root = _CGROUP_ROOT
    limit = _read_int(os.path.join(root, "memory.max"))
    if limit is not None:  # cgroup v2
        used = _read_int(os.path.join(root, "memory.current"))
        if used is None:
            return None
        used -= _stat_value(os.path.join(root, "memory.stat"), "inactive_file")
        return max(0, limit - max(0, used))
    v1 = os.path.join(root, "memory")
    limit = _read_int(os.path.join(v1, "memory.limit_in_bytes"))
    if limit is None or limit >= _CGROUP_V1_UNLIMITED:
        return None
    used = _read_int(os.path.join(v1, "memory.usage_in_bytes"))
    if used is None:
        return None
    used -= _stat_value(os.path.join(v1, "memory.stat"), "total_inactive_file")
    return max(0, limit - max(0, used))


def _mb_fixed(n: int, *, decimals: int = 0) -> str:
    return f"{n / 1_000_000:.{decimals}f} MB"


def check_parse_memory(size: int, *, is_json: bool) -> None:
    """Refuse a spec whose parse would not fit in the memory free right now."""
    needed = size * (JSON_PARSE_FACTOR if is_json else YAML_PARSE_FACTOR)
    free = available_memory_bytes()
    if free is None or needed <= free:
        return
    raise SpecMemoryError(
        f"Parsing this {_mb_fixed(size, decimals=1)} API spec needs about "
        f"{_mb_fixed(needed)} of memory and tool-generator has {_mb_fixed(free)} "
        f"free right now. Try again in a minute; if it keeps happening, raise "
        f"tool-generator's memory limit."
    )


_FIRST_NON_SPACE = re.compile(r"\s*(\S)")
_FIRST_NON_SPACE_BYTE = re.compile(rb"\s*(\S)")


def spec_size(text: str) -> int:
    """UTF-8 size of ``text`` without encoding it when it is plain ASCII."""
    return len(text) if text.isascii() else len(text.encode("utf-8"))


def looks_like_json(text: str) -> bool:
    """True when the first non-space character opens a JSON object or array.
    A byte-order mark makes it False, which is also how json.loads sees it."""
    m = _FIRST_NON_SPACE.match(text)
    return bool(m) and m.group(1) in "{["


def _check_size(text: str, source_url: Optional[str]) -> tuple[int, bool]:
    size = spec_size(text)
    is_json = looks_like_json(text)
    if size > limit_for(is_json):
        raise SpecTooLargeError(too_large_message(size, source_url=source_url, is_json=is_json))
    return size, is_json


def check_spec_size(text: Optional[str], *, source_url: Optional[str] = None) -> None:
    """Raise SpecTooLargeError for a spec over its format's limit. A fetch calls
    this; the memory check waits for ``parse_spec``, where the parse happens."""
    if text:
        _check_size(text, source_url)


def refuse_yaml_fallback(text: str, error: Exception) -> None:
    """Call before reading text that failed json.loads as YAML: over
    SPEC_MAX_BYTES that parse would need ~45x the text in memory, so refuse."""
    size = spec_size(text)
    if size > SPEC_MAX_BYTES:
        raise SpecTooLargeError(not_json_message(size, error))


_T = TypeVar("_T")

# One large parse at a time, per event loop (tool-generator runs one).
_gate_loop: Optional[asyncio.AbstractEventLoop] = None
_gate: Optional[asyncio.Semaphore] = None


def _large_parse_gate() -> asyncio.Semaphore:
    global _gate_loop, _gate
    loop = asyncio.get_running_loop()
    if _gate_loop is not loop or _gate is None:
        _gate_loop, _gate = loop, asyncio.Semaphore(1)
    return _gate


async def parse_spec(
    text: str, parse: Callable[[str], _T], *, source_url: Optional[str] = None
) -> _T:
    """``parse(text)`` in a worker thread, after the size check and — for a spec
    of LARGE_PARSE_BYTES or more, once no other large parse is running — the
    memory check. Raises what ``parse`` raises."""
    size, is_json = _check_size(text, source_url)
    if size < LARGE_PARSE_BYTES:
        check_parse_memory(size, is_json=is_json)
        return await asyncio.to_thread(parse, text)
    gate = _large_parse_gate()
    await gate.acquire()
    try:
        check_parse_memory(size, is_json=is_json)
        task = asyncio.ensure_future(asyncio.to_thread(parse, text))
    except BaseException:
        gate.release()
        raise

    def _done(t: "asyncio.Future[_T]") -> None:
        # The thread cannot be stopped, so a cancelled caller leaves the gate
        # held until the parse really ends — the memory is in use until then.
        gate.release()
        if not t.cancelled():
            t.exception()  # retrieved, so a caller gone by now logs no warning

    task.add_done_callback(_done)
    return await asyncio.shield(task)


async def read_capped(resp, *, source_url: Optional[str] = None) -> str:
    """Read a streamed httpx response, refusing once it passes its format's
    limit — so an oversize body is never held whole in memory. The format is
    known from the first non-space byte; until then the larger limit applies."""
    try:
        declared = int((getattr(resp, "headers", None) or {}).get("content-length"))
    except (TypeError, ValueError):
        declared = None
    if declared is not None and declared > ANY_SPEC_MAX_BYTES:
        raise SpecTooLargeError(too_large_message(declared, source_url=source_url))
    is_json: Optional[bool] = None
    limit = ANY_SPEC_MAX_BYTES
    chunks = []
    total = 0
    async for chunk in resp.aiter_bytes():
        total += len(chunk)
        chunks.append(chunk)
        if is_json is None:
            # Chunk by chunk: "{" and "[" are one byte, and a chunk of only
            # whitespace has no match.
            m = _FIRST_NON_SPACE_BYTE.match(chunk)
            if m:
                is_json = m.group(1) in b"{["
                limit = limit_for(is_json)
                if declared is not None and declared > limit:
                    raise SpecTooLargeError(
                        too_large_message(declared, source_url=source_url, is_json=is_json)
                    )
        if total > limit:
            raise SpecTooLargeError(too_large_message(None, source_url=source_url, is_json=is_json))
    body = b"".join(chunks)
    encoding = getattr(resp, "charset_encoding", None) or "utf-8"
    try:
        return body.decode(encoding, errors="replace")
    except LookupError:
        return body.decode("utf-8", errors="replace")
