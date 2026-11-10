"""Every core image we publish carries THIRD_PARTY_NOTICES.md at /licenses/.

The notices file is what the shipped dependencies' licences ask a redistributor to
hand over with the binary, and the binary we hand over is an image -- yet no
Dockerfile copied it, so an image pulled from ghcr.io carried none of it.

The fix has two halves, and each can be undone without the other going red:

  * each Dockerfile in docker-publish.yml's literal `build-and-push` matrix COPYs the
    file into its FINAL stage (a builder stage's copy never reaches the image);
  * those whose build context is a subdirectory cannot reach a repo-root file, so
    they take it from a BuildKit named context called `legal` (the repo root, of which
    the root .dockerignore lets through only the files a COPY names). Every place
    that builds one of them must then pass `legal`, or that build fails at
    `COPY --from=legal` -- docker-publish, every compose file, every script that
    runs `docker build` on one, and every Python argv list that does (the Kafka
    security matrix's run.py, whose kafka-connect build named only its directory and
    so went unseen until CI failed on it). A build site added later is held to it by
    census, not by a list kept here.

Out of scope, and said so in the notices file: the discovered `mcp-*` connector
matrix and the connector images built at runtime (connector-deployer, the tool
generator), which pass only the `shared` context today.
"""

import ast
import fnmatch
import os
import re
import subprocess

import pytest
import yaml

REPO_ROOT = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", ".."))
WORKFLOW = os.path.join(REPO_ROOT, ".github", "workflows", "docker-publish.yml")
CI_FILTERS = os.path.join(REPO_ROOT, ".github", "paths-filters.yml")
NOTICES = "THIRD_PARTY_NOTICES.md"
COPY = re.compile(
    r"^COPY\s+((?:--\S+\s+)*)" + re.escape(NOTICES) + r"\s+/licenses/" + re.escape(NOTICES) + r"\s*$"
)


class _ComposeLoader(yaml.SafeLoader):
    """Reads compose's `!override` / `!reset` tags as the plain value they wrap."""


def _tagged(loader, _suffix, node):
    if isinstance(node, yaml.MappingNode):
        return loader.construct_mapping(node, deep=True)
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node, deep=True)
    return loader.construct_scalar(node)


_ComposeLoader.add_multi_constructor("!", _tagged)


def _workflow():
    with open(WORKFLOW) as fh:
        return yaml.safe_load(fh)


def _core_matrix():
    """-> [(service, context, dockerfile)] of the literal build-and-push matrix."""
    include = _workflow()["jobs"]["build-and-push"]["strategy"]["matrix"]["include"]
    return [(e["service"], os.path.normpath(e["context"]), os.path.normpath(e["dockerfile"])) for e in include]


def _final_stage(dockerfile):
    """The instructions after the last FROM, continuation lines joined."""
    with open(os.path.join(REPO_ROOT, dockerfile)) as fh:
        joined = re.sub(r"\\\n", " ", fh.read())
    lines = [ln.strip() for ln in joined.splitlines() if ln.strip() and not ln.strip().startswith("#")]
    last = max(i for i, ln in enumerate(lines) if re.match(r"FROM\s", ln, re.I))
    return lines[last + 1:]


def _legal_dockerfiles():
    """The matrix Dockerfiles whose context is not the repo root."""
    return {df for _, ctx, df in _core_matrix() if ctx != "."}


def test_the_matrix_was_read():
    services = [s for s, _, _ in _core_matrix()]
    # A vacuity floor, not a census: the matrix had 13 entries when this was written.
    assert len(services) >= 10 and "frontend" in services and "api-gateway" in services, services
    assert _legal_dockerfiles(), "no subdirectory-context image found; the matrix shape changed"


@pytest.mark.parametrize("service,context,dockerfile", _core_matrix(), ids=lambda v: str(v))
def test_each_published_image_copies_the_notices_into_its_final_stage(service, context, dockerfile):
    copies = [m for m in (COPY.match(ln) for ln in _final_stage(dockerfile)) if m]
    assert len(copies) == 1, (
        f"{service}: {dockerfile}'s final stage must COPY {NOTICES} to /licenses/{NOTICES} once; "
        f"found {len(copies)}"
    )
    flags = copies[0].group(1).split()
    if context == ".":
        assert "--from=legal" not in flags, f"{service} builds from the repo root; COPY the file directly"
    else:
        assert "--from=legal" in flags, (
            f"{service} builds from ./{context}, which cannot reach a repo-root file: "
            f"COPY --from=legal {NOTICES} /licenses/{NOTICES}"
        )


def test_docker_publish_passes_the_legal_context():
    steps = _workflow()["jobs"]["build-and-push"]["steps"]
    builds = [s for s in steps if "docker/build-push-action" in str(s.get("uses", ""))]
    assert len(builds) == 1, f"expected one build-push step in build-and-push, found {len(builds)}"
    contexts = str(builds[0].get("with", {}).get("build-contexts", "")).split()
    assert "legal=." in contexts, f"build-and-push must pass `build-contexts: legal=.`; it passes {contexts}"


