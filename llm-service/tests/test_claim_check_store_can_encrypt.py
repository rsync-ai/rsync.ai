"""The claim-check store must be able to do the encryption its client asks for.

minio-mcp writes every claim-check object with ServerSideEncryption=AES256 by
default (`_sse_enabled`, RSYNC_MINIO_SSE unset = on). SSE-S3 on MinIO needs a
KMS key, and no compose file, chart or installer gave the bundled MinIO one.
So every put was refused with

    Server side encryption specified but KMS is not configured

and the connector caught it, logged "MinIO SSE (AES256) rejected ... retrying
without SSE" and wrote the object unencrypted. Claim-check objects hold the rows
of a batch, so batch data sat at rest in plain form on every install while the
code said it was encrypted. Found on prod 2026-10-15 (the warning on every put).

Bug class: a store the client is configured to ask for SSE cannot honour it.
Checked here for every shape that runs the bundled MinIO a claim-check client
can reach: the compose files (dev, prod, staging, quickstart, e2e), the env
templates an operator copies, install.sh (fresh .env and the upgrade backfill),
install-k8s.sh and the Helm chart.

The key FORMAT is part of the check, not decoration: MinIO refuses to start on a
malformed key ("FATAL Failed to connect to KMS: kms: invalid secret key format",
exit 1), and everything that waits on its health then never starts either. So a
template placeholder like `change-me` would turn the fix into an outage.
"""
import base64
import os
import pathlib
import re
import shutil
import subprocess

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
CONNECTOR = REPO / "shared/mcp-connectors/internal/minio/versions/v1.0.0/connector.py"
INSTALL_SH = REPO / "install.sh"
INSTALL_K8S = REPO / "install-k8s.sh"
CHART = REPO / "deploy/helm/rsync-ai"
BASH = shutil.which("bash") or "/bin/bash"
VAR = "MINIO_KMS_SECRET_KEY"

# MinIO servers that no claim-check client talks to. Each one is a deliberate
# decision; a new MinIO server anywhere fails below until it is classified.
NOT_A_CLAIM_CHECK_STORE = {
    # The blob lane stages file bytes as-is; nothing sends SSE to it.
    "blob-staging",
}

class _ComposeLoader(yaml.SafeLoader):
    pass


def _resolve_tag(loader, node):
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node, deep=True)
    if isinstance(node, yaml.MappingNode):
        return loader.construct_mapping(node, deep=True)
    return loader.construct_scalar(node)


for _tag in ("!override", "!reset"):
    _ComposeLoader.add_constructor(_tag, _resolve_tag)


def _load(path):
    return yaml.load(path.read_text(), Loader=_ComposeLoader) or {}


KEY_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*:([A-Za-z0-9+/]{43}=)")


def _well_formed(value: str) -> bool:
    m = KEY_RE.fullmatch(value or "")
    return bool(m) and len(base64.b64decode(m.group(1))) == 32


def _compose_files():
    out = subprocess.run(["git", "ls-files", "docker-compose*.yml"], cwd=REPO, capture_output=True, text=True, check=True)
    files = [REPO / f for f in out.stdout.split()]
    assert len(files) >= 10, f"only {len(files)} compose files found; the glob is wrong"
    return files


def _env_of(service: dict) -> dict:
    env = service.get("environment") or {}
    if isinstance(env, list):
        return dict(e.split("=", 1) if "=" in e else (e, "") for e in env)
    return {k: "" if v is None else str(v) for k, v in env.items()}


def _minio_servers():
    """(file, service name, env) for every service that runs `minio server`."""
    found = []
    for f in _compose_files():
        services = _load(f).get("services") or {}
        for name, svc in services.items():
            image = str((svc or {}).get("image", ""))
            cmd = svc.get("command") or ""
            cmd = " ".join(cmd) if isinstance(cmd, list) else str(cmd)
            if "minio" in image and cmd.strip().startswith("server"):
                found.append((f.name, name, _env_of(svc)))
    return found


def test_the_connector_still_asks_for_sse_by_default():
    # The premise of every check below. If SSE ever becomes opt-in, this file's
    # obligations change with it -- so the premise is asserted, not assumed.
    src = CONNECTOR.read_text()
    assert '_env("RSYNC_MINIO_SSE", "true")' in src, "minio-mcp no longer defaults SSE on; revisit this guard"
    assert 'ServerSideEncryption="AES256"' in src


def test_every_bundled_claim_check_minio_has_a_kms_key():
    servers = _minio_servers()
    claim_check = [(f, n, e) for f, n, e in servers if n not in NOT_A_CLAIM_CHECK_STORE]
    assert {"docker-compose.yml", "docker-compose.quickstart.yml"} <= {f for f, _, _ in claim_check}, (
        f"the census did not find the base and quickstart MinIO: {servers}"
    )
    missing = [f"{f}:{n}" for f, n, e in claim_check if VAR not in e]
    assert not missing, (
        f"MinIO servers a claim-check client can reach that set no {VAR}: {missing}. minio-mcp "
        "asks for SSE on every put; without a KMS key MinIO refuses and the object is written "
        "unencrypted. If this server is not a claim-check store, add it to NOT_A_CLAIM_CHECK_STORE "
        "with the reason."
    )


