"""Only the public repository publishes to the community image names.

`.github/workflows/docker-publish.yml` is byte-identical in both repositories and pushes
to `ghcr.io/rsync-ai/<service>`. Without a guard, a tag or a dispatch in the private
repository overwrites `:latest` or `:main` -- the tags a self-host install pulls -- with
images built from a tree that carries code the public one deliberately does not ship.
Nothing in the registry refuses a second writer. This was
KI-BOTH-REPOS-PUBLISH-THE-SAME-IMAGE-NAMES, and the decision that closed it (2026-09-28)
is: the public repository is the only publisher of the community images, the private
one publishes nothing, and paid code publishes its own `rsync-enterprise` image from its
own repository.

THE BUG CLASS: A SECOND WRITER INTO A SHARED NAMESPACE
------------------------------------------------------
So the census is every workflow, not one file. A job that can write to the registry --
by holding `packages: write` (or `write-all`), by pushing an image in a step, or by
running `helm push` -- must be guarded to the public repository, and so must every other
job in a workflow that holds one, because a publish workflow that still runs its
discovery and then fails or skips in the private repository is noise at best. The
guard must be a top-level conjunct of the job's `if:`: `guard || anything` is a guard
the other side can walk around.

THE OPPOSITE OF EVERY OTHER REPOSITORY GUARD HERE
-------------------------------------------------
`test_repo_slug_is_publishable.py` requires the guards in ci.yml to admit BOTH
repositories, because there a guard that skips is a check that silently stopped
running. This guard exists to skip in one of them. It reuses that file's matcher, so a
guard spelled in a way either file cannot see is a guard neither checks.
"""

import glob
import os
import re

import pytest

from test_published_image_destination import _doc, _job_pushes_an_image  # noqa: E402
from test_repo_slug_is_publishable import (  # noqa: E402
    PRIVATE_SLUG,
    PUBLIC_SLUG,
    _allowed_repositories,
)

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

# Declared with os.path.join(REPO_ROOT, <constants>) so
# test_ci_filter_covers_every_guard_subject.py can read this guard's subjects.
_WORKFLOW_GLOBS = (
    os.path.join(REPO_ROOT, ".github", "workflows", "*.yml"),
    os.path.join(REPO_ROOT, ".github", "workflows", "*.yaml"),
)
_PUBLISH_WORKFLOW = os.path.join(REPO_ROOT, ".github", "workflows", "docker-publish.yml")

# Measured 2026-09-28: build-and-push, build-and-push-connectors and publish-chart. A
# detector that finds fewer has stopped working, and would pass on the empty set.
_MIN_WRITER_JOBS = 3

_GUARD = re.compile(r"github\.repository\s*==\s*'([^']+)'")


def _workflows():
    paths = sorted(p for pattern in _WORKFLOW_GLOBS for p in glob.glob(pattern))
    assert paths, f"no workflow files match {_WORKFLOW_GLOBS}"
    return paths


def _grants_package_write(permissions):
    if permissions == "write-all":
        return True
    return isinstance(permissions, dict) and permissions.get("packages") == "write"


def _job_writes_to_the_registry(job, workflow_permissions):
    if _grants_package_write(job.get("permissions", workflow_permissions)):
        return True
    if _job_pushes_an_image(job):
        return True
    return any(
        isinstance(step, dict) and re.search(r"\bhelm\s+push\b", step.get("run") or "")
        for step in job.get("steps") or []
    )


def _top_level(expr, op):
    """Split `expr` on `op` where it sits outside parentheses and quotes."""
    parts, depth, quote, start, i = [], 0, None, 0, 0
    while i < len(expr):
        c = expr[i]
        if quote:
            quote = None if c == quote else quote
        elif c in "'\"":
            quote = c
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        elif depth == 0 and expr.startswith(op, i):
            parts.append(expr[start:i])
            i += len(op)
            start = i
            continue
        i += 1
    parts.append(expr[start:])
    return [p.strip() for p in parts]


