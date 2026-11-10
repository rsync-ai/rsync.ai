"""`test_connection` must not report an unproven credential as validated.

The bug this locks down was *inverted*, which is why it survived review: the
guard that exists to refuse an unvalidated credential was keyed on
`auth_type == "none"`, and `auth_type` is PLUMBING, not semantics. Generated
REST connectors set it to "none" on purpose so `BaseMCPConnector` does not
auto-inject its own `Authorization` header. So the guard was disabled on every
connector that needed it, and armed only on the one genuinely public API --
making that connector impossible to add at all.

Why it matters beyond a cosmetic status: a connector whose `test_connection`
returns success is auto-promoted `lifecycle=draft -> preview`. An API that
happens to serve one endpoint publicly could therefore promote a connector
holding a bogus credential, and every later call to a real endpoint 401s.

The property under test is a CONTROL, not a status code: a 2xx only proves a
credential valid if the same request would have been REFUSED without it. These
tests drive each connector against synthetic APIs where the answer is known by
construction, so they fail on the behaviour rather than on the shape of the
code that implements it.
"""

import ast
import glob
import hashlib
import importlib.util
import json
import logging
import os
import re
import sys

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
PUBLIC = os.path.join(ROOT, "public")
TEMPLATE = os.path.join(
    os.path.dirname(os.path.dirname(ROOT)),
    "llm-service", "src", "agents", "tool_generator", "templates", "connector.py.j2",
)

# Endpoints a connector may probe to ask "who am I?". Kept here rather than
# imported so a connector quietly dropping one from its own list cannot also
# quietly shrink the test's idea of the world.
WHOAMI = {"/users/me", "/user/me", "/me", "/whoami",
          "/account", "/accounts/me", "/profile", "/user"}

AUTH_HEADERS = ("authorization", "x-api-key", "api-key",
                "x-auth-token", "private-token", "x-access-token")

# Floors. Every count below is asserted to be positive before any verdict is
# read from it: a discovery regression returns an empty set, and an empty set
# passes every `for` loop silently.
MIN_CORROBORATING_CONNECTORS = 3  # github-rest, notion-rest, stripe (petstore deleted)
MIN_VENDORED_BASES = 15


def _discover(predicate):
    """Connectors matching `predicate`, found by RECURSIVE glob.

    A flat `public/*/latest.json` finds ten of twenty-four -- `database/` and
    `storage/` sit one level deeper -- and reports no error while missing them.
    """
    found = []
    pattern = os.path.join(ROOT, "**", "latest.json")
    for manifest in sorted(glob.glob(pattern, recursive=True)):
        if os.sep + "versions" + os.sep in manifest:
            continue
        root = os.path.dirname(manifest)
        with open(manifest) as handle:
            current = json.load(handle)["current_version"]
        version_dir = os.path.join(root, "versions", str(current))
        source = os.path.join(version_dir, "connector.py")
        if os.path.isfile(source) and predicate(source):
            found.append((os.path.basename(root), version_dir))
    return found


def _has_corroboration(source):
    with open(source, encoding="utf-8") as handle:
        return "_probe_endpoint_is_public" in handle.read()


CORROBORATING = _discover(_has_corroboration)