def _tracked(*patterns):
    out = subprocess.run(["git", "ls-files", *patterns], cwd=REPO_ROOT, capture_output=True, text=True, check=True)
    return out.stdout.split()


def _compose_builds():
    """-> [(file, service, base, build)] for every build stanza that names a context.

    A stanza with only `args` is an overlay that inherits its context from the base
    file -- `docker compose config` on the merged files is what checks those.
    Compose resolves an overlay's relative paths from the project directory (the
    first -f file's), so a context is resolved from the file's own directory, or
    from the repo root when only that one exists.
    """
    found = []
    for f in _tracked("*compose*.yml", "*compose*.yaml"):
        with open(os.path.join(REPO_ROOT, f)) as fh:
            doc = yaml.load(fh, Loader=_ComposeLoader) or {}
        for name, spec in (doc.get("services") or {}).items():
            build = spec.get("build") if isinstance(spec, dict) else None
            if isinstance(build, str):
                build = {"context": build}
            if not isinstance(build, dict) or "context" not in build:
                continue
            base = os.path.dirname(f)
            if not os.path.isdir(os.path.join(REPO_ROOT, base, build["context"])):
                base = ""
            found.append((f, name, base, build))
    return found


def _named_contexts(build):
    ac = build.get("additional_contexts") or {}
    if isinstance(ac, list):
        ac = dict(item.split("=", 1) for item in ac)
    return ac


def test_compose_census_found_the_legal_builds():
    built = {
        os.path.normpath(os.path.join(base, b["context"], b.get("dockerfile", "Dockerfile")))
        for _, _, base, b in _compose_builds()
    }
    # Floor: docker-compose.yml builds the frontend, and docker-compose.oss.yml the seed.
    assert {"frontend/Dockerfile", "shared/mcp-connectors/Dockerfile.seed"} <= built, sorted(built)


def test_every_compose_build_of_a_legal_dockerfile_passes_the_repo_root():
    legal, wrong = _legal_dockerfiles(), []
    for f, name, base, build in _compose_builds():
        dockerfile = os.path.normpath(os.path.join(base, build["context"], build.get("dockerfile", "Dockerfile")))
        if dockerfile not in legal:
            continue
        given = _named_contexts(build).get("legal")
        if given is None or os.path.normpath(os.path.join(base, given)) != ".":
            wrong.append(f"{f} service {name} builds {dockerfile} with legal={given!r}")
    assert not wrong, (
        "these builds fail at `COPY --from=legal`; add `additional_contexts: {legal: <repo root>}`:\n  "
        + "\n  ".join(wrong)
    )


_DOCKER_BUILD = re.compile(r"\bdocker\s+(?:buildx\s+)?build\b")


def _mentions(text, dockerfile):
    base = os.path.basename(dockerfile)
    return dockerfile in text or (base != "Dockerfile" and base in text)


def _context_dir(dockerfile):
    """A build of a default-named `Dockerfile` can name only its directory
    (`docker build -t x shared/internal/infra/kafka-connect`); this matches that
    directory as a whole path, not as the prefix of a longer one. None for a
    Dockerfile with its own name, which any build must spell out."""
    if os.path.basename(dockerfile) != "Dockerfile":
        return None
    return re.compile(r"(?<![\w.-])" + re.escape(os.path.dirname(dockerfile)) + r"/?(?![\w.-])")


def _names_a_legal_build(text):
    legal = _legal_dockerfiles()
    dirs = [rx for rx in map(_context_dir, legal) if rx]
    return any(_mentions(text, df) for df in legal) or any(rx.search(text) for rx in dirs)


def _script_builds():
    """-> [(file, line number, line)] of every `docker build` in a tracked script,
    workflow or Makefile that names a legal Dockerfile -- in the file, or by its
    directory on the build line itself."""
    legal, found = _legal_dockerfiles(), []
    for f in _tracked("*.sh", ".github/workflows/*.yml", "Makefile"):
        with open(os.path.join(REPO_ROOT, f)) as fh:
            text = fh.read()
        named = any(_mentions(text, df) for df in legal)
        for n, line in enumerate(text.splitlines(), 1):
            code = line.split("#", 1)[0] if not line.lstrip().startswith("#") else ""
            if _DOCKER_BUILD.search(code) and (named or _names_a_legal_build(code)):
                found.append((f, n, code))
    return found