def test_a_literal_default_key_is_well_formed():
    for f, n, e in _minio_servers():
        if VAR not in e:
            continue
        v = e[VAR]
        m = re.fullmatch(r"\$\{" + VAR + r":-([^}]*)\}", v)
        literal = m.group(1) if m else (None if v.startswith("${") else v)
        if literal is not None:
            assert _well_formed(literal), f"{f}:{n} {VAR} default {literal!r} would make MinIO exit at start"


def test_prod_and_quickstart_refuse_to_render_without_the_key():
    # No working default exists for these two: a key shipped in a public file is
    # no key, and a missing one silently disables the encryption again.
    prod = _load(REPO / "docker-compose.prod.yml")["services"]["minio"]
    qs = _load(REPO / "docker-compose.quickstart.yml")["services"]["minio"]
    for label, svc in (("docker-compose.prod.yml", prod), ("docker-compose.quickstart.yml", qs)):
        v = _env_of(svc).get(VAR, "")
        assert v.startswith("${" + VAR + ":?"), f"{label} minio {VAR} must be :?-required, got {v!r}"


@pytest.mark.parametrize("template", [".env.example", ".env.prod.example", ".env.staging.example"])
def test_every_template_carries_a_well_formed_key(template):
    m = re.search(rf"^{VAR}=(.*)$", (REPO / template).read_text(), re.M)
    assert m, f"{template} has no {VAR} line; a stand-up from it either fails to render or runs without SSE"
    assert _well_formed(m.group(1).strip()), f"{template} {VAR} is not name:base64(32 bytes); MinIO would exit at start"


# --- install.sh --------------------------------------------------------------


def _install_lib(tmp_path):
    lib = tmp_path / "install-lib.sh"
    lib.write_text("\n".join(INSTALL_SH.read_text().splitlines()[:-1]) + "\n")
    return lib


def _run_bash(tmp_path, body, path=None):
    driver = tmp_path / "drive.sh"
    driver.write_text("set -euo pipefail\n" + body)
    env = {
        "PATH": path or os.environ.get("PATH", "/usr/bin:/bin"),
        "HOME": str(tmp_path),
        "LC_ALL": "C",
        "TMPDIR": os.environ.get("TMPDIR", "/tmp"),
        "PUBLIC_HOST": "localhost",
    }
    return subprocess.run([BASH, str(driver)], capture_output=True, text=True, env=env, timeout=60)


def _dotenv_value(path, name):
    m = re.search(rf"^(?:export )?{name}=(.*)$", path.read_text(), re.M)
    return m.group(1).strip() if m else None


def test_a_fresh_install_writes_a_well_formed_key(tmp_path):
    keys = []
    for i in range(2):
        d = tmp_path / f"i{i}"
        d.mkdir()
        cp = _run_bash(d, f'source "{_install_lib(d)}"\nINSTALL_DIR="{d}"\nTTY_OK=0\nprompt_env\nwrite_env\n')
        assert cp.returncode == 0, cp.stdout + cp.stderr
        v = _dotenv_value(d / ".env", VAR)
        assert v is not None, f"install.sh wrote no {VAR}; the quickstart refuses to render"
        assert _well_formed(v), f"install.sh wrote a malformed {VAR}; MinIO would exit at start"
        keys.append(v)
    assert keys[0] != keys[1], "two installs got the same KMS key"


def test_the_key_generator_works_without_openssl(tmp_path):
    # generate_secret has an urandom fallback for boxes without openssl; so must this.
    bindir = tmp_path / "bin"
    bindir.mkdir()
    for d in ("/usr/bin", "/bin"):
        for name in os.listdir(d):
            if name != "openssl" and not (bindir / name).exists():
                (bindir / name).symlink_to(os.path.join(d, name))
    cp = _run_bash(tmp_path, f'source "{_install_lib(tmp_path)}"\nk=$(generate_kms_key)\necho "KEY=$k"\n', path=str(bindir))
    assert cp.returncode == 0, cp.stdout + cp.stderr
    assert _well_formed(cp.stdout.split("KEY=", 1)[1].strip()), cp.stdout


def _check_env_required(tmp_path, env_text):
    (tmp_path / ".env").write_text(env_text)
    qs = REPO / "docker-compose.quickstart.yml"
    return _run_bash(
        tmp_path,
        f'source "{_install_lib(tmp_path)}"\nINSTALL_DIR="{tmp_path}"\nCOMPOSE_ARGS=(-f "{qs}")\ncheck_env_required\n',
    )


def _full_quickstart_env():
    lines = []
    for v in sorted(set(re.findall(r"\$\{([A-Z_][A-Z0-9_]*):\?", (REPO / "docker-compose.quickstart.yml").read_text()))):
        lines.append(f"{v}=x" * (v != VAR))
    return "\n".join(l for l in lines if l) + "\n"


