"""The self-host install artifacts keep row values away from a model.

RSYNC_LLM_SAMPLE_ROWS_ALLOWED decides whether Ask rsync.ai may ever send sample
rows. Unset means allowed, because that is cloud, so the code default cannot
protect a self-host install: only the artifact it installs from can. Two
containers check it -- api-gateway (config.LLMSampleRowsAllowed) and
llm-service's egress guard (src/egress/sample_rows.py) -- and the promise that
no row value reaches a model holds only while both read "false". A container
left out reads unset, which is allowed, and nothing would fail.

So the self-host compose (the file install.sh downloads) and the Helm chart
must render "false" on both containers, and the dev and prod compose -- cloud --
must never set it. The chart's case also proves one value drives both
containers and that removing it keeps them off.

Ask rsync.ai and the agent gateway are the other way round: on unless an install
turns them off. A compose default of false would quietly undo the code default,
so every compose file's fallback is checked as well.

RSYNC_SINGLE_WORKSPACE keeps a self-host install to one workspace. Unset means
multi-workspace, because that is cloud, so again only the install artifact can
turn it on: the self-host compose and the chart must render "true" on
api-gateway (removing the chart value keeps it on), and cloud must never set it.

This proves the rendered files. It does not prove a running install.
"""

import pathlib
import re
import shutil
import subprocess

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
CHART = REPO / "deploy" / "helm" / "rsync-ai"

SAMPLE_ROWS = "RSYNC_LLM_SAMPLE_ROWS_ALLOWED"
CHECKERS = ("api-gateway", "llm-service")
SELF_HOST = "docker-compose.quickstart.yml"
CLOUD = ("docker-compose.yml", "docker-compose.prod.yml")
ON_BY_DEFAULT = ("RSYNC_ASSISTANT_ENABLED", "RSYNC_AGENT_GATEWAY_ENABLED")
SINGLE_WORKSPACE = "RSYNC_SINGLE_WORKSPACE"

# The documented install command's flags, so a render reaches these containers
# instead of stopping at an unrelated required-value check. Fakes throughout.
RENDER_FLAGS = [
    "--set", "secrets.jwtSecret=FAKEPLACEHOLDER",
    "--set", "secrets.encryptionKey=FAKEPLACEHOLDER",
    "--set", "secrets.postgresPassword=FAKEPLACEHOLDER",
    "--set", "secrets.minioAccessKey=FAKEPLACEHOLDER",
    "--set", "secrets.minioSecretKey=FAKEPLACEHOLDER",
    "--set", "frontend.publicUrl=https://app.example.com",
    "--set", "frontend.apiUrl=https://api.example.com",
]


class _ComposeLoader(yaml.SafeLoader):
    """SafeLoader that reads through Compose's `!override` / `!reset` tags.

    prod.yml uses them, and safe_load raises on an unknown tag.
    """


def _resolve_tag(loader, node):
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node, deep=True)
    if isinstance(node, yaml.MappingNode):
        return loader.construct_mapping(node, deep=True)
    return loader.construct_scalar(node)


for _tag in ("!override", "!reset"):
    _ComposeLoader.add_constructor(_tag, _resolve_tag)


def _compose_env(name):
    """{service: {var: raw value}} for one compose file, list or map form."""
    services = yaml.load((REPO / name).read_text(), Loader=_ComposeLoader)["services"]
    out = {}
    for svc, spec in services.items():
        env = (spec or {}).get("environment") or {}
        if isinstance(env, list):
            env = dict(e.split("=", 1) if "=" in e else (e, None) for e in env)
        out[svc] = {k: (None if v is None else str(v)) for k, v in env.items()}
    return out


def _fallback(raw, var):
    """The value compose uses when the operator's .env leaves var unset."""
    m = re.fullmatch(r"\$\{" + var + r":-([^}]*)\}", raw or "")
    return m.group(1) if m else raw


def test_the_self_host_compose_turns_sample_rows_off_in_both_checkers():
    env = _compose_env(SELF_HOST)
    for svc in CHECKERS:
        assert svc in env, f"no {svc} service in {SELF_HOST}"
        got = env[svc].get(SAMPLE_ROWS)
        # Literal, not ${...:-false}: a .env line must not be able to turn
        # them on in one container and leave the other off.
        assert got == "false", (
            f"{SELF_HOST} {svc}: {SAMPLE_ROWS}={got!r}, want \"false\". Unset "
            "means allowed (cloud), so this container would let sample rows through."
        )


