"""
Masking Utility for Sensitive Data

This module provides functions to mask sensitive data before logging.
Used across all Python services to prevent credential leaks in logs.
"""

import re
from typing import Any, Dict, Optional, Union
import copy

# List of keys that should be masked in logs: the credential group of
# shared/sensitive_keys_golden.json, which the two Go SensitiveKeys lists also follow
# (tests/test_sensitive_keys_golden.py). No PII keys here -- see that test's docstring.
SENSITIVE_KEYS = {
    "password",
    "secret",
    "secret_key",
    "secret_access_key",
    "api_key",
    "apikey",
    "token",
    "access_key",
    "access_key_id",
    "refresh_token",
    "access_token",
    "bearer",
    "authorization",
    "credentials",
    "credentials_json",
    "private_key",
    "client_secret",
    "aws_secret_access_key",
    "aws_access_key_id",
    "connection_string",
    "conn_str",
    "db_password",
}

MASKED_VALUE = "********"


def is_sensitive_key(key: str) -> bool:
    """Check if a key should be masked."""
    if not key:
        return False
    key_lower = key.lower()
    return any(sensitive in key_lower for sensitive in SENSITIVE_KEYS)


def mask_dict(data: Optional[Dict[str, Any]]) -> Optional[Dict[str, Any]]:
    """
    Create a copy of a dictionary with sensitive values masked.
    
    Works recursively on nested dictionaries.
    
    Args:
        data: Dictionary to mask
        
    Returns:
        A new dictionary with sensitive values replaced with "********"
    """
    if data is None:
        return None
    
    if not isinstance(data, dict):
        return data
    
    masked = {}
    for key, value in data.items():
        if is_sensitive_key(key):
            masked[key] = MASKED_VALUE
        elif isinstance(value, dict):
            masked[key] = mask_dict(value)
        elif isinstance(value, list):
            masked[key] = [
                mask_dict(item) if isinstance(item, dict) else item
                for item in value
            ]
        else:
            masked[key] = value
    
    return masked


def mask_connection_string(conn_str: str) -> str:
    """
    Mask passwords and secrets in connection strings.
    
    Args:
        conn_str: Connection string (e.g., "postgresql://user:password@host/db")
        
    Returns:
        Connection string with sensitive parts masked
    """
    if not conn_str:
        return conn_str
    
    # Mask URL-style passwords: ://user:password@ -- user may be empty; runs to the last @ before the path
    result = re.sub(
        r'(://[^:/@\s]*:)([^/?#\s]+)(@)',
        rf'\1{MASKED_VALUE}\3',
        conn_str
    )
    
    # Mask key=value style passwords
    patterns = [
        r'(?i)(password|pwd|passwd)=([^;&\s]+)',
        r'(?i)(secret|api_key|token)=([^;&\s]+)',
    ]
    
    for pattern in patterns:
        result = re.sub(pattern, rf'\1={MASKED_VALUE}', result)
    
    return result


# ------------------------------------------------------------------------------
# LLM privacy scrubber — Python mirror of backend-orchestrator/pkg/llmscrub.
# Contract (CLAUDE.md "LLM data privacy"): the LLM may see schema metadata and
# user-authored text — never row values, credentials, or PII. Any free-form
# error/log text destined for a prompt must pass through scrub_error_for_llm.
# Keep the regex set in lockstep with the Go implementation.
# ------------------------------------------------------------------------------

_SCRUB_REDACTED = "[redacted]"