def test_an_upgrade_backfills_the_key(tmp_path):
    # An install made before this fix has no key; its MinIO has nothing encrypted
    # under one, so a first key strands nothing. The re-run must add it rather
    # than stop -- the quickstart's :? would otherwise refuse to render.
    cp = _check_env_required(tmp_path, _full_quickstart_env())
    assert cp.returncode == 0, cp.stdout + cp.stderr
    v = _dotenv_value(tmp_path / ".env", VAR)
    assert v and _well_formed(v), f"the upgrade backfill wrote {v!r} for {VAR}"


def test_an_upgrade_never_replaces_an_existing_key(tmp_path):
    # Replacing it would make every object and IAM entry MinIO encrypted under
    # the old key unreadable ("failed to decrypt ciphertext").
    kept = "rsync-ai-kms:" + base64.b64encode(bytes(range(32))).decode()
    cp = _check_env_required(tmp_path, _full_quickstart_env() + f"{VAR}={kept}\n")
    assert cp.returncode == 0, cp.stdout + cp.stderr
    assert _dotenv_value(tmp_path / ".env", VAR) == kept


# --- Kubernetes --------------------------------------------------------------


def _k8s_env(tmp_path):
    env = {k: v for k, v in os.environ.items() if not k.startswith("RSYNC_")}
    env.pop("OPENAI_API_KEY", None)
    env.update(RSYNC_INSTALL_DIR=str(tmp_path / "inst"), RSYNC_CHART=str(CHART), HOME=str(tmp_path))
    return env


def _minio_container(docs):
    for d in docs:
        if d.get("kind") == "StatefulSet" and d["metadata"]["name"].endswith("-minio"):
            return d["spec"]["template"]["spec"]["containers"][0]
    pytest.fail("no MinIO StatefulSet rendered")


@pytest.mark.skipif(shutil.which("helm") is None, reason="helm not installed")
def test_install_k8s_gives_the_bundled_minio_a_key(tmp_path):
    cp = subprocess.run([BASH, str(INSTALL_K8S), "--render-only"], env=_k8s_env(tmp_path), capture_output=True, text=True, timeout=180)
    assert cp.returncode == 0, cp.stdout + cp.stderr
    key = _dotenv_value(tmp_path / "inst" / ".env", VAR)
    assert key and _well_formed(key), f"install-k8s.sh wrote {key!r} for {VAR}"

    out = subprocess.run(
        ["helm", "template", "rsync-ai", str(CHART), "-f", str(tmp_path / "inst" / "values.generated.yaml")],
        capture_output=True, text=True, timeout=120,
    )
    assert out.returncode == 0, out.stderr
    docs = [d for d in yaml.safe_load_all(out.stdout) if d]
    secret = next(d for d in docs if d["kind"] == "Secret" and d["metadata"]["name"] == "rsync-ai-secrets")
    assert secret["stringData"].get(VAR) == key, "the generated key does not reach the chart Secret"
    env = {e["name"]: e for e in _minio_container(docs).get("env", [])}
    assert env.get(VAR, {}).get("valueFrom", {}).get("secretKeyRef", {}).get("key") == VAR, (
        f"the MinIO pod does not read {VAR} from the Secret"
    )


RENDER_FLAGS = [
    "--set", "secrets.jwtSecret=FAKEPLACEHOLDER",
    "--set", "secrets.encryptionKey=FAKEPLACEHOLDER",
    "--set", "secrets.postgresPassword=FAKEPLACEHOLDER",
    "--set", "secrets.minioAccessKey=FAKEPLACEHOLDER",
    "--set", "secrets.minioSecretKey=FAKEPLACEHOLDER",
    "--set", "frontend.publicUrl=https://app.example.com",
    "--set", "frontend.apiUrl=https://api.example.com",
]


def _helm(*extra):
    return subprocess.run(["helm", "template", "r", str(CHART), *RENDER_FLAGS, *extra], capture_output=True, text=True, timeout=180)


@pytest.mark.skipif(shutil.which("helm") is None, reason="helm not installed")
def test_the_chart_without_a_key_still_renders_and_the_pod_still_starts():
    # existingSecret installs predate the key; their Secret has no such entry, and
    # a non-optional secretKeyRef would leave MinIO in CreateContainerConfigError.
    out = _helm()
    assert out.returncode == 0, out.stderr
    ref = {e["name"]: e for e in _minio_container([d for d in yaml.safe_load_all(out.stdout) if d]).get("env", [])}
    assert ref[VAR]["valueFrom"]["secretKeyRef"].get("optional") is True


@pytest.mark.skipif(shutil.which("helm") is None, reason="helm not installed")
def test_the_chart_refuses_a_malformed_key():
    out = _helm("--set", "secrets.minioKmsSecretKey=change-me")
    assert out.returncode != 0 and "minioKmsSecretKey" in out.stderr, (
        "a malformed key rendered; MinIO would exit at start with 'invalid secret key format'"
    )