@pytest.mark.parametrize("name", CLOUD)
def test_the_cloud_compose_never_sets_sample_rows(name):
    setters = sorted(svc for svc, env in _compose_env(name).items() if SAMPLE_ROWS in env)
    assert not setters, (
        f"{name} sets {SAMPLE_ROWS} in {setters}. Cloud leaves it unset (allowed); "
        "the self-host value belongs in docker-compose.quickstart.yml only."
    )


def test_the_self_host_compose_keeps_one_workspace():
    got = _compose_env(SELF_HOST).get("api-gateway", {}).get(SINGLE_WORKSPACE)
    assert got == "true", (
        f"{SELF_HOST} api-gateway: {SINGLE_WORKSPACE}={got!r}, want \"true\". Unset "
        "means multi-workspace (cloud), so a self-host install would let anyone "
        "create workspaces."
    )


@pytest.mark.parametrize("name", CLOUD)
def test_the_cloud_compose_never_sets_single_workspace(name):
    setters = sorted(svc for svc, env in _compose_env(name).items() if SINGLE_WORKSPACE in env)
    assert not setters, (
        f"{name} sets {SINGLE_WORKSPACE} in {setters}. Cloud leaves it unset "
        "(multi-workspace); the self-host value belongs in docker-compose.quickstart.yml only."
    )


@pytest.mark.parametrize("name", (SELF_HOST, *CLOUD))
def test_ask_and_the_agent_gateway_fall_back_to_on(name):
    gateway = _compose_env(name).get("api-gateway", {})
    for var in ON_BY_DEFAULT:
        assert var in gateway, f"{name} api-gateway no longer passes {var}"
        assert _fallback(gateway[var], var) == "true", (
            f"{name} api-gateway: {var}={gateway[var]!r} falls back to off, "
            "undoing the code default (on)."
        )


def _render(*extra):
    proc = subprocess.run(
        ["helm", "template", "r", str(CHART), *RENDER_FLAGS, *extra],
        capture_output=True,
        text=True,
        timeout=180,
        cwd=str(REPO),
    )
    assert proc.returncode == 0, f"helm template {' '.join(extra)} failed:\n{proc.stderr[-3000:]}"
    env = {}
    for doc in yaml.safe_load_all(proc.stdout):
        if not doc or doc.get("kind") != "Deployment":
            continue
        for c in doc["spec"]["template"]["spec"]["containers"]:
            env[c["name"]] = {e["name"]: e.get("value") for e in c.get("env", [])}
    missing = [c for c in CHECKERS if c not in env]
    assert not missing, f"no {missing} container in the render; containers: {sorted(env)}"
    return env


# (extra flags, the value both containers must read). Removing the key must
# keep them off: unset in the code means allowed.
CHART_CASES = {
    "chart default": ([], "false"),
    "value removed": (["--set", "generation.llm.sampleRowsAllowed=null"], "false"),
    "operator opts in": (["--set", "generation.llm.sampleRowsAllowed=true"], "true"),
}


@pytest.mark.skipif(shutil.which("helm") is None, reason="helm not installed")
@pytest.mark.parametrize("case", CHART_CASES)
def test_the_chart_gives_both_checkers_one_value(case):
    extra, want = CHART_CASES[case]
    env = _render(*extra)
    got = {c: env[c].get(SAMPLE_ROWS) for c in CHECKERS}
    assert got == {c: want for c in CHECKERS}, (
        f"{case}: {SAMPLE_ROWS} renders as {got}, want {want!r} on both. A container "
        "without it reads unset, which allows sample rows."
    )


# Removing the value must keep one workspace: unset in the code means many.
SINGLE_WORKSPACE_CASES = {
    "chart default": ([], "true"),
    "value removed": (["--set", "apiGateway.singleWorkspace=null"], "true"),
    "operator opts out": (["--set", "apiGateway.singleWorkspace=false"], "false"),
}


@pytest.mark.skipif(shutil.which("helm") is None, reason="helm not installed")
@pytest.mark.parametrize("case", SINGLE_WORKSPACE_CASES)
def test_the_chart_keeps_one_workspace(case):
    extra, want = SINGLE_WORKSPACE_CASES[case]
    got = _render(*extra)["api-gateway"].get(SINGLE_WORKSPACE)
    assert got == want, (
        f"{case}: api-gateway {SINGLE_WORKSPACE}={got!r}, want {want!r}. Unset "
        "means multi-workspace (cloud)."
    )