# The rules that match a credential and nothing else. They are also the
# first half of what scrub_secrets_for_llm applies to a whole outbound prompt
# (src/egress), which is why they are named on their own: a prompt holds
# JSON, SQL placeholders and prose, and the value rules below (quoted
# strings, JSON numbers, digit runs) would shred it. These are the SAME
# regex objects the full list uses, in the same relative order, so the
# Go/Python lockstep is unchanged.
_SECRET_PATTERNS_HEAD = [
    # Credentials embedded in URLs: scheme://user:pass@host. The user name may
    # be empty (redis://:pass@host); requiring one left that password in clear.
    # `(?<!\w)`: the scheme starts where its word does. Python's engine
    # backtracks, so without it a long word run was re-read from each of its
    # starts (quadratic; Go's RE2 is not). The leftmost match starts there
    # anyway -- \w+ can always reach back to the run's start -- so the output
    # is the same.
    (re.compile(r"(?<!\w)(\w+://)[^/\s:@]*:[^/\s@]*@"), rf"\1{_SCRUB_REDACTED}@"),
    # HTTP credential headers — must run before the KV rule, whose \S+ would
    # otherwise consume only the word "Bearer" and leave the token itself.
    (re.compile(r"\bBearer\s+[A-Za-z0-9._~+/=\-]+", re.IGNORECASE), f"Bearer {_SCRUB_REDACTED}"),
    # key=value / key: value pairs with credential-bearing key names.
    (
        re.compile(
            r"\b(password|passwd|pwd|secret|token|api[_-]?key|apikey|authorization|bearer|access[_-]?key|private[_-]?key|client[_-]?secret)\b\s*[=:]\s*\S+",
            re.IGNORECASE,
        ),
        rf"\1={_SCRUB_REDACTED}",
    ),
    # The same key=value shape when the credential word is fused into a longer
    # key name. The rule above is anchored with \b, and an underscore is a word
    # character, so KAFKA_SASL_PASSWORD, SASL_PASSWORD, sasl_plain_password,
    # KAFKA_SASL_OAUTHBEARER_CLIENT_SECRET and the OAuthClientSecret field of
    # kafkaclient.Config under %#v had no word boundary before the credential
    # word and matched nothing at all. (Config.String() redacts, so %v/%+v are
    # already safe; %#v and reflect-based dumps bypass it.)
    #
    # Kafka is where that bites hardest: every SASL credential this platform
    # reads is spelled that way, and a broker credential is shared across all
    # tenants rather than scoped to one, so a single connect error carrying an
    # env dump leaks a cluster-wide secret. AWS_SECRET_ACCESS_KEY /
    # AWS_SESSION_TOKEN ride the same shape on the MSK IAM path.
    #
    # The key name is kept: it is configuration metadata, and it names the
    # variable an operator has to rotate. Bare "key" is deliberately NOT a
    # suffix — masking it would eat the Postgres "Key (email)=" detail and
    # schema metadata like partition_key. Character classes are spelled out
    # rather than using \w because Python's \w is Unicode-aware and Go's is not.
    #
    # The key starts where its run of key characters does, `(?<![A-Za-z0-9_.\-])`
    # rather than Go's `\b`: from a `\b` inside a long run, Python's engine read
    # the run again from each start (quadratic). The key name is kept whole, so
    # the output is the same, and a key after a non-ASCII letter (éPASSWORD=)
    # is now taken as Go takes it.
    # MUST stay lockstep with Go llmscrub + the sink worker's scrubLog.
    (
        re.compile(
            r"(?<![A-Za-z0-9_.\-])([A-Za-z0-9_.\-]*(?:password|passwd|pwd|secret|token|api[_.\-]?key|access[_.\-]?key|private[_.\-]?key))\b\s*[=:]\s*\S+",
            re.IGNORECASE,
        ),
        rf"\1={_SCRUB_REDACTED}",
    ),
    # Bare JWTs (base64url evades the base64 rule's charset/length): eyJ = {"
    (re.compile(r"\beyJ[A-Za-z0-9_\-]{4,}(?:\.[A-Za-z0-9_\-]+){1,2}"), _SCRUB_REDACTED),
]
# Long base64-ish blobs (tokens, keys). 40+ chars keeps 32-hex trace ids.
_B64 = re.compile(r"\b[A-Za-z0-9+/]{40,}={0,2}\b")
# Inside a URL path: a 40+ blob with no `/` (the path's own slashes are not part
# of a token), and any 20+ run of letters AND digits (_redact_mixed_token): a
# sheet id, a webhook secret, a bot token.
_URL_PATH_BLOB = re.compile(r"[A-Za-z0-9+]{40,}={0,2}")
_URL_TOKEN = re.compile(r"[A-Za-z0-9_\-]{20,}")
# The value of a credential-named query or fragment parameter: ?key=,
# &X-Amz-Signature=, &X-Amz-Credential=, ?code=, #access_token=.
_URL_QUERY_CRED = re.compile(
    r"(?i)([?&;#][A-Za-z0-9_.\-]*(?:key|sig|signature|secret|token|password|passwd|pwd|auth|credential|code)=)[^&#\s'\"<>]*"
)
_URL_PATH_START = re.compile(r"[/?#]")
_URL_QUERY_START = re.compile(r"[?#]")


def _redact_mixed_token(m: "re.Match[str]") -> str:
    """A _URL_TOKEN run holding a letter and a digit is a token; a word run
    like HubSpot-public-api-spec-collection has no digit and stays."""
    t = m.group(0)
    if any(c.isdigit() for c in t) and any(c.isalpha() for c in t):
        return _SCRUB_REDACTED
    return t


