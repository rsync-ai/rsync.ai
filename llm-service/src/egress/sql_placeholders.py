"""SQL literal placeholders: the values in a query never reach an LLM.

Before SQL goes into a prompt, every literal in it is replaced by
``{{L<n>:str|num|bytes}}`` and its comments are dropped. That covers strings,
numbers, national, unicode, raw and dollar-quoted strings, and hex, bit and
byte strings. The model still sees the query's shape: tables, columns,
functions, joins and filters. The values it filters on stay here. When the
model hands SQL back, :func:`restore_sql_literals` puts the originals back,
and refuses anything it cannot place exactly.

Literals are found with sqlglot's tokenizer for the query's own dialect,
never with a regex. Quoting rules differ by dialect: ``"x"`` is a string in
MySQL and BigQuery and an identifier in Postgres. Only a tokenizer that knows
the dialect gets that right. With no dialect, or an unknown one, the SQL is
refused (:class:`SqlPlaceholderError`). The caller then withholds the field.

Two kinds of string stay readable, because the time-grain repair needs them
and they carry no data: date-part names (``'month'`` in
``DATE_TRUNC('month', ts)``) and pure date-format patterns (``'YYYY-MM'``,
``'%Y-%m'``).
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Optional

from sqlglot.dialects.dialect import Dialect
from sqlglot.tokens import Token, TokenType

from src.text2sql.compiler import resolve_dialect

__all__ = [
    "LiteralMap",
    "SqlPlaceholderError",
    "placeholder_sql",
    "restore_sql_literals",
]


class SqlPlaceholderError(ValueError):
    """SQL could not be placeholdered or restored safely. Nothing was sent or restored."""


_KIND_BY_TOKEN = {
    TokenType.STRING: "str",
    TokenType.NATIONAL_STRING: "str",
    TokenType.UNICODE_STRING: "str",
    TokenType.RAW_STRING: "str",
    TokenType.HEREDOC_STRING: "str",
    TokenType.NUMBER: "num",
    TokenType.HEX_STRING: "bytes",
    TokenType.BIT_STRING: "bytes",
    TokenType.BYTE_STRING: "bytes",
}

# Tokens glued onto a literal with no gap belong to it: BigQuery reads 1_000
# as NUMBER "1" + VAR "_000", and MySQL reads it as one VAR "1_000".
_GLUE_RE = re.compile(r"[A-Za-z0-9_]+\Z")

_DATE_PARTS = frozenset(
    {
        "year", "years", "yy", "yyyy", "isoyear",
        "quarter", "quarters", "qtr", "q",
        "month", "months", "mon", "mm",
        "week", "weeks", "wk", "isoweek", "weekday", "weekofyear",
        "day", "days", "dd", "dow", "doy", "isodow", "dayofweek", "dayofyear", "dayofmonth",
        "hour", "hours", "hh",
        "minute", "minutes", "mi",
        "second", "seconds", "ss",
        "millisecond", "milliseconds", "ms", "microsecond", "microseconds", "us",
        "epoch", "decade", "century", "millennium",
    }
)
# A string made only of date-format tokens and separators. Letters on their
# own (no format token) cannot pass, so a name or a code cannot either.
_FORMAT_RE = re.compile(
    r"(?:YYYY|IYYY|YYY|YY|MONTH|MON|MM|DDD|DD|DAY|DY|HH24|HH12|HH|MI|SS|MS|US|IW|WW|Q|"
    r"%[A-Za-z]|[-/: .T,_])+\Z",
    re.IGNORECASE,
)
_FORMAT_TOKEN_RE = re.compile(r"YYYY|YY|MM|DD|HH|MI|SS|%[A-Za-z]", re.IGNORECASE)


def _is_readable_string(tok: Token) -> bool:
    if tok.token_type != TokenType.STRING:
        return False
    text = tok.text or ""
    if len(text) > 32:
        return False
    if text.lower() in _DATE_PARTS:
        return True
    return bool(_FORMAT_RE.match(text)) and bool(_FORMAT_TOKEN_RE.search(text))


@dataclass
class LiteralMap:
    """The literals taken out of every SQL field of one prompt.

    Numbering is shared across fields, so ``{{L3:str}}`` means one value
    wherever it appears in the prompt.
    """

    entries: dict[int, tuple[str, str]] = field(default_factory=dict)

    def add(self, kind: str, original: str) -> int:
        n = len(self.entries) + 1
        self.entries[n] = (kind, original)
        return n

    def __len__(self) -> int:
        return len(self.entries)


def _tokenize(sql: str, dialect: Optional[str]) -> list[Token]:
    name = resolve_dialect(dialect or "")
    if not name:
        raise SqlPlaceholderError("no SQL dialect given")
    try:
        dia = Dialect.get_or_raise(name)
    except Exception as exc:  # unknown dialect
        raise SqlPlaceholderError(f"unknown SQL dialect {name!r}") from exc
    try:
        return list(dia.tokenize(sql))
    except Exception as exc:
        raise SqlPlaceholderError("SQL could not be tokenized") from exc


def placeholder_sql(sql: str, dialect: Optional[str], literals: LiteralMap) -> str:
    """Return ``sql`` with every literal replaced and comments dropped.

    Text between tokens is kept only when it is whitespace: a comment, or
    anything else the tokenizer skipped, becomes one space. Raises
    :class:`SqlPlaceholderError` when the SQL cannot be tokenized for
    ``dialect``.
    """
    if not sql or not sql.strip():
        return ""
    tokens = _tokenize(sql, dialect)
    out: list[str] = []
    pos = 0
    i = 0
    while i < len(tokens):
        tok = tokens[i]
        start, end = tok.start, tok.end + 1
        if start < pos:
            # Synthetic tokens share the span of the one before them (Spark
            # reads 1L as NUMBER, DCOLON, BIGINT, all over "1L").
            if end > pos:
                raise SqlPlaceholderError("overlapping SQL tokens")
            i += 1
            continue
        if out:
            gap = sql[pos:start]
            out.append(gap if not gap or gap.isspace() else " ")
        text = sql[start:end]
        kind = _KIND_BY_TOKEN.get(tok.token_type)
        if kind is None and text[:1].isdigit():
            kind = "num"
        if kind is not None and not _is_readable_string(tok):
            # Swallow tokens glued on with no gap (1_000, 1L).
            j = i + 1
            while j < len(tokens):
                nxt = tokens[j]
                if nxt.start < end:
                    if nxt.end + 1 > end:
                        end = nxt.end + 1
                    j += 1
                    continue
                if nxt.start == end and _GLUE_RE.match(sql[nxt.start : nxt.end + 1]):
                    end = nxt.end + 1
                    j += 1
                    continue
                break
            n = literals.add(kind, sql[start:end])
            out.append(f"{{{{L{n}:{kind}}}}}")
            pos = end
            i = j
            continue
        out.append(text)
        pos = end
        i += 1
    return "".join(out)


# A placeholder as the model may hand it back: optionally wrapped in single
# quotes, optionally with spaces inside the braces, optionally without its type.
_PLACEHOLDER_RE = re.compile(r"(')?\{\{\s*L(\d+)\s*(?::\s*(\w+)\s*)?\}\}(')?")
_SENTINEL = "__RSYNC_L{}__"
_SENTINEL_RE = re.compile(r"__RSYNC_L(\d+)__")


def restore_sql_literals(sql: str, literals: LiteralMap, dialect: Optional[str]) -> str:
    """Put the original literals back into SQL the model returned.

    Refuses (:class:`SqlPlaceholderError`) when a placeholder names a literal
    this prompt never had, names the wrong type, sits inside a string,
    identifier or comment, is glued to another token, or is left malformed.
    A placeholder the model dropped is fine: its value simply is not used.
    """
    if not sql:
        return sql
    if "__RSYNC_L" in sql:
        raise SqlPlaceholderError("reserved placeholder marker in model SQL")
    if not literals.entries and "{{" not in sql:
        return sql

    def _sub(m: re.Match) -> str:
        open_q, num, kind, close_q = m.group(1), int(m.group(2)), m.group(3), m.group(4)
        entry = literals.entries.get(num)
        if entry is None:
            raise SqlPlaceholderError(f"unknown placeholder L{num}")
        want_kind, _ = entry
        if kind is not None and kind != want_kind:
            raise SqlPlaceholderError(f"placeholder L{num} changed type")
        if open_q and close_q:
            # '{{L1:str}}' -- the original already carries its quotes.
            if want_kind != "str":
                raise SqlPlaceholderError(f"placeholder L{num} quoted as a string")
            return _SENTINEL.format(num)
        return (open_q or "") + _SENTINEL.format(num) + (close_q or "")

    text = _PLACEHOLDER_RE.sub(_sub, sql)
    if re.search(r"\{\{\s*L\d", text):
        raise SqlPlaceholderError("malformed placeholder in model SQL")
    if "__RSYNC_L" not in text:
        return text

    tokens = _tokenize(text, dialect)
    placed = {
        (t.start, t.end + 1)
        for t in tokens
        if t.token_type == TokenType.VAR and _SENTINEL_RE.fullmatch(text[t.start : t.end + 1])
    }
    found = [(m.start(), m.end(), int(m.group(1))) for m in _SENTINEL_RE.finditer(text)]
    if {(s, e) for s, e, _ in found} != placed:
        raise SqlPlaceholderError("placeholder is not a whole value in model SQL")

    out: list[str] = []
    last = 0
    for s, e, num in found:
        out.append(text[last:s])
        out.append(literals.entries[num][1])
        last = e
    out.append(text[last:])
    return "".join(out)
