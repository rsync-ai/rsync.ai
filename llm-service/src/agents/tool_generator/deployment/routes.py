"""
Connector lifecycle routes — moat-free.

Serves POST /v1/deploy: (re)start or JIT-build an EXISTING connector container.
This is the ONLY connector-lifecycle endpoint the Go data plane calls
(backend-orchestrator server_manager.go -> TOOL_GENERATOR_URL/v1/deploy) for
self-heal / pinned-version JIT builds.

Relocated here (out of agents/integration.py) so the community / OSS image can
serve connector deploy WITHOUT importing the connector-generation package or its
curated config YAML. This module's import closure is stdlib + fastapi/pydantic +
the moat-free deployment service + on-disk connector-path helpers — it must NEVER
import .orchestrator, ..config, or any generation agent. The private (cloud)
image re-mounts this router from register_agentic_routes() so its /v1/deploy
behaviour is byte-for-byte unchanged.
"""

import os
import re
import json
import asyncio
import hashlib
import logging
from pathlib import Path
from typing import Dict, Any, Optional, Tuple

import httpx
from fastapi import APIRouter, Depends, Header, HTTPException
from pydantic import BaseModel, Field

from src.utils.connector_paths import iter_connector_dirs, resolve_current_dir
from .container_names import versioned_container_name
from .service import DeploymentService

logger = logging.getLogger(__name__)

lifecycle_router = APIRouter(tags=["Connector Lifecycle"])

# The only ENVIRONMENT values under which an unset INTERNAL_SERVICE_SECRET lets a
# call through. Same set as shared/mcp-connectors/oauth/token_manager.py
# _is_dev_env and connector-deployer config.IsDev.
_DEV_ENVIRONMENTS = ("development", "dev")


def require_internal_secret(
    x_internal_secret: Optional[str] = Header(default=None),
) -> None:
    """S2S gate: connector-lifecycle sits on rsync-ai-mcp with untrusted JIT
    connectors and holds docker.sock, so /v1/deploy can build+start containers.
    Mirror api-gateway InternalServiceMiddleware / orchestrator requirePrincipal.
    Enforced when INTERNAL_SERVICE_SECRET is set. When it is unset the call is
    let through ONLY if ENVIRONMENT is explicitly development/dev (the dev
    compose); every other value -- production, staging, a typo, or ENVIRONMENT
    missing altogether -- answers 503. It used to be the other way round (open
    unless ENVIRONMENT said production), so a container started without
    ENVIRONMENT left the docker.sock-backed /v1/deploy open to any caller on
    rsync-ai-mcp. The prod compose also guards presence with ${...:?}; this is
    defense-in-depth for any other launch path.
    """
    secret = (os.getenv("INTERNAL_SERVICE_SECRET") or "").strip()
    if not secret:
        env = (os.getenv("ENVIRONMENT") or "").strip().lower()
        if env not in _DEV_ENVIRONMENTS:
            raise HTTPException(status_code=503, detail="internal_secret_not_configured")
        return
    if (x_internal_secret or "").strip() != secret:
        raise HTTPException(status_code=401, detail="invalid_internal_secret")


_RE_NON_ID = re.compile(r"[^a-z0-9-]+")

# Network aliases a JIT-deployed connector must claim so kafka-mcp-sink can resolve
# it by the SAME unversioned hostname it uses for compose-started connectors.
# MUST stay in lockstep with the canonical map in
# scripts/mcp_generate_compose.py (NETWORK_ALIASES_BY_CONNECTOR_ID).
_NETWORK_ALIASES_BY_CONNECTOR_ID: dict[str, list[str]] = {
    "mysql": ["rsync-ai-mysql-mcp"],
    "postgresql": ["rsync-ai-postgresql-mcp"],
    "aws-s3": ["rsync-ai-aws_s3-mcp", "rsync-ai-aws-s3-mcp"],
}


