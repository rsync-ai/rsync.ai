"""Field classes: what each prompt variable is, and the shape it may take.

Every variable a prompt template uses is tagged in that prompt's YAML under
``llm_fields: {variable: class}``. A class names where the value comes from;
its projection bounds what the value may look like on the way into a prompt.
A value that does not fit is refused, never passed through.

The classes, and what each lets through:

``identifier``
    Names: pipeline, table, column and field ids. Strings of at most 256
    characters with no line break, and lists or maps of them -- also when the
    caller hands the list or map over as JSON text.
``catalog``
    Table metadata. Keeps schema, name, alias, primary key and, per column,
    name, type and the primary-key and nullable flags. Everything else is
    dropped: row counts, sizes, comments, samples, statistics. A declared type
    that quotes values -- MySQL ``enum('active','banned')``, ClickHouse
    ``Enum8('a' = 1)`` -- keeps only the type name.
``enum``
    A short setting: a string of at most 64 characters, a boolean, or an
    integer under 1000 (``max_tables``).
``user_text``
    What the user typed. It is theirs to send.
``rsync_text``
    Text rsync.ai wrote, such as a validator's message. Passed through the error
    scrubber, because a message about a query can quote the query, and then
    through the text mask below.
``sql``
    SQL with every literal replaced by a placeholder and its comments dropped
    (:mod:`.sql_placeholders`). SQL that cannot be read for its dialect is
    withheld.
``model_output``
    What a model returned on an earlier turn of the same task.
``config_keys``
    A configuration map, reduced to its sorted key names.
``connections``
    Connection records, reduced to id, name and connector type.
``evidence``
    A pipeline-diagnosis evidence object: free text scrubbed, counts and
    sizes turned into yes/no, every string through the text mask, then JSON.

The text mask, for ``rsync_text`` and evidence strings, after the error
scrubber. The scrubber (shared with Go through shared/scrubber_golden.json)
takes digit runs of seven or more and IP addresses, which leaves a count
written for people -- ``7,340,033 rows``, ``7.3M``, ``12k events``, ``4 GiB``
-- and a host name. Counts become ``[count]``; zero is kept, so ``0 rows``
still reads as nothing arrived, the one thing a diagnosis needs to know about
volume. Bare small numbers (a line number, SQLSTATE ``23505``) are not counts
and stay. A URL's host, a dotted ``host:port`` and a dotted host name where
error text puts one (``lookup db.example.com``, ``dial tcp …``, ``host name
"…"``, ``server at "…"``) become ``[host]``. A dotted name anywhere else is
kept, because ``schema.table`` has the same shape and a diagnosis needs table
names.

Every class then goes through the guard's secret scrub like the rest of the
prompt.
"""

from __future__ import annotations

import json
import re
from typing import Any, Optional

from src.utils.masking import HOST_IN_ERROR_TEXT, connections_for_llm, scrub_error_for_llm, scrub_evidence_for_llm

from .sql_placeholders import LiteralMap, SqlPlaceholderError, placeholder_sql

__all__ = ["FIELD_CLASSES", "FieldRefused", "WITHHELD_SQL", "mask_counts", "mask_endpoints", "project"]

FIELD_CLASSES = (
    "identifier",
    "catalog",
    "enum",
    "user_text",
    "rsync_text",
    "sql",
    "model_output",
    "config_keys",
    "connections",
    "evidence",
)

WITHHELD_SQL = "(withheld: this SQL could not be read safely)"

_MAX_IDENTIFIER = 256
_MAX_ENUM = 64
_MAX_SMALL_INT = 1000


class FieldRefused(ValueError):
    """A variable's value does not fit its class."""


def _identifier(value: Any, where: str) -> Any:
    if value is None or isinstance(value, bool):
        return value
    if isinstance(value, str):
        if value[:1] in ("[", "{"):
            try:
                data = json.loads(value)
            except ValueError:
                data = None
            if isinstance(data, (list, dict)):
                return json.dumps(_identifier(data, where))
        if len(value) > _MAX_IDENTIFIER or "\n" in value or "\r" in value:
            raise FieldRefused(f"{where} is not an identifier")
        return value
    if isinstance(value, (list, tuple)):
        return [_identifier(v, where) for v in value]
    if isinstance(value, dict):
        return {_identifier(str(k), where): _identifier(v, where) for k, v in value.items()}
    raise FieldRefused(f"{where} is a {type(value).__name__}, not an identifier")


_TABLE_KEYS = ("schema", "schema_name", "name", "table", "table_name", "alias")
_TABLE_KEY_LISTS = ("primary_key", "primary_keys")
_COLUMN_KEYS = ("name", "type", "data_type")
_COLUMN_FLAGS = ("is_primary_key", "is_nullable")