def _scrub_url(url: str) -> str:
    """Keep a URL readable and take what in it can be a secret: a blob in
    scheme://authority, token-shaped path segments, and credential parameters
    and blobs in the query or fragment. Userinfo was already taken by the
    URL-credential rule."""
    start = url.find("://") + 3
    m = _URL_PATH_START.search(url, start)
    end = m.start() if m else len(url)
    head, path, query = url[:end], url[end:], ""
    m = _URL_QUERY_START.search(path)
    if m:
        path, query = path[: m.start()], path[m.start():]
    head = _B64.sub(_SCRUB_REDACTED, head)
    path = _URL_TOKEN.sub(_redact_mixed_token, _URL_PATH_BLOB.sub(_SCRUB_REDACTED, path))
    query = _URL_QUERY_CRED.sub(rf"\1{_SCRUB_REDACTED}", query)
    query = _URL_TOKEN.sub(_redact_mixed_token, _B64.sub(_SCRUB_REDACTED, query))
    return head + path + query


def _scrub_url_or_blob(m: "re.Match[str]") -> str:
    return _scrub_url(m.group(1)) if m.group(1) is not None else _SCRUB_REDACTED


# A URL, or else a long base64-ish blob (_B64). Read as one blob, a long URL path
# became `…HubSpot-public-api-spec-[redacted].json`, while a key in a short query
# value went through untouched. A URL goes to _scrub_url instead. The scheme is
# capped at 32 characters so a long dotted run cannot make this engine
# quadratic. MUST stay lockstep with Go llmscrub (reURLOrBase64 + scrubURL).
_SECRET_PATTERN_URL_OR_B64 = (
    re.compile(r"([A-Za-z][A-Za-z0-9+.\-]{0,31}://[^\s'\"<>]+)|\b[A-Za-z0-9+/]{40,}={0,2}\b"),
    _scrub_url_or_blob,
)
_SECRET_PATTERNS = _SECRET_PATTERNS_HEAD + [_SECRET_PATTERN_URL_OR_B64]


class _RunStartRule:
    """A rule that opens with a run of one character class, matched in linear
    time by Python's backtracking engine, with the output ``re.sub`` gives.

    From every start inside a long run, ``re`` reads the rest of the run again
    before it fails, so a 20 KB run takes seconds (Go's RE2 does not). The
    leftmost match only starts inside a run right where the previous match
    ended, so that one position is tried as it is, and every other start must
    not follow a character of the run. ``body`` must not match the empty
    string.
    """

    def __init__(self, body: str, run_class: str) -> None:
        self.pattern = body
        self._at = re.compile(body)
        self._after_run = re.compile(rf"(?<!{run_class})(?:{body})")

    def sub(self, repl: str, string: str) -> str:
        out, pos = [], 0
        while True:
            m = (self._at.match(string, pos) if pos else None) or self._after_run.search(string, pos)
            if m is None:
                break
            out.append(string[pos : m.start()])
            out.append(m.expand(repl))
            pos = m.end()
        out.append(string[pos:])
        return "".join(out)


# A host name, in the places error text puts one: `lookup X`, `dial tcp X`,
# libpq's `host name "X"`, `host "X"` and `server at "X"`. A bare dotted name has
# the same shape as schema.table, which a diagnosis needs, so the name is taken
# only after one of these words, and only when it has a dot: `lookup postgres`
# (a compose service) and the prose `lookup failed:` stay. `_` is allowed for
# SRV names (_mongodb._tcp.cluster0...). Runs after the IPv4 rule, so an address
# is already [ip-redacted]. src.egress.fields.mask_endpoints applies it too.
# MUST stay lockstep with Go llmscrub + the sink worker's scrubLog.
HOST_IN_ERROR_TEXT = re.compile(
    r'(?i)(\blookup\s+|\bdial\s+(?:tcp|udp)[46]?\s+|\bhost(?:\s+name)?\s+"|\bserver\s+at\s+")'
    r"[A-Za-z0-9_\-]+(?:\.[A-Za-z0-9_\-]+)+"
)