def _guard_problem(condition):
    """Why this `if:` does not confine the job to the public repository, or None."""
    if condition is None:
        return "has no `if:` at all"
    expr = str(condition).strip()
    wrapped = re.fullmatch(r"\$\{\{(.*)\}\}", expr, re.S)
    if wrapped:
        expr = wrapped.group(1).strip()
    if len(_top_level(expr, "||")) > 1:
        return f"has a top-level `||`, so the other side can bypass the guard: {expr!r}"
    guards = [c for c in _top_level(expr, "&&") if _GUARD.fullmatch(c)]
    if not guards:
        return f"has no top-level `github.repository == '{PUBLIC_SLUG}'` conjunct: {expr!r}"
    admitted = _allowed_repositories(expr)
    if admitted != [{PUBLIC_SLUG}]:
        return f"admits {[sorted(s) for s in admitted]}, not exactly {PUBLIC_SLUG}: {expr!r}"
    return None


def _publishing_workflows():
    """{path: {job name: job}} for every workflow with at least one registry writer."""
    found = {}
    for path in _workflows():
        doc = _doc(path) or {}
        jobs = doc.get("jobs") or {}
        wf_permissions = doc.get("permissions")
        if any(isinstance(j, dict) and _job_writes_to_the_registry(j, wf_permissions) for j in jobs.values()):
            found[path] = jobs
    return found


# --------------------------------------------------------------------------------------
# controls
# --------------------------------------------------------------------------------------


def test_the_census_finds_the_registry_writers():
    publishing = _publishing_workflows()
    assert _PUBLISH_WORKFLOW in publishing, (
        "docker-publish.yml was not detected as writing to the registry. Either the "
        "detector stopped matching its jobs or the workflow moved; retarget this file "
        "rather than let the census below pass over nothing."
    )
    doc = _doc(_PUBLISH_WORKFLOW)
    writers = [
        name for name, job in doc["jobs"].items()
        if _job_writes_to_the_registry(job, doc.get("permissions"))
    ]
    assert len(writers) >= _MIN_WRITER_JOBS, (
        f"only {writers} were detected as registry writers in docker-publish.yml; "
        f"expected at least {_MIN_WRITER_JOBS}. The detector is degrading toward the "
        f"empty set, on which every assertion below passes."
    )


@pytest.mark.parametrize("condition", [
    f"github.repository == '{PUBLIC_SLUG}'",
    f"${{{{ github.repository == '{PUBLIC_SLUG}' }}}}",
    f"github.repository == '{PUBLIC_SLUG}' && github.ref_type == 'tag'",
    f"github.ref_type == 'tag' && github.repository == '{PUBLIC_SLUG}'",
])
def test_a_confining_guard_is_accepted(condition):
    assert _guard_problem(condition) is None, _guard_problem(condition)


@pytest.mark.parametrize("condition", [
    None,
    "github.ref_type == 'tag'",
    f"github.repository == '{PUBLIC_SLUG}' || github.event_name == 'workflow_dispatch'",
    f"github.repository != '{PUBLIC_SLUG}'",
    f"github.repository == '{PRIVATE_SLUG}'",
    f"github.repository == '{PUBLIC_SLUG}-enterprise'",
    f"contains(fromJSON('[\"{PRIVATE_SLUG}\", \"{PUBLIC_SLUG}\"]'), github.repository)",
    f"github.repository == '{PUBLIC_SLUG}' && contains(fromJSON('[\"{PRIVATE_SLUG}\"]'), github.repository)",
    f"(github.repository == '{PUBLIC_SLUG}' || true)",
])
def test_a_guard_that_does_not_confine_is_refused(condition):
    assert _guard_problem(condition) is not None, (
        f"{condition!r} was accepted, but it lets a job run somewhere other than "
        f"{PUBLIC_SLUG}. The checker is weaker than this file claims."
    )


# --------------------------------------------------------------------------------------
# the assertion
# --------------------------------------------------------------------------------------


def test_every_job_in_a_publishing_workflow_runs_only_in_the_public_repo():
    problems = []
    for path, jobs in _publishing_workflows().items():
        rel = os.path.relpath(path, REPO_ROOT)
        for name, job in jobs.items():
            if not isinstance(job, dict):
                continue
            why = _guard_problem(job.get("if"))
            if why:
                problems.append(f"  {rel} job `{name}` {why}")
    assert not problems, (
        f"These jobs can run outside {PUBLIC_SLUG}, which is the only repository that "
        f"may publish the community images:\n" + "\n".join(problems) + "\n"
        f"Give each one `if: github.repository == '{PUBLIC_SLUG}'`, conjoined with `&&` "
        f"to any condition it already has. A publish from anywhere else overwrites the "
        f"images a self-host install pulls."
    )
