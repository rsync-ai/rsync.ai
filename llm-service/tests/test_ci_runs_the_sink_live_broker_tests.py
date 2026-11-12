"""CI gives the sink worker's live-broker tests the broker they skip without.

WHY THIS EXISTS. The kafka-mcp-sink worker's live tests reach a broker through
`liveKafkaBroker` (kafka_security_test.go), which skips unless
SINK_LIVE_KAFKA_BROKER is set. The `go` job never set it, so all five skipped on
every run, and four of them failed from #797 on with nobody seeing it -- a check
that cannot fail. The job now starts a broker and exports the variable. This file
holds the three things that make that true:

  * the variable the tests read is the one a step writes to $GITHUB_ENV (read
    from the Go source, so renaming it in one place goes red here);
  * that step runs whenever the sink module is tested -- its `if:` carries every
    disjunct of the `SINK` switch on the `go test` step -- and comes before it;
  * the broker is removed under `always()`, so a failed run does not leave a
    container on a self-hosted runner.

THE CLASS. A Go test that skips on an unset env var is dormant until CI sets it.
`test_every_skip_gate_in_the_sink_module_is_wired_or_named` censuses the sink
module's gates: each one is either set by this job or named in NOT_WIRED with
its reason, so a new dormant test is a red line, not a silent skip.

REACHABILITY. This runs in llm-service-unit, gated on the `llm` filter;
`test_the_ci_filter_covers_every_file_this_guard_reads` keeps every file read
here inside that filter.
"""

import fnmatch
import re
from pathlib import Path

import pytest

yaml = pytest.importorskip("yaml")

REPO = Path(__file__).resolve().parents[2]
CI_YML = REPO / ".github" / "workflows" / "ci.yml"
FILTERS = REPO / ".github" / "paths-filters.yml"
SINK_MODULE = REPO / "shared" / "mcp-connectors" / "internal" / "kafka-mcp-sink" / "worker-src"
LIVE_HELPER = SINK_MODULE / "cmd" / "kafka-sink-worker" / "kafka_security_test.go"

# Skip gates in the sink module that the `go` job does not set yet. Each entry is
# a test that never runs in CI; the census below fails if one is wired (drop it
# from here) or no longer exists.
NOT_WIRED = {
    "AUD4_LIVE_KAFKA": "aud4_dlq_test.go: needs a live broker; not wired with SINK_LIVE_KAFKA_BROKER",
    "API_RECORDS_PG_DSN": "api_dest_rows_pg_test.go: needs a migrated PostgreSQL; no service in this job",
}

GETENV = re.compile(r'os\.Getenv\("([A-Z0-9_]+)"\)')


def _go_job_steps():
    ci = yaml.safe_load(CI_YML.read_text(encoding="utf-8"))
    return ci["jobs"]["go"]["steps"]


def _live_broker_var():
    """The env var `liveKafkaBroker` reads -- from the Go source, not a copy here."""
    src = LIVE_HELPER.read_text(encoding="utf-8")
    body = re.search(r"func liveKafkaBroker\(t \*testing\.T\) string \{\n(.*?)\n\}", src, re.S)
    assert body, f"liveKafkaBroker not found in {LIVE_HELPER.relative_to(REPO)}; the guard went blind"
    names = GETENV.findall(body.group(1))
    assert len(names) == 1, f"liveKafkaBroker should read one env var, reads {names}"
    return names[0]


def _go_test_step(steps):
    idx = [i for i, s in enumerate(steps) if "go test" in str(s.get("run", ""))]
    assert len(idx) == 1, f"expected one `go test` step in the go job, found {len(idx)}"
    return idx[0]


def _sink_switch(step):
    """The disjuncts of the go test step's SINK switch: when the sink module is tested."""
    expr = str((step.get("env") or {}).get("SINK", ""))
    inner = re.fullmatch(r"\$\{\{\s*(.*?)\s*\}\}", expr.strip())
    assert inner, f"the go test step's SINK switch changed shape: {expr!r}"
    parts = [p.strip() for p in inner.group(1).split("||")]
    assert "steps.filter.outputs.sink == 'true'" in parts, parts
    return parts