def _managed_connectors_enabled() -> bool:
    """
    Managed connectors mode:
    - true: tool-generator may build Docker images + start containers (requires docker.sock / Docker daemon)
    - false (default): tool-generator only writes artifacts; operator runs external MCP stack separately
    """
    v = (os.getenv("RSYNC_MANAGED_CONNECTORS") or "").strip().lower()
    return v in ("1", "true", "yes")


def _canonicalize_connector_id(raw: str) -> str:
    """
    Canonical connector id (kebab-case).

    This MUST match how the rest of the platform identifies connectors:
    - metadata.json `id`
    - metadata.json `connector_type`
    - MCP tool prefix (e.g., aws-s3_export)
    """
    s = (raw or "").strip().lower()
    s = s.replace(" ", "-").replace("_", "-")
    s = _RE_NON_ID.sub("", s)
    while "--" in s:
        s = s.replace("--", "-")
    s = s.strip("-")
    # Small compatibility aliases (keep minimal and predictable)
    if s == "s3":
        return "aws-s3"
    if s == "postgres":
        return "postgresql"
    return s


class DeployRequestV1(BaseModel):
    """
    (Re)deploy an existing connector container.

    Used by backend-orchestrator for self-healing: if a connector container is missing,
    start it without regenerating code.
    """
    connector_name: str = Field(..., description="Connector identifier (any alias accepted)")
    version: str = Field("latest", description="Connector version (e.g., 'latest' or 'v1.2.3')")
    build_if_missing: bool = Field(False, description="If true, build image if not present")
    force_rebuild: bool = Field(
        False,
        description=(
            "Regenerate path: always rebuild the image, and recreate the container unless "
            "start is false, so the freshly written code ships. Leaves the self-heal "
            "start-only path unchanged when false."
        ),
    )
    start: bool = Field(
        True,
        description=(
            "Read only with force_rebuild. False starts nothing: generation sends false, "
            "so a new connector's code runs only once a pipeline uses it (the "
            "orchestrator's infra preflight starts it then). The image is built now, or, "
            "under the connector-deployer, by the deployer when it first starts the connector."
        ),
    )


class DeployResponseV1(BaseModel):
    success: bool
    connector_id: str
    version: str
    container_name: str
    image: Optional[str] = None
    started: bool = False
    built: bool = False
    # True when an image build was kicked off in the background (a cold build can take
    # minutes). The caller should poll the container's /health rather than block on /deploy.
    building: bool = False
    error_message: Optional[str] = None
    # "build" when the connector's image failed to build: the files must change
    # before it can run, so retrying is pointless (see _BUILD_FAILURES).
    error_stage: Optional[str] = None


# In-flight JIT builds, keyed by target container_name. Prevents two concurrent /deploy
# calls (e.g. two pipelines pinned to the same old version) from launching duplicate
# builds. Accessed only from async handlers on a single event loop, so a plain set with
# synchronous check-then-add (no await between) is race-free.
_INFLIGHT_BUILDS: set = set()

# Deployer mode (DEPLOYER_URL set, as the prod compose sets it): one deployer
# /v1/deploy call builds a missing image and starts the container, blocking for as
# long as the build takes (up to the deployer's 900s). The orchestrator gives this
# route 15s (server_manager.go deployCallTimeout), so it used to give up mid-build,
# read the timeout as "still deploying", and start the same build again on every
# retry; a build that failed was only logged, so the user was told the connector
# "is still being set up" for good.
#
# Now each deployer call runs as a task, one per container name, and a request
# waits for it at most _DEPLOY_ANSWER_WITHIN_S before answering building=true; a
# retry joins the running task. A build failure is remembered against a
# fingerprint of versions/<v>/ and answered at once, with the end of the build log,
# until those files change (or a force_rebuild, or a tool-generator restart).
_DEPLOYER_TASKS: Dict[str, asyncio.Task] = {}
_BUILD_FAILURES: Dict[str, Tuple[str, str]] = {}
_DEPLOY_ANSWER_WITHIN_S = 10.0