def _load_connector_class(version_dir):
    """Import a connector module in isolation and return its connector class."""
    sys.path.insert(0, version_dir)
    try:
        name = "probe_" + hashlib.sha1(version_dir.encode()).hexdigest()[:10]
        spec = importlib.util.spec_from_file_location(
            name, os.path.join(version_dir, "connector.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    finally:
        sys.path.pop(0)
    for attr in dir(module):
        obj = getattr(module, attr)
        if (isinstance(obj, type) and attr.endswith("Connector")
                and not attr.startswith("Base")):
            return obj
    raise AssertionError(f"no connector class found in {version_dir}")


def _credential_variants(cls):
    """EVERY credential shape this connector declares, one dict per key.

    Testing a single key is not enough, and this is not hypothetical: checking
    only the first declared key leaves a hardcoded-list regression invisible.
    Notion calls its token `integration_token`, so a `creds_present` check that
    greps for access_token/token/api_key silently classifies a real Notion
    credential as "nothing to validate" -- and the connection is then never
    validated at all. That mutation survives a one-key test and dies here.
    """
    methods = getattr(cls, "SUPPORTED_AUTH_METHODS", None) or {}
    variants = []
    for spec in methods.values():
        for key in ((spec or {}).get("config_keys") or []):
            variants.append({key: "test-credential-value"})
        param = (spec or {}).get("query_param")
        if param:
            variants.append({param: "test-credential-value"})
    return variants or [{"access_token": "test-credential-value"}]


def _credential_for(cls):
    """One representative credential, for assertions that do not vary by key."""
    return _credential_variants(cls)[0]


def _drive(version_dir, world, config):
    """Run `test_connection` against a synthetic API whose truth we control.

    Returns (success, credentials_validated, warned).
    """
    result, _ = _drive_result(version_dir, world, config)
    return (result.get("success"),
            result.get("credentials_validated"),
            bool(result.get("warning")))


def _drive_result(version_dir, world, config):
    """`_drive`, returning the whole response and whether ANY request the
    connector sent carried auth material -- the fact the response is about."""
    carried = []
    logging.disable(logging.CRITICAL)
    try:
        cls = _load_connector_class(version_dir)
        inst = cls.__new__(cls)
        cls.__init__(inst)
        if config:
            try:
                inst.configure_from_connection(dict(config))
            except Exception:
                pass

        def fake_request(method, endpoint, cfg=None, **kwargs):
            try:
                headers = inst._get_headers(cfg or {}) or {}
            except Exception:
                headers = {}
            # A bare scheme name ("Bearer" with no token) is NOT auth material:
            # a server treats it exactly like sending nothing.
            authed = any(
                k.lower() in AUTH_HEADERS and len(str(v or "").split()) >= 2
                for k, v in headers.items())
            carried.append(authed)
            ok = {"success": True, "status_code": 200, "data": {}}
            deny = lambda code: {"success": False, "status_code": code,
                                 "error": f"HTTP {code}"}
            if world == "everything_public":
                return ok
            if world == "resource_auth_gated":
                return deny(404) if endpoint in WHOAMI else (
                    ok if authed else deny(401))
            if world == "whoami_auth_gated":
                return ok if authed else deny(401)
            if world == "whoami_public":
                return ok if endpoint in WHOAMI else deny(404)
            if world == "whoami_errors_anonymously":
                # Auth-gated, but it answers an anonymous caller 500 rather
                # than 401/403, so the control cannot read a verdict from it.
                if endpoint not in WHOAMI:
                    return deny(404)
                return ok if authed else deny(500)
            if world == "resource_refuses_anonymous_400":
                # ClickUp: a request with no token is answered 400
                # "Authorization header required" (OAUTH_017), not 401.
                return deny(404) if endpoint in WHOAMI else (
                    ok if authed else deny(400))
            if world == "resource_hides_from_anonymous_404":
                return deny(404) if endpoint in WHOAMI else (
                    ok if authed else deny(404))
            if world == "resource_rate_limits_anonymously":
                # 429 is about timing, not the credential: no verdict.
                return deny(404) if endpoint in WHOAMI else (
                    ok if authed else deny(429))
            if world == "rejects_everything":
                return deny(401)
            raise AssertionError(f"unknown world {world}")

        inst._make_request = fake_request
        result = inst.test_connection(dict(config))
    finally:
        logging.disable(logging.NOTSET)
    return result, any(carried)


IDS = [name for name, _ in CORROBORATING]


def test_corroboration_coverage_is_not_vacuous():
    """Nothing below means anything until we know we found the connectors."""
    assert len(CORROBORATING) >= MIN_CORROBORATING_CONNECTORS, (
        f"found only {len(CORROBORATING)} connectors carrying the corroboration "
        f"probe ({IDS}); expected at least {MIN_CORROBORATING_CONNECTORS}. A "
        "discovery regression looks exactly like this."
    )


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_public_endpoint_never_validates_a_credential(name, version_dir):
    """THE security property. Every endpoint answers anonymously, so nothing
    here can prove a credential -- reporting one as validated is the bug."""
    cls = _load_connector_class(version_dir)
    variants = _credential_variants(cls)
    assert variants, f"{name}: no declared credential keys to test with"
    for creds in variants:
        key = next(iter(creds))
        success, validated, warned = _drive(version_dir, "everything_public", creds)
        assert validated is not True, (
            f"{name} (credential key {key!r}): reported credentials_validated=True "
            "against an API that answers every probe anonymously. A 2xx from a "
            "public endpoint proves reachability, never credential validity -- and "
            "this verdict auto-promotes the connector draft -> preview."
        )
        assert success is True and warned, (
            f"{name} (credential key {key!r}): expected (success=True, warning set), "
            f"got (success={success}, warning={warned}). Refusing outright would make "
            "a genuinely public API impossible to connect at all."
        )


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_public_whoami_does_not_validate_a_credential(name, version_dir):
    """A whoami path is not automatically auth-protected. GitHub answers
    /users/me to anybody; trusting a whoami 2xx is the same bug one step in."""
    cls = _load_connector_class(version_dir)
    for creds in _credential_variants(cls):
        _, validated, _ = _drive(version_dir, "whoami_public", creds)
        assert validated is not True, (
            f"{name} (credential key {next(iter(creds))!r}): trusted a 2xx from a "
            "PUBLIC whoami endpoint as proof of credential validity."
        )


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
@pytest.mark.parametrize("world", ["resource_auth_gated", "whoami_auth_gated",
                                   "resource_refuses_anonymous_400",
                                   "resource_hides_from_anonymous_404"])
def test_auth_gated_endpoint_does_validate(name, version_dir, world):
    """The control must not be so strict it rejects real credentials: when the
    endpoint refuses anonymous callers, a 2xx DOES prove the credential.

    "Refuses" is any 4xx to the identical request minus the credential, not
    only 401/403. Bug class: a refusal read from one status code instead of
    from the two answers differing. ClickUp answers a missing token 400, so a
    valid token failed its connection test (2026-10-04).
    """
    cls = _load_connector_class(version_dir)
    success, validated, _ = _drive(version_dir, world, _credential_for(cls))
    assert success is True and validated is True, (
        f"{name}/{world}: an endpoint that refuses anonymous callers answered "
        f"2xx with credentials, which proves them valid; got "
        f"(success={success}, credentials_validated={validated})."
    )


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_no_credentials_against_public_api_still_connects(name, version_dir):
    """Nothing was supplied, so there is nothing to validate. This is the
    regression that made the public-API connector unaddable."""
    success, validated, _ = _drive(version_dir, "everything_public", {})
    assert success is True, f"{name}: refused a public API when no credential was supplied"
    assert validated is not True, f"{name}: claimed to validate a credential that was never supplied"


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_a_rate_limited_control_proves_nothing_and_says_why(name, version_dir):
    """Two-sided with the 4xx rule above: a 429 to the anonymous re-ask is
    about timing, so the credential stays unproven. And the error must name
    that, not the 404 of a whoami path tried after it -- "Last error: HTTP 404"
    sent a user with a working token looking for a wrong URL."""
    cls = _load_connector_class(version_dir)
    result, carried = _drive_result(version_dir, "resource_rate_limits_anonymously",
                                    _credential_for(cls))
    assert carried, f"{name}: the declared credential never reached a request"
    assert result.get("success") is False, f"{name}: {result}"
    error = str(result.get("error") or "")
    assert "not validated" in error and "404" not in error, f"{name}: {error!r}"


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_rejected_credentials_fail(name, version_dir):
    """A 401 everywhere is a failure, with or without a corroboration step."""
    cls = _load_connector_class(version_dir)
    success, _, _ = _drive(version_dir, "rejects_everything", _credential_for(cls))
    assert success is False, f"{name}: reported success when the API refused every probe"


# Every key `_credentials_present()` counts as "a credential was supplied", one
# config per key. Its generic tail is the part no connector declares, which is
# why driving only DECLARED keys (`_credential_variants`) never found
# KI-CREDENTIAL-CONTROL-DISARMED-BY-UNDECLARED-KEY: a key the connector does not
# turn into a header sends every probe out anonymously, the anonymous control
# has nothing to strip and answers None, and a public whoami 2xx was then
# reported `credentials_validated: True`.
GENERIC_CREDENTIALS = [
    {key: "test-credential-value"} for key in (
        "access_token", "token", "api_key", "apikey", "bearer_token",
        "personal_access_token", "private_token", "auth_token")
] + [{"username": "test-user", "password": "test-credential-value"}]


def _names(warning, key):
    return re.search(rf"(?<![A-Za-z0-9_]){re.escape(key)}(?![A-Za-z0-9_])", warning)


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_a_credential_no_request_carries_is_never_validated(name, version_dir):
    """Every credential key, declared or not, against a public whoami.

    Two-sided, per connector: a key the connector sends must go through the
    anonymous control (and the warning must not blame it); a key it does not
    send must come back unvalidated WITH a warning that names it, so the user
    can see which key to change.
    """
    carried_keys, dropped_keys = [], []
    for creds in GENERIC_CREDENTIALS:
        result, carried = _drive_result(version_dir, "whoami_public", creds)
        keys = sorted(creds)
        warning = str(result.get("warning") or "")
        assert result.get("credentials_validated") is not True, (
            f"{name} {keys}: reported credentials_validated=True on a whoami that "
            f"answers everybody (any request carried auth: {carried})."
        )
        assert result.get("success") is True and warning, (
            f"{name} {keys}: expected (success=True, warning set), got {result}"
        )
        if carried:
            carried_keys += keys
            assert not any(_names(warning, k) for k in keys), (
                f"{name} {keys}: the request DID carry this credential, but the "
                f"warning names it as unused: {warning!r}"
            )
        else:
            dropped_keys += keys
            missing = [k for k in keys if not _names(warning, k)]
            assert not missing, (
                f"{name} {keys}: no request carried {missing}, and the warning does "
                f"not say so, so the user cannot tell which key to change: {warning!r}"
            )
    # Anti-vacuity: both sides must actually have been exercised.
    assert carried_keys, f"{name}: no credential key reached a request at all"
    assert dropped_keys, f"{name}: every key reached a request; the dropped side is untested"


@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_an_unreadable_control_on_a_carried_credential_keeps_the_old_verdict(name, version_dir):
    """The other None path, kept on purpose: the credential DID reach the
    request, the whoami refused the anonymous control with a 500 rather than a
    401/403, so the control has no verdict and the historical rule (trust the
    whoami 2xx) decides. This pins that the fix above narrows that rule to
    credentials no request carried, rather than dropping it for everyone -- an
    inconclusive control must not fail or downgrade a working connection. If
    this is deliberately tightened later, update the caveat recorded in
    CAPABILITIES-ARCHIVE.md under KI-CREDENTIAL-CONTROL-DISARMED-BY-UNDECLARED-KEY.
    """
    cls = _load_connector_class(version_dir)
    result, carried = _drive_result(version_dir, "whoami_errors_anonymously", _credential_for(cls))
    assert carried, f"{name}: the declared credential never reached a request"
    assert result.get("success") is True and result.get("credentials_validated") is True, (
        f"{name}: {result}"
    )


# ---------------------------------------------------------------------------
# Drift guards: the generator and its output must not diverge again.
# ---------------------------------------------------------------------------

HELPER_NAMES = ("_credentials_present", "_anonymous_config", "_auth_material",
                "_unused_credential_keys", "_probe_endpoint_is_public")


def _function_source(path, name):
    """Slice one method out by TEXT, not AST.

    The generator template is a Jinja file and is not parseable Python, so an
    AST extractor raises on it. Both sides go through this same slicer so the
    comparison stays apples-to-apples.
    """
    with open(path, encoding="utf-8") as handle:
        lines = handle.read().splitlines()
    start = None
    for i, line in enumerate(lines):
        stripped = line.lstrip()
        if stripped.startswith(f"def {name}(") and line != stripped:
            start, indent = i, len(line) - len(stripped)
            break
    if start is None:
        return None
    end = len(lines)
    for j in range(start + 1, len(lines)):
        line = lines[j]
        if not line.strip():
            continue
        if len(line) - len(line.lstrip()) <= indent:
            end = j
            break
    return "\n".join(lines[start:end]).rstrip()


@pytest.mark.skipif(not os.path.isfile(TEMPLATE), reason="generator template not in this tree")
@pytest.mark.parametrize("helper", HELPER_NAMES)
@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_helpers_match_the_generator_template(name, version_dir, helper):
    """A fix applied to a connector but not its template is undone by the next
    regeneration -- silently, because the regenerated file still looks correct.

    The helper block carries no Jinja tokens, so byte-identity is exact here.
    """
    generated = _function_source(os.path.join(version_dir, "connector.py"), helper)
    if generated is None:
        pytest.fail(f"{name} advertises corroboration but has no {helper}")
    expected = _function_source(TEMPLATE, helper)
    assert expected is not None, f"{helper} is missing from {os.path.basename(TEMPLATE)}"
    assert generated == expected, (
        f"{name}: {helper} has drifted from connector.py.j2. Patch the template "
        "and the connector together, or the next regeneration reverts the fix."
    )


# The helpers read two class-level tuples. Those are not functions, so the
# byte-identity check above steps straight over them -- which is exactly how a
# connector can carry all three helpers, match the template character for
# character, and still raise AttributeError the first time test_connection runs.
# That is not hypothetical: stripe shipped in precisely that state.
CLASS_ATTRS = ("_CREDENTIAL_CONFIG_KEYS", "_AUTH_HEADER_NAMES")


def _attr_block(path, attr):
    """The tuple literal assigned to `attr`, sliced by TEXT so the Jinja
    template and the generated file go through the same reader."""
    with open(path, encoding="utf-8") as handle:
        lines = handle.read().splitlines()
    for i, line in enumerate(lines):
        if line.strip().startswith(f"{attr} = ("):
            out = [line]
            for j in range(i + 1, len(lines)):
                out.append(lines[j])
                if lines[j].strip() == ")":
                    return "\n".join(out)
            break
    return None


@pytest.mark.skipif(not os.path.isfile(TEMPLATE), reason="generator template not in this tree")
@pytest.mark.parametrize("attr", CLASS_ATTRS)
@pytest.mark.parametrize("name,version_dir", CORROBORATING, ids=IDS)
def test_helper_class_attrs_match_the_generator_template(name, version_dir, attr):
    """The tuples the helpers depend on must ship with them, and must match."""
    generated = _attr_block(os.path.join(version_dir, "connector.py"), attr)
    assert generated is not None, (
        f"{name} carries the corroboration helpers but not {attr}, which they "
        "read. test_connection raises AttributeError on the first probe."
    )
    expected = _attr_block(TEMPLATE, attr)
    assert expected is not None, f"{attr} is missing from {os.path.basename(TEMPLATE)}"
    assert generated == expected, (
        f"{name}: {attr} has drifted from connector.py.j2."
    )


# `_handle_tool_call` carries the guard that stops a crafted tool name
# ("<type>__probe_endpoint_is_public" strips to a private method) from reaching
# getattr. Every vendored copy is byte-identical to the canonical one except
# kafka-mcp-sink, which predates the configure_from_connection step but does
# carry the guard -- so it is checked behaviourally below rather than by text.
# That missing step-0 hydration is deliberate and reasoned at the call site in
# that connector's `_handle_tool_call` (grep KI-KAFKA-SINK-NO-CONFIG-HYDRATION),
# and is separately gated by test_dispatch_hydration_census.py -- so
# `test_vendored_dispatch_matches_canonical` skipping this path is a documented
# exemption, not an unguarded hole.
KNOWN_OLDER_BASE = {"internal/kafka-mcp-sink/versions/v1.0.0/base_connector.py"}

VENDORED_BASES = sorted(
    os.path.relpath(p, ROOT)
    for p in glob.glob(os.path.join(ROOT, "*", "**", "base_connector.py"), recursive=True)
)


def test_vendored_base_discovery_is_not_vacuous():
    assert len(VENDORED_BASES) >= MIN_VENDORED_BASES, (
        f"found only {len(VENDORED_BASES)} vendored base_connector.py copies; "
        f"expected at least {MIN_VENDORED_BASES}."
    )


@pytest.mark.parametrize("relpath", VENDORED_BASES)
def test_vendored_dispatch_refuses_private_methods(relpath):
    """The security property, asserted on the text of every copy.

    Checked per-copy because the Docker build context is the versioned dir:
    these copies are the code that actually runs, and a fix landing only on the
    canonical file would not reach any of them.
    """
    path = os.path.join(ROOT, relpath)
    body = _function_source(path, "_handle_tool_call")
    if body is None:
        pytest.skip(f"{relpath} does not define _handle_tool_call")
    assert "startswith('_')" in body or 'startswith("_")' in body, (
        f"{relpath}: _handle_tool_call no longer rejects underscore-prefixed "
        "method names. A tool named '<connector_type>__probe_endpoint_is_public' "
        "strips to a private method that getattr will happily resolve."
    )


@pytest.mark.parametrize("relpath", VENDORED_BASES)
def test_vendored_dispatch_matches_canonical(relpath):
    """Byte-identity, so ANY drift surfaces -- not just this one guard."""
    if relpath in KNOWN_OLDER_BASE:
        pytest.skip(f"{relpath} is a declared older variant; guarded test above still applies")
    canonical = _function_source(os.path.join(ROOT, "base_connector.py"), "_handle_tool_call")
    assert canonical is not None
    body = _function_source(os.path.join(ROOT, relpath), "_handle_tool_call")
    if body is None:
        pytest.skip(f"{relpath} does not define _handle_tool_call")
    assert body == canonical, (
        f"{relpath}: _handle_tool_call has drifted from "
        "shared/mcp-connectors/base_connector.py. If the divergence is "
        "deliberate, add it to KNOWN_OLDER_BASE with a reason."
    )