def _column_type(value: Any, where: str) -> Any:
    declared = _identifier(value, where)
    # Quotes in a declared type surround values: enum and set labels.
    if isinstance(declared, str) and ("'" in declared or '"' in declared):
        declared = declared.split("(", 1)[0].strip()
    return declared


def _column(col: Any, where: str) -> Any:
    if isinstance(col, str):
        return _identifier(col, where)
    if not isinstance(col, dict):
        raise FieldRefused(f"{where} has a column that is neither a name nor an object")
    out: dict = {}
    for key in _COLUMN_KEYS:
        if key in col:
            out[key] = _column_type(col[key], where) if key != "name" else _identifier(col[key], where)
    for key in _COLUMN_FLAGS:
        if key in col:
            out[key] = bool(col[key])
    return out


def _table(table: Any, where: str) -> dict:
    if not isinstance(table, dict):
        raise FieldRefused(f"{where} has a table that is not an object")
    out: dict = {}
    for key in _TABLE_KEYS:
        if key in table:
            out[key] = _identifier(table[key], where)
    for key in _TABLE_KEY_LISTS:
        if key in table:
            out[key] = _identifier(table[key], where)
    if "columns" in table:
        cols = table["columns"] or []
        if not isinstance(cols, (list, tuple)):
            raise FieldRefused(f"{where} has columns that are not a list")
        out["columns"] = [_column(c, where) for c in cols]
    return out


def _catalog(value: Any, where: str) -> Any:
    if value is None:
        return None
    if isinstance(value, dict):
        return _table(value, where)
    if isinstance(value, (list, tuple)):
        return [_table(t, where) for t in value]
    raise FieldRefused(f"{where} is not table metadata")


def _enum(value: Any, where: str) -> Any:
    if value is None or isinstance(value, bool):
        return value
    if isinstance(value, int):
        if abs(value) >= _MAX_SMALL_INT:
            raise FieldRefused(f"{where} is too large a number for a setting")
        return value
    if isinstance(value, str):
        if len(value) > _MAX_ENUM or "\n" in value or "\r" in value:
            raise FieldRefused(f"{where} is not a short setting")
        return value
    raise FieldRefused(f"{where} is a {type(value).__name__}, not a setting")


def _user_text(value: Any, where: str) -> Any:
    if value is None or isinstance(value, str):
        return value
    raise FieldRefused(f"{where} is a {type(value).__name__}, not text")


_NUMBER = r"\d+(?:\.\d+)?"
_COUNT_PATTERNS = (
    # 7,340,033 -- digits grouped for reading.
    re.compile(r"(?<![\w.])\d{1,3}(?:,\d{3})+(?:\.\d+)?(?![\w,])"),
    # 7.3M, 12k, 4 GiB, 2.1 billion.
    re.compile(
        rf"(?<![\w.]){_NUMBER}\s?(?:[KkMBT]|[KMGTP]i?B|thousand|million|billion|trillion|mn|bn)\b"
    ),
    # 45000 rows, 12 events.
    re.compile(
        rf"(?<![\w.]){_NUMBER}(?=\s*(?:rows?|records?|messages?|events?|documents?|docs|items|entries|"
        r"bytes|objects|files|batches|lines|tuples|keys)\b)",
        re.IGNORECASE,
    ),
)


def _count_or_zero(match: re.Match) -> str:
    digits = re.sub(r"[^\d]", "", match.group(0))
    return match.group(0) if digits and int(digits) == 0 else "[count]"


def mask_counts(text: str) -> str:
    """Replace a count written for people with ``[count]``; keep zero."""
    for pattern in _COUNT_PATTERNS:
        text = pattern.sub(_count_or_zero, text)
    return text


# The scheme is capped at 32 characters, as in the error scrubber's URL rule:
# uncapped, a long dotted run was read again from each of its starts.
_URL_HOST = re.compile(r"\b([A-Za-z][A-Za-z0-9+.\-]{0,31}://)[^\s/'\"?#<>]+")
# db.example.com:5432 -- but not diagnose.go:95, a source position in a log line.
_HOST_PORT = re.compile(
    r"(?<![\w.\-])(?:[A-Za-z0-9\-]+\.)+(?!(?:go|py|java|js|ts|rs|kt|scala|rb|c|h|cc|cpp):)"
    r"[A-Za-z][A-Za-z0-9\-]*:\d{2,5}\b"
)