BUILD_FAILED_PREFIX = "connector image failed to build"
# The deployer's build errors (connector-deployer dockerx/backend.go buildLegacy /
# buildRootless): "<builder> (...) failed: <last 1500 chars of build output>". A
# timeout or a pull of the built image is not the connector's fault, so not matched.
_BUILD_FAILED_RE = re.compile(r"(?:docker build|buildctl)\b[^:]*\bfailed:\s*(.*)\Z", re.S)
_BUILD_LOG_TAIL_CHARS = 1000


def _build_failure_message(start_info: str) -> Optional[str]:
    """The user-facing message for a deployer answer that is an image build failure,
    with the end of the build log; None for any other failure."""
    m = _BUILD_FAILED_RE.search(start_info or "")
    if not m:
        return None
    log_tail = m.group(1).strip()
    if len(log_tail) > _BUILD_LOG_TAIL_CHARS:
        log_tail = log_tail[-_BUILD_LOG_TAIL_CHARS:]
        log_tail = log_tail[log_tail.find("\n") + 1:] if "\n" in log_tail else log_tail
    return f"{BUILD_FAILED_PREFIX}. End of the build log:\n{log_tail}"


def _version_dir_fingerprint(version_dir: Path) -> str:
    """What an image built from versions/<v>/ is made of: every file's path and bytes.
    Bytecode is left out; whoever imports the code writes it."""
    h = hashlib.sha256()
    for p in sorted(version_dir.rglob("*")):
        if not p.is_file() or "__pycache__" in p.parts or p.suffix == ".pyc":
            continue
        h.update(str(p.relative_to(version_dir)).encode())
        h.update(b"\0")
        h.update(p.read_bytes())
        h.update(b"\0")
    return h.hexdigest()


def _deployer_task(container_name: str, fingerprint: str, start) -> asyncio.Task:
    """The running deployer call for container_name, started by start() if none is."""
    task = _DEPLOYER_TASKS.get(container_name)
    if task is not None and not task.done():
        return task
    task = asyncio.ensure_future(start())

    def _done(t: asyncio.Task) -> None:
        if _DEPLOYER_TASKS.get(container_name) is t:
            _DEPLOYER_TASKS.pop(container_name, None)
        if t.cancelled() or t.exception() is not None:
            return
        ok, info = t.result()
        message = None if ok else _build_failure_message(info)
        if message:
            _BUILD_FAILURES[container_name] = (fingerprint, message)
            logger.error(f"❌ Image build failed for {container_name}; refusing it until its files change")

    task.add_done_callback(_done)
    _DEPLOYER_TASKS[container_name] = task
    return task


async def _build_and_start_in_background(
    *,
    deploy_service,
    connector_id: str,
    concrete_version: str,
    version_dir: Path,
    container_name: str,
    net_aliases,
    recreate: bool = False,
    start: bool = True,
) -> None:
    """Build a connector image from versions/<v>/ and, unless start is false, start
    its container.

    Runs as a detached task so the /deploy response can return immediately; the caller
    polls the container's /health to learn when it becomes ready. Always clears the
    in-flight marker on exit so a failed build can be retried on the next run.
    """
    try:
        build_result = await deploy_service.docker_builder.build(
            connector_dir=version_dir,
            name=connector_id,
            version=concrete_version,
        )
        if not build_result.success:
            logger.error(
                f"❌ JIT build failed for {container_name}: "
                f"{build_result.error_message or 'unknown error'}"
            )
            return
        if not start:
            # Under the connector-deployer, build() only defers: the deployer builds the
            # image when it first starts the connector, so nothing was built here.
            deferred = getattr(deploy_service.docker_builder, "use_deployer", False)
            logger.info(
                f"✅ {build_result.full_image_name}: "
                f"{'build deferred to the deployer' if deferred else 'built'}; not started"
            )
            return
        start_ok, start_info = await deploy_service.docker_builder.start_container(
            image_name=build_result.full_image_name,
            container_name=container_name,
            port=None,
            network=deploy_service.docker_network,
            env_vars={
                "MCP_CONNECTOR_NAME": connector_id,
                "MCP_CONNECTOR_VERSION": concrete_version,
                "MCP_PORT": "8000",
                "PORT": "8000",
                "MCP_HTTP_MODE": "true",
                "DOCKER_CONTAINER": "true",
            },
            aliases=net_aliases,
            recreate=recreate,
        )
        if start_ok:
            logger.info(f"✅ JIT build+start complete for {container_name}")
        else:
            logger.error(f"❌ JIT start failed for {container_name}: {start_info}")
    except Exception as e:
        logger.error(f"❌ JIT build task crashed for {container_name}: {e}")
    finally:
        _INFLIGHT_BUILDS.discard(container_name)