_SCRUB_PATTERNS = [
    # Postgres not-null/check DETAIL dumps the ENTIRE row — greedy to end of
    # string so a truncated dump can't leak a partial row.
    (re.compile(r"\bFailing row contains\b.*", re.IGNORECASE | re.DOTALL), f"Failing row contains ({_SCRUB_REDACTED})"),
    # Everything after a SQL VALUES keyword is row data (multi-tuple included).
    (re.compile(r"\bVALUES\b\s*\(.*", re.IGNORECASE | re.DOTALL), f"VALUES ({_SCRUB_REDACTED})"),
    # Postgres duplicate-key detail: Key (email)=(user@x.com) — keep column names.
    (re.compile(r"(\bKey\s*\([^)]*\)=)\((?:[^()]|\([^)]*\))*\)", re.IGNORECASE), rf"\1({_SCRUB_REDACTED})"),
    *_SECRET_PATTERNS_HEAD,
    # Double-quoted string directly after a colon — Postgres quotes offending
    # VALUES this way ('invalid input syntax for type integer: "jane"') and it
    # catches JSON string values; identifier quoting never follows a colon.
    (re.compile(r'(:\s*)"(?:[^"\\]|\\.)*"'), rf'\1"{_SCRUB_REDACTED}"'),
    # JSON numeric values ("age": 41) — quoted key + colon + number.
    (re.compile(r'("[\w\-]+"\s*:\s*)-?\d[\d.eE+\-]*'), r"\1[num-redacted]"),
    # Single-quoted literals — SQL string values (fail-closed on identifiers).
    # `(?:'|\Z)` also covers a literal left OPEN by upstream log truncation.
    #
    # The leading `(^|[^A-Za-z0-9_])` keeps prose intact: a SQL literal opens at
    # the start of the text or after a delimiter, while an apostrophe between
    # word characters is a contraction ("couldn't", "the user's"). Without it,
    # "Couldn't … pipeline 'orders-sync'" consumed the prose as the literal and
    # left the pipeline name in the clear — redacting the wrong half.
    #
    # Paired and unpaired are ONE rule: a second `'…$` pass re-read the
    # `'[redacted]'` this rule had just written and appended another
    # `'[redacted]`, corrupting already-redacted text. `\Z` (not `$`) so this
    # matches Go's `$`, which never matches before a trailing newline.
    #
    # `\\?\Z`: a cut can also land right after a backslash ('john\\), where
    # `\\.` has nothing left to pair with. Without it that literal matched
    # nowhere and went through whole, and a long run of `'\\` was read again
    # from every quote (quadratic in Python).
    # MUST stay lockstep with Go llmscrub + base_connector scrub_sensitive.
    (re.compile(r"(^|[^A-Za-z0-9_])'(?:[^'\\]|\\.)*(?:'|\\?\Z)", re.DOTALL), rf"\1'{_SCRUB_REDACTED}'"),
    # Email addresses appearing outside quotes.
    (_RunStartRule(r"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}", r"[A-Za-z0-9._%+\-]"), "[email-redacted]"),
    # IPv4 addresses — client/host IPs are personal data under GDPR and aren't
    # needed for diagnosis. Octets are validated 0-255, so 3-part version strings
    # (8.0.32) and ISO dates don't match; a 4-part dotted-quad (incl. a rare
    # 1.2.3.4 version) is an accepted loss. MUST stay lockstep with Go llmscrub.
    (
        re.compile(r"\b(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\b"),
        "[ip-redacted]",
    ),
    (HOST_IN_ERROR_TEXT, r"\1[host-redacted]"),
    # URLs (readable, secrets inside taken), else long base64-ish blobs.
    _SECRET_PATTERN_URL_OR_B64,
    # Dashed SSN / US-phone shapes (ISO dates \d{4}-\d{2}-\d{2} don't match —
    # timestamps must survive for diagnosis).
    (re.compile(r"\b\d{3}-\d{2}-\d{4}\b"), "[num-redacted]"),
    (re.compile(r"\b\d{3}[-.]\d{3}[-.]\d{4}\b"), "[num-redacted]"),
    # Digit runs of 7+ — likely record ids / SSNs / phones; ports and counts survive.
    (re.compile(r"\d{7,}"), "[num-redacted]"),
]


def scrub_error_for_llm(text: Optional[str], max_len: int = 0) -> str:
    """Mask likely customer data (row values, credentials, PII) in error text.

    Lossy by design: losing a fragment of an error message costs diagnosis
    quality; leaking a row value breaks the privacy contract. Truncation (when
    ``max_len`` > 0) happens after scrubbing so a cut can never split a masked
    token back open.
    """
    if not text:
        return ""
    result = text
    for pattern, replacement in _SCRUB_PATTERNS:
        result = pattern.sub(replacement, result)
    if max_len > 0 and len(result) > max_len:
        result = result[:max_len] + "…"
    return result