def _exporters(steps, var):
    return [i for i, s in enumerate(steps) if re.search(rf"\b{var}=\S", str(s.get("run", ""))) and "GITHUB_ENV" in str(s.get("run", ""))]


def test_the_go_job_exports_the_broker_the_live_tests_read():
    var = _live_broker_var()
    steps = _go_job_steps()
    found = _exporters(steps, var)
    assert len(found) == 1, (
        f"no step in the go job writes {var}=... to $GITHUB_ENV, so every test behind "
        f"liveKafkaBroker skips in CI" if not found else f"{len(found)} steps export {var}"
    )


def test_the_broker_starts_before_and_whenever_the_sink_module_is_tested():
    var = _live_broker_var()
    steps = _go_job_steps()
    test_at = _go_test_step(steps)
    (broker_at,) = _exporters(steps, var)
    assert broker_at < test_at, f"the {var} step must come before `go test`"
    cond = str(steps[broker_at].get("if", ""))
    missing = [d for d in _sink_switch(steps[test_at]) if d not in cond]
    assert not missing, (
        f"the sink module is tested when any of these holds, but the broker step's `if:` "
        f"lacks {missing}; on that run the live tests skip again: {cond!r}"
    )


def test_the_broker_is_removed_even_when_the_job_fails():
    steps = _go_job_steps()
    test_at = _go_test_step(steps)
    names = set(re.findall(r'--name\s+"?(\$\w+)"?', str(steps[_exporters(steps, _live_broker_var())[0]].get("run", ""))))
    assert len(names) == 1, f"the broker step should name its container through one variable, found {names}"
    (name,) = names
    removers = [s for s in steps[test_at + 1:] if re.search(rf'docker\s+rm\b.*{re.escape(name)}\b', str(s.get("run", "")))]
    assert removers, f"no step after `go test` removes {name}"
    assert any("always()" in str(s.get("if", "")) for s in removers), (
        "the removal must run under always(): a failed `go test` would otherwise leave the "
        "broker running on a persistent self-hosted runner"
    )


def _skip_gates():
    """-> {env var: file} for every os.Getenv in a sink _test.go whose next three lines skip."""
    gates = {}
    for f in sorted(SINK_MODULE.rglob("*_test.go")):
        lines = f.read_text(encoding="utf-8").splitlines()
        for n, line in enumerate(lines):
            for var in GETENV.findall(line):
                if any("t.Skip" in ln for ln in lines[n:n + 4]):
                    gates[var] = f.name
    return gates


def test_every_skip_gate_in_the_sink_module_is_wired_or_named():
    gates = _skip_gates()
    assert _live_broker_var() in gates, f"the census missed liveKafkaBroker's gate; it went blind: {gates}"
    steps = _go_job_steps()
    wired = {v for v in gates if _exporters(steps, v)}
    unaccounted = sorted(set(gates) - wired - set(NOT_WIRED))
    assert not unaccounted, (
        "these sink tests skip on an env var the go job never sets, so they never run in CI; "
        f"wire them or name them in NOT_WIRED with the reason: {[(v, gates[v]) for v in unaccounted]}"
    )
    stale = sorted(set(NOT_WIRED) - (set(gates) - wired))
    assert not stale, f"NOT_WIRED names gates that are wired or gone; drop them: {stale}"


@pytest.mark.parametrize("subject", [CI_YML, LIVE_HELPER], ids=lambda p: p.name)
def test_the_ci_filter_covers_every_file_this_guard_reads(subject):
    """Same rule as the tests of this name elsewhere: this file runs only when the
    `llm` filter fires, so a PR touching only a file read here must match it."""
    rel = str(subject.relative_to(REPO))
    pats = yaml.safe_load(FILTERS.read_text(encoding="utf-8"))["llm"]
    assert any(fnmatch.fnmatch(rel, p) for p in pats), f"`{rel}` is read here, but no `llm` pattern matches it"