@lifecycle_router.post("/deploy", dependencies=[Depends(require_internal_secret)])
async def deploy_connector_v1(request: DeployRequestV1) -> Dict[str, Any]:
    """
    Ensure a connector container is running (HTTP MCP mode).

    Default behavior is "start-only" (fast). If image is missing and build_if_missing=true,
    we build it from the connector's latest versioned snapshot.
    """
    tools_dir = os.getenv("TOOLS_DIR", "/app/shared/mcp-connectors")
    connector_id = _canonicalize_connector_id(request.connector_name)

    if not _managed_connectors_enabled():
        raise HTTPException(
            status_code=501,
            detail=(
                "Managed connector deployment is disabled for this tool-generator instance. "
                "Start the external `rsync-ai-mcp` stack (or set RSYNC_MANAGED_CONNECTORS=true to enable managed mode)."
            ),
        )

    # Resolve on-disk directory across the new nested layout:
    # - <base>/public/<category>/<id>
    # - <base>/internal/<id>
    # - legacy flat: <base>/<id>
    base = Path(tools_dir)
    if base.name in ("public", "internal"):
        base = base.parent

    candidates = {
        connector_id,
        connector_id.replace("-", "_"),
        connector_id.replace("_", "-"),
    }

    def find_connector_dir() -> Optional[Path]:
        # 1) internal direct
        internal_root = base / "internal"
        if internal_root.is_dir():
            for cand in candidates:
                p = internal_root / cand
                if p.is_dir():
                    return p

        # A public connector root is identified by latest.json (the version
        # pointer). Requiring it here stops an empty lock-only dir (e.g. a stray
        # flat public/mysql holding only .promotion.lock) from shadowing the
        # canonical category-layout root public/database/mysql. See
        # KI-MYSQL-DUP-DIR. (Internal step 1 is left as-is: context7 has no
        # latest.json; step 5 is the metadata-driven safety net.)
        def _is_root(p: Path) -> bool:
            return p.is_dir() and (p / "latest.json").is_file()

        # 2) public direct (flat)
        public_root = base / "public"
        if public_root.is_dir():
            for cand in candidates:
                p = public_root / cand
                if _is_root(p):
                    return p

            # 3) public category layout
            for cat in public_root.iterdir():
                if not cat.is_dir():
                    continue
                for cand in candidates:
                    p = cat / cand
                    if _is_root(p):
                        return p

        # 4) legacy flat under base
        for cand in candidates:
            p = base / cand
            if _is_root(p):
                return p

        # 5) metadata-driven fallback (future-proof)
        roots = [base, base / "public", base / "internal"]
        for r in roots:
            if not r.is_dir():
                continue
            try:
                for cdir in iter_connector_dirs(r):
                    meta_path = resolve_current_dir(cdir) / "metadata.json"
                    if not meta_path.exists():
                        continue
                    try:
                        data = json.loads(meta_path.read_text())
                    except Exception:
                        continue
                    cid = _canonicalize_connector_id(str(data.get("id") or data.get("connector_type") or cdir.name))
                    if cid == connector_id:
                        return cdir
            except Exception:
                continue
        return None

    connector_dir = find_connector_dir()
    if connector_dir is None:
        raise HTTPException(404, f"Connector '{request.connector_name}' not found in TOOLS_DIR")

    # Resolve concrete version
    requested_version = (request.version or "latest").strip()
    if requested_version == "":
        requested_version = "latest"

    if requested_version.lower() == "latest":
        latest_path = connector_dir / "latest.json"
        if not latest_path.exists():
            raise HTTPException(400, f"Connector '{connector_id}' is not versioned (missing latest.json)")
        try:
            latest = json.loads(latest_path.read_text())
        except Exception as e:
            raise HTTPException(500, f"Failed to read latest.json for '{connector_id}': {e}")
        concrete_version = (latest.get("current_version") or "").strip()
        if not concrete_version:
            raise HTTPException(500, f"latest.json for '{connector_id}' has no current_version")
    else:
        concrete_version = requested_version
        if not concrete_version.startswith("v"):
            concrete_version = f"v{concrete_version}"

    version_dir = connector_dir / "versions" / concrete_version
    if not version_dir.is_dir():
        raise HTTPException(404, f"Version path not found: {version_dir}")

    # Container naming: ALWAYS versioned.
    #
    # We resolve "latest" to a concrete version above, so runtime should never depend on a
    # stable container name like `rsync-ai-<id>-mcp` (which would break pinned pipelines).
    # The prefix is STACK_PREFIX, matching the name the orchestrator looks for.
    container_name = versioned_container_name(connector_id, concrete_version)

    deploy_service = DeploymentService()

    # Attempt fast start first (no build).
    image_ref = f"mcp-{connector_id}:{concrete_version}"
    started = False
    built = False
    # Every JIT-deployed connector claims the unversioned hostname first, matching
    # mcp_generate_compose.py. The map below only adds legacy extras (aws_s3). The
    # alias stays `rsync-ai-` on every stack: kafka-mcp-sink resolves it without
    # reading STACK_PREFIX.
    unversioned_alias = f"rsync-ai-{connector_id}-mcp"
    net_aliases = [unversioned_alias] + [
        a
        for a in _NETWORK_ALIASES_BY_CONNECTOR_ID.get(connector_id, [])
        if a != unversioned_alias
    ]

    # Regenerate path: the on-disk code for this version was just (re)written, but an
    # image tagged mcp-<id>:<version> may already exist from a prior build. The
    # start-only fast path below would reuse that stale image (and reuse an
    # already-running container), silently shipping the OLD code. When force_rebuild is
    # set, rebuild THIS version and recreate its container instead. Self-heal /deploy
    # callers leave force_rebuild False and keep the start-only path.
    if request.force_rebuild:
        _BUILD_FAILURES.pop(container_name, None)
        if container_name in _INFLIGHT_BUILDS:
            logger.info(f"🔁 Build already in flight for {container_name} — returning building=true")
        else:
            _INFLIGHT_BUILDS.add(container_name)
            asyncio.create_task(
                _build_and_start_in_background(
                    deploy_service=deploy_service,
                    connector_id=connector_id,
                    concrete_version=concrete_version,
                    version_dir=version_dir,
                    container_name=container_name,
                    net_aliases=net_aliases,
                    recreate=True,
                    start=request.start,
                )
            )
            logger.info(
                f"🏗️  Force-rebuild {image_ref} in background for {container_name} (regenerate"
                f"{'' if request.start else ', build only'})"
            )
        return DeployResponseV1(
            success=True,
            connector_id=connector_id,
            version=concrete_version,
            container_name=container_name,
            image=image_ref,
            started=False,
            built=False,
            building=True,
        ).model_dump(exclude_none=False, exclude_defaults=False)

    def _start():
        return deploy_service.docker_builder.start_container(
            image_name=image_ref,
            container_name=container_name,
            port=None,
            network=deploy_service.docker_network,
            env_vars={
                "MCP_CONNECTOR_NAME": connector_id,
                "MCP_CONNECTOR_VERSION": concrete_version,
                "MCP_PORT": "8000",
                "PORT": "8000",
                "MCP_HTTP_MODE": "true",
                "DOCKER_CONTAINER": "true",
            },
            aliases=net_aliases,
        )

    if getattr(deploy_service.docker_builder, "use_deployer", False) is True:
        fingerprint = _version_dir_fingerprint(version_dir)
        failed = _BUILD_FAILURES.get(container_name)
        if failed is not None and failed[0] == fingerprint:
            return DeployResponseV1(
                success=False,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                error_message=failed[1],
                error_stage="build",
            ).model_dump(exclude_none=False, exclude_defaults=False)
        _BUILD_FAILURES.pop(container_name, None)
        task = _deployer_task(container_name, fingerprint, _start)
        done, _ = await asyncio.wait({task}, timeout=_DEPLOY_ANSWER_WITHIN_S)
        if not done:
            logger.info(f"🏗️  Deployer still building/starting {container_name} — answering building=true")
            return DeployResponseV1(
                success=True,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                building=True,
            ).model_dump(exclude_none=False, exclude_defaults=False)
        start_ok, start_info = task.result()
        build_failure = None if start_ok else _build_failure_message(start_info)
        if build_failure:
            return DeployResponseV1(
                success=False,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                error_message=build_failure,
                error_stage="build",
            ).model_dump(exclude_none=False, exclude_defaults=False)
    else:
        start_ok, start_info = await _start()
    if start_ok:
        started = True
    else:
        # Image missing and a build is allowed: build IN THE BACKGROUND and return
        # immediately. A cold build can take minutes — far longer than the orchestrator's
        # deploy POST timeout — so blocking the response would just make the caller give
        # up. Instead we kick off the build as a detached task and report building=true;
        # the caller polls the container's /health (with an extended deadline) to learn
        # when it's ready. This is the path that serves pipelines pinned to an old version
        # whose image was pruned after a newer version was promoted.
        if request.build_if_missing and "Image not found" in (start_info or ""):
            if container_name in _INFLIGHT_BUILDS:
                logger.info(f"🔁 Build already in flight for {container_name} — returning building=true")
            else:
                _INFLIGHT_BUILDS.add(container_name)
                asyncio.create_task(
                    _build_and_start_in_background(
                        deploy_service=deploy_service,
                        connector_id=connector_id,
                        concrete_version=concrete_version,
                        version_dir=version_dir,
                        container_name=container_name,
                        net_aliases=net_aliases,
                    )
                )
                logger.info(
                    f"🏗️  JIT building {image_ref} in background for {container_name} "
                    f"(pinned version not pre-built)"
                )
            return DeployResponseV1(
                success=True,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                started=False,
                built=False,
                building=True,
            ).model_dump(exclude_none=False, exclude_defaults=False)
        else:
            return DeployResponseV1(
                success=False,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                started=False,
                built=False,
                error_message=start_info or "Failed to start container",
            ).model_dump(exclude_none=False, exclude_defaults=False)

    # Verify health (best-effort with retries). Containers often take a few seconds to boot.
    try:
        import time

        deadline = time.time() + 30.0  # seconds
        last_err: Optional[str] = None
        async with httpx.AsyncClient(timeout=2.0) as client:
            while time.time() < deadline:
                try:
                    resp = await client.get(f"http://{container_name}:8000/health")
                    if resp.status_code == 200:
                        last_err = None
                        break
                    last_err = f"HTTP {resp.status_code}"
                except Exception as e:
                    last_err = str(e)

                await asyncio.sleep(1.0)

        if last_err:
            return DeployResponseV1(
                success=False,
                connector_id=connector_id,
                version=concrete_version,
                container_name=container_name,
                image=image_ref,
                started=True,
                built=built,
                error_message=f"Health check failed: {last_err}",
            ).model_dump(exclude_none=False, exclude_defaults=False)
    except Exception as e:
        return DeployResponseV1(
            success=False,
            connector_id=connector_id,
            version=concrete_version,
            container_name=container_name,
            image=image_ref,
            started=True,
            built=built,
            error_message=f"Health check failed: {e}",
        ).model_dump(exclude_none=False, exclude_defaults=False)

    return DeployResponseV1(
        success=True,
        connector_id=connector_id,
        version=concrete_version,
        container_name=container_name,
        image=image_ref,
        started=started,
        built=built,
    ).model_dump(exclude_none=False, exclude_defaults=False)