def _argv_build_lists(src):
    """-> [(line number, source)] of each list or tuple literal in Python source
    that starts a docker build: `["build", ...]` (run through a `docker()` helper),
    `["docker", "build", ...]` or `["docker", "buildx", "build", ...]`. The whole
    literal is returned, because its arguments span lines."""
    found = []
    for node in ast.walk(ast.parse(src)):
        if not isinstance(node, (ast.List, ast.Tuple)):
            continue
        lead = []
        for e in node.elts:
            if not (isinstance(e, ast.Constant) and isinstance(e.value, str)):
                break
            lead.append(e.value)
        if lead[:1] == ["build"] or lead[:2] == ["docker", "build"] or lead[:3] == ["docker", "buildx", "build"]:
            found.append((node.lineno, ast.get_source_segment(src, node) or ""))
    return found


def _python_builds():
    """-> [(file, line number, source)] of every argv build list in a tracked .py
    file that names a legal Dockerfile or its directory."""
    found = []
    for f in _tracked("*.py"):
        with open(os.path.join(REPO_ROOT, f), errors="replace") as fh:
            src = fh.read()
        if not re.search(r"""['"]build['"]""", src):
            continue
        try:
            builds = _argv_build_lists(src)
        except SyntaxError:
            continue
        found += [(f, n, seg) for n, seg in builds if _names_a_legal_build(seg)]
    return found


def test_a_build_named_by_its_directory_is_seen():
    """Controls for both censuses: each must find a build that names only the
    directory, across lines, and must not take a longer path for it."""
    rx = _context_dir("shared/internal/infra/kafka-connect/Dockerfile")
    assert rx.search("docker build -t x shared/internal/infra/kafka-connect")
    assert rx.search('os.path.join(ROOT, "shared/internal/infra/kafka-connect")')
    assert not rx.search("shared/internal/infra/kafka-connect-extra")
    src = 'jobs = {"c": ["build", "-t", "x",\n        os.path.join(ROOT, "shared/internal/infra/kafka-connect")]}\n'
    (n, seg), = _argv_build_lists(src)
    assert n == 1 and "kafka-connect" in seg and _names_a_legal_build(seg) and "legal=" not in seg
    assert _argv_build_lists('subprocess.run(["go", "build", "./cmd/x"])') == []


def test_every_script_that_builds_a_legal_dockerfile_passes_legal():
    """A script that names a legal Dockerfile and runs `docker build` passes
    `--build-context legal=` on every such line -- a variable Dockerfile path
    (`-f "$CTX/$dockerfile"`) is why this asks of the file, not of the line."""
    builds = _script_builds()
    wrong = [f"{f}:{n}: {code.strip()}" for f, n, code in builds if "--build-context legal=" not in code]
    assert not wrong, "these builds fail at `COPY --from=legal`:\n  " + "\n  ".join(wrong)
    # Floor: the kind loader and the two OSS image scripts.
    assert len(builds) >= 3, f"found {len(builds)} script builds of a legal Dockerfile; the census went blind"


def test_every_python_build_of_a_legal_dockerfile_passes_legal():
    """The same rule for a build run from Python, where `docker build` never
    appears as text: an argv list naming a legal Dockerfile or its directory
    carries `--build-context legal=` within the same literal."""
    builds = _python_builds()
    wrong = [f"{f}:{n}: {' '.join(seg.split())}" for f, n, seg in builds if "legal=" not in seg]
    assert not wrong, (
        "these builds fail at `COPY --from=legal`; add `\"--build-context\", f\"legal={ROOT}\"`:\n  "
        + "\n  ".join(wrong)
    )
    # Floor: the Kafka security matrix builds the kafka-connect image (kafka-matrix/run.py).
    assert len(builds) >= 1, "found no Python build of a legal Dockerfile; the census went blind"


def _subjects():
    """The files this guard found something in -- each must be able to run it."""
    subjects = {os.path.relpath(WORKFLOW, REPO_ROOT)}
    subjects |= {df for _, _, df in _core_matrix()}
    subjects |= {f for f, _, base, b in _compose_builds()
                 if os.path.normpath(os.path.join(base, b["context"], b.get("dockerfile", "Dockerfile")))
                 in _legal_dockerfiles()}
    subjects |= {f for f, _, _ in _script_builds()}
    subjects |= {f for f, _, _ in _python_builds()}
    return sorted(subjects)


@pytest.mark.parametrize("subject", _subjects())
def test_the_ci_filter_covers_every_file_this_guard_reads(subject):
    """Same rule, same fnmatch caveat, as the test of this name in
    test_shipped_images_are_pinned.py: this file runs in llm-service-unit, which is
    gated on the `llm` paths filter, so a PR touching only a Dockerfile or compose
    file it checks must match that filter, or the guard skips exactly the change it
    exists to catch. (A script added somewhere the filter does not reach is found
    by the census on the next run that does fire, not on its own PR.)"""
    with open(CI_FILTERS) as fh:
        pats = yaml.safe_load(fh)["llm"]
    assert any(fnmatch.fnmatch(subject, p) for p in pats), (
        f"`{subject}` is read by this guard, but no `llm` pattern in .github/paths-filters.yml matches it"
    )