def scrub_secrets_for_llm(text: Optional[str]) -> str:
    """Mask credentials only: URL userinfo, Bearer tokens, credential key=value
    pairs, JWTs, long base64 blobs, and tokens and credential parameters in URLs.

    The egress guard (src/egress) runs this over every message of every prompt,
    before and after any added stage. It is the credential half of
    scrub_error_for_llm and deliberately not the rest: a prompt is made of
    rsync.ai's own template text, JSON and SQL with its literals already replaced,
    and the value rules would mangle all three. Row values never reach a prompt
    in the first place -- each variable is projected by its field class
    (src/egress/fields.py) -- so this is the backstop for what a user types or
    pastes.
    """
    if not text:
        return ""
    result = text
    for pattern, replacement in _SECRET_PATTERNS:
        result = pattern.sub(replacement, result)
    return result


# Fields of a connection record an LLM prompt may carry. Allowlist, not denylist:
# see connections_for_llm.
LLM_CONNECTION_FIELDS = ("id", "name", "connector_type")


def connections_for_llm(connections: Optional[list]) -> list:
    """Project connection records down to the fields a planner prompt may see.

    The planners need to name a connection ("use connection X as the source"),
    which takes an id, a display name and a connector type -- and nothing else.
    Everything else on the record is either a credential or a description of the
    customer's infrastructure: host, port, database and bucket names, and
    ``last_test_error``, which is free-form error text and therefore the same
    leak path ``scrub_error_for_llm`` exists to close.

    Allowlist by construction. The API gateway masks the config it hands out
    (``maskSensitiveFields``), but that is a *denylist* of key names, so a
    connector that stores a secret under a name nobody has added to it yet
    fails open. On the way into a prompt the failure has to go the other way:
    a field nobody has thought about is dropped, not forwarded.

    Non-dict entries are dropped rather than passed through -- the callers feed
    this straight to ``json.dumps``.
    """
    if not connections:
        return []
    out = []
    for c in connections:
        if not isinstance(c, dict):
            continue
        out.append({field: c.get(field) for field in LLM_CONNECTION_FIELDS})
    return out


# Keys in a diagnose-evidence object whose string values are free-form error or log
# text. Anything NOT named here is treated as metadata and kept, which is what keeps
# a diagnosis readable: table and column names, stage ids and statuses are what the
# model needs. Counts and sizes are kept only as yes/no -- the egress guard's
# evidence class does that after this scrub (src/egress/fields.py).
#
# The second line of this set is what makes it a backstop rather than a decoration.
# An audit found the original set naming keys no producer writes while missing the
# ones it does. Every log line the API gateway attaches arrives under "body", a blocked stage's explanation under "blocking_description", and a
# dependency probe's free-form output under "details" (diagnose.go) -- precisely the
# fields a forgotten Scrub on the Go side would leak through.
EVIDENCE_FREETEXT_KEYS = {
    "error", "last_error", "error_message", "stderr", "stdout", "logs", "log",
    "message", "detail", "exception", "traceback", "output", "reason",
    "body", "blocking_description", "details", "sink_error_detail", "stalled_stage_logs",
}


def scrub_evidence_for_llm(evidence: Any, _inherited: bool = False) -> Any:
    """Scrub the free-text strings in an LLM diagnose-evidence object.

    Defense in depth for ``POST /v1/diagnose/pipeline``. The producer in api-gateway
    scrubs each free-text field at source; this catches the field it forgets, and it
    is the ONLY check on the path when the evidence object comes from somewhere else
    -- the endpoint accepts an arbitrary object from any caller that can reach
    llm-service.

    The free-text verdict is inherited DOWNWARD. A dependency's ``details`` map is
    built per-probe out of whatever that probe returned, and a log line is a list
    element, so neither can be covered by naming leaf keys: the decision has to be
    made at the subtree. Once a key says "this is log or error text", every string
    beneath it is treated as such no matter what it is called.
    """
    if isinstance(evidence, dict):
        out = {}
        for k, v in evidence.items():
            freetext = _inherited or k in EVIDENCE_FREETEXT_KEYS
            out[k] = (
                scrub_error_for_llm(v)
                if (freetext and isinstance(v, str))
                else scrub_evidence_for_llm(v, freetext)
            )
        return out
    if isinstance(evidence, list):
        return [scrub_evidence_for_llm(v, _inherited) for v in evidence]
    return evidence