def mask_endpoints(text: str) -> str:
    """Replace a URL's host, a dotted ``host:port`` and a dotted host name after
    ``lookup`` / ``dial tcp`` / ``host name "`` / ``server at "`` with ``[host]``."""
    text = _HOST_PORT.sub("[host]", _URL_HOST.sub(r"\1[host]", text))
    return HOST_IN_ERROR_TEXT.sub(r"\1[host]", text)


def _mask_text(text: str) -> str:
    return mask_counts(mask_endpoints(text))


def _rsync_text(value: Any, where: str) -> Any:
    if value is None or isinstance(value, bool):
        return value
    if isinstance(value, str):
        return _mask_text(scrub_error_for_llm(value))
    if isinstance(value, (int, float)):
        # Line and column positions pass; anything count-sized does not.
        return value if abs(value) < _MAX_SMALL_INT else "[num-redacted]"
    if isinstance(value, (list, tuple)):
        return [_rsync_text(v, where) for v in value]
    if isinstance(value, dict):
        return {str(k): _rsync_text(v, where) for k, v in value.items()}
    raise FieldRefused(f"{where} is a {type(value).__name__}, not text")


def _model_output(value: Any, where: str) -> Any:
    if value is None or isinstance(value, (str, bool, int, float, list, dict)):
        return value
    raise FieldRefused(f"{where} is a {type(value).__name__}, not model output")


def _load_json(value: Any, where: str) -> tuple[Any, bool]:
    if isinstance(value, str):
        try:
            return json.loads(value), True
        except ValueError as exc:
            raise FieldRefused(f"{where} is not JSON") from exc
    return value, False


def _config_keys(value: Any, where: str) -> Any:
    if value is None:
        return None
    data, _ = _load_json(value, where)
    if not isinstance(data, dict):
        raise FieldRefused(f"{where} is not a configuration map")
    return sorted(_identifier(str(k), where) for k in data)


def _connections(value: Any, where: str) -> Any:
    data, was_text = _load_json(value, where)
    if data is not None and not isinstance(data, list):
        raise FieldRefused(f"{where} is not a list of connections")
    projected = [
        {k: _identifier(v, where) for k, v in c.items()} for c in connections_for_llm(data)
    ]
    return json.dumps(projected) if was_text else projected


# Evidence keys whose numbers measure the customer's data: how many rows,
# records, bytes or messages. Only whether they are zero reaches a prompt.
_EVIDENCE_COUNT_KEY = re.compile(
    r"count|rows?\b|rows_|records?|bytes|size|lag|offset|total|messages|events|backlog|num_|_num",
    re.IGNORECASE,
)


def _evidence_counts(value: Any, key: str = "") -> Any:
    if isinstance(value, dict):
        return {k: _evidence_counts(v, str(k)) for k, v in value.items()}
    if isinstance(value, list):
        return [_evidence_counts(v, key) for v in value]
    if isinstance(value, str):
        return _mask_text(value)
    if isinstance(value, (int, float)) and not isinstance(value, bool) and _EVIDENCE_COUNT_KEY.search(key):
        return value > 0
    return value


def _evidence(value: Any, where: str) -> str:
    data, _ = _load_json(value, where)
    if not isinstance(data, dict):
        raise FieldRefused(f"{where} is not an evidence object")
    try:
        return json.dumps(_evidence_counts(scrub_evidence_for_llm(data)), indent=2, default=str)
    except (TypeError, ValueError) as exc:
        raise FieldRefused(f"{where} is not JSON-serializable") from exc


_PROJECTIONS = {
    "identifier": _identifier,
    "catalog": _catalog,
    "enum": _enum,
    "user_text": _user_text,
    "rsync_text": _rsync_text,
    "model_output": _model_output,
    "config_keys": _config_keys,
    "connections": _connections,
    "evidence": _evidence,
}


def project(
    field_class: str,
    value: Any,
    *,
    name: str,
    dialect: Optional[str] = None,
    literals: Optional[LiteralMap] = None,
) -> Any:
    """Return ``value`` reduced to what ``field_class`` lets into a prompt.

    Raises :class:`FieldRefused` when the value does not fit, and for an
    unknown class.
    """
    if field_class == "sql":
        if value is None or value == "":
            return value
        if not isinstance(value, str):
            raise FieldRefused(f"{name} is a {type(value).__name__}, not SQL")
        try:
            return placeholder_sql(value, dialect, literals if literals is not None else LiteralMap())
        except SqlPlaceholderError:
            return WITHHELD_SQL
    fn = _PROJECTIONS.get(field_class)
    if fn is None:
        raise FieldRefused(f"{name} has an unknown field class {field_class!r}")
    return fn(value, name)
