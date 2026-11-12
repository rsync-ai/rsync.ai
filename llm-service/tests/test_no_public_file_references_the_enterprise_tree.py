"""No file that survives the public cut may depend on the paid code.

This is the census guard of the boundary in §3.3 of
`docs/internal/public-vs-cloud-feature-split.md`. The paid code lives in its own private
repository, `rsync-ai/rsync-enterprise`, and ships as its own service and image. Until
2026-09-28 it had an in-tree home here instead, excluded from the cut; that directory is
retired, and this guard now watches both halves of the move.

WHAT THE BOUNDARY ACTUALLY HAS TO STOP
--------------------------------------
Not a leaked file -- the paid code is not in this repository to leak. The failure that
ends the boundary is a *public file that needs the private code*: a Go module import, a
`replace` into a sibling checkout, a build context of `../rsync-enterprise`, a
submodule, a pip or npm dependency on it. Any of those makes the public build either fail
for everyone without access, or quietly pull paid code into a community image -- which is
option A2, rejected in §3.0 because the core must never link the paid code as a library.
So the subject is dependency-shaped references, not mentions.

WHY THE DETECTOR IS REFERENCE-SHAPED, NEVER THE BARE NAME
---------------------------------------------------------
The name is legitimately everywhere a reader needs it. `rsync-enterprise` is the reserved
IMAGE name that `test_enterprise_images_cannot_overwrite_a_public_one.py` keeps off every
public surface, docs name the repository in prose, and a link to it is a link, not a
dependency. The bare word `enterprise` is ordinary English and a live data value -- the
top plan tier, `free | pro | enterprise` in `047_workspaces.sql`. A guard matching either
would be red on arrival for correct code, and red-on-arrival guards get relaxed until
they protect nothing while still printing green. The patterns below match only shapes
that mean "this file needs that code to build or run".

AND THE DIRECTORY MUST STAY RETIRED
-----------------------------------
An in-tree paid directory ships to the public repository unless an exclude entry stops it,
and an exclude entry is a promise about a cut, not a check on a pull request. So the last
test asserts nothing is committed under the old path or under a nested clone of the new
repository, in either tree.

WHY THIS GUARD RUNS IN BOTH TREES AND SKIPS IN NEITHER
------------------------------------------------------
Its subject is the surviving tree, which is exactly what the public repository is. In the
private tree it asks "does anything that WILL ship depend on the paid code?", in the
public tree "did anything that DID ship?". `scripts/flip/assert-public-suite.sh`, which
materialises the cut and re-runs this directory's guards inside it, runs this file in
both. Only the surviving set differs: `excludes.txt` is itself cut, so in the public tree
there is no list to subtract and "surviving" is simply "tracked".

ANTI-VACUITY
------------
  * a floor on the census denominator (`_MIN_SURVIVING`), well under the count measured;
  * a positive control per pattern, so a regex that stops matching is caught by the
    detector failing to fire on a reference it is supposed to catch;
  * a negative control of the legitimate mentions above, so the detector cannot be
    "fixed" into matching the image name, a prose link or the plan tier.
"""

import os
import re
import subprocess

import pytest

_TESTS = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(_TESTS))

# Declared this way on purpose: test_ci_filter_covers_every_guard_subject.py derives a
# guard's subjects by AST-walking for os.path.join(REPO_ROOT, <string constants>).
# Built any other way this guard declares no subject and the census skips it.
_EXCLUDES = os.path.join(REPO_ROOT, "scripts", "flip", "excludes.txt")

# The paid repository, as a repository name, a Python package and a module path. One
# place, so the patterns, the controls and the failure messages cannot drift apart.
_OWNER = "rsync-ai"
_REPO = "rsync-enterprise"
_PY_PACKAGE = "rsync_enterprise"

# Paths that must never be tracked here: the retired in-tree directory, and a clone of the
# paid repository nested in this checkout.
_RETIRED_PATHS = ("enterprise", _REPO)

# Measured 2026-09-22: 2985 tracked, 2895 surviving the cut. The floor sits far below
# that so ordinary growth or pruning never trips it, and far above zero so a census that
# stopped enumerating cannot report a clean sweep.
_MIN_SURVIVING = 1000

# Files this guard may not scan for itself, because they carry the patterns as data.
# Computed rather than hardcoded so a rename cannot turn the exclusion into a miss.
_SELF = os.path.relpath(os.path.abspath(__file__), REPO_ROOT)


def _rx(pattern: str) -> "re.Pattern[str]":
    return re.compile(
        pattern.replace("OWNER", re.escape(_OWNER))
        .replace("PYPKG", re.escape(_PY_PACKAGE))
        .replace("REPO", re.escape(_REPO))
    )


# (name, regex, what a hit means). Every one of these is a shape that only appears when a
# file needs the paid code -- never a shape the name takes in prose, a link or an image.
_PATTERNS = (
    (
        "sibling-path",
        _rx(r"(?<![A-Za-z0-9_.\-/])(?:\.\./)+REPO(?![A-Za-z0-9_\-])"),
        "a relative path into a sibling checkout of the paid repository",
    ),
    (
        "go-import",
        _rx(r'"github\.com/OWNER/REPO(?:/[A-Za-z0-9_./\-]*)?"'),
        "a Go import of the paid module",
    ),
    (
        "go-mod",
        _rx(r"(?m)^[ \t]*(?:(?:require|replace)[ \t]+)?github\.com/OWNER/REPO(?:/\S*)?[ \t]+(?:v\d|=>)"),
        "a go.mod require or replace of the paid module",
    ),
    (
        "py-import",
        _rx(r"(?m)^[ \t]*(?:from|import)[ \t]+PYPKG\b"),
        "a Python import of the paid package",
    ),
    (
        "js-import",
        _rx(r"""(?:from|import|require\()\s*['"](?:@OWNER/)?REPO(?:/[^'"]*)?['"]"""),
        "a TypeScript/JavaScript import of the paid package",
    ),
    (
        "npm-dependency",
        _rx(r"""(?m)^[ \t]*"(?:@OWNER/)?REPO"[ \t]*:[ \t]*\""""),
        "a package.json dependency on the paid package",
    ),
    (
        "git-dependency",
        _rx(r"(?:git\+(?:https?|ssh)://\S*|git@github\.com:)OWNER/REPO\b"),
        "a git URL used as a dependency (pip `git+`, npm, ssh remote)",
    ),
    (
        "submodule-or-clone",
        _rx(r"(?m)(?:^[ \t]*url[ \t]*=[ \t]*\S*|\bgit[ \t]+(?:clone|submodule[ \t]+add)\b[^\n]*?)OWNER/REPO\b"),
        "a submodule or a scripted clone of the paid repository",
    ),
)


def _scan(text: str) -> list[tuple[str, int, str]]:
    """Every reference-shaped hit in one file, as (pattern name, 1-based line, line)."""
    found = []
    for name, rx, _why in _PATTERNS:
        for m in rx.finditer(text):
            line_no = text.count("\n", 0, m.start()) + 1
            line = text.split("\n")[line_no - 1].strip()
            found.append((name, line_no, line[:160]))
    return found


def _git(*args: str) -> str:
    proc = subprocess.run(
        ["git", *args], cwd=REPO_ROOT, capture_output=True, text=True, check=False
    )
    assert proc.returncode == 0, f"git {' '.join(args)} failed: {proc.stderr.strip()}"
    return proc.stdout


def _exclude_entries() -> list[str]:
    """`excludes.txt`, parsed the way the cut itself parses it.

    `sed 's/#.*//' | tr -d '[:blank:]' | grep -v '^$'`, mirrored rather than tidied: a
    guard that reads the list differently from the script that applies it can pass on
    entries the cut never sees.
    """
    out = []
    with open(_EXCLUDES, encoding="utf-8") as fh:
        for raw in fh:
            line = "".join(raw.split("#", 1)[0].split())
            if line:
                out.append(line)
    return out


def _surviving_files() -> list[str]:
    """Every tracked file that is still there after the cut.

    In a pre-cut tree that is `git ls-files` minus the cut manifest, built with the same
    three pathspec forms `materialise-cut.sh` expands each entry into. In the public tree
    `excludes.txt` is gone -- it excludes itself -- and there is nothing to subtract,
    which is correct: everything tracked there has already survived.
    """
    tracked = [p for p in _git("ls-files").split("\n") if p]
    if not os.path.isfile(_EXCLUDES):
        return tracked
    pathspecs = []
    for entry in _exclude_entries():
        pathspecs += [entry, f":(glob)**/{entry}", f":(glob)**/{entry}/**"]
    cut = {p for p in _git("ls-files", "--", *pathspecs).split("\n") if p}
    return [p for p in tracked if p not in cut]


def _read(rel: str) -> str | None:
    """File text, or None for anything that is not text."""
    try:
        with open(os.path.join(REPO_ROOT, rel), "rb") as fh:
            blob = fh.read()
    except (IsADirectoryError, FileNotFoundError, PermissionError):
        return None
    if b"\0" in blob[:8192]:
        return None
    return blob.decode("utf-8", errors="replace")


# --------------------------------------------------------------------------------------
# controls: the detector has to be able to fail, and to fail for the right reason
# --------------------------------------------------------------------------------------

_MUST_FIRE = [
    ("sibling-path", "    context: ../rsync-enterprise"),
    ("sibling-path", "      enterprise: ../../rsync-enterprise/identity"),
    ("sibling-path", "COPY ../rsync-enterprise/bin /app/bin"),
    ("sibling-path", "replace example.com/x => ../rsync-enterprise"),
    ("go-import", '\t"github.com/rsync-ai/rsync-enterprise/identity"'),
    ("go-mod", "require github.com/rsync-ai/rsync-enterprise v0.1.0"),
    ("go-mod", "\tgithub.com/rsync-ai/rsync-enterprise v0.1.0 // indirect"),
    ("go-mod", "replace github.com/rsync-ai/rsync-enterprise => ../rsync-enterprise"),
    ("py-import", "from rsync_enterprise.identity import assert_sso"),
    ("py-import", "import rsync_enterprise"),
    ("js-import", 'import { Sso } from "@rsync-ai/rsync-enterprise/sso"'),
    ("js-import", "const x = require('rsync-enterprise')"),
    ("npm-dependency", '    "@rsync-ai/rsync-enterprise": "^0.1.0",'),
    ("git-dependency", "rsync-enterprise @ git+https://github.com/rsync-ai/rsync-enterprise.git@v0.1.0"),
    ("git-dependency", "git+ssh://git@github.com/rsync-ai/rsync-enterprise.git"),
    ("submodule-or-clone", "\turl = https://github.com/rsync-ai/rsync-enterprise.git"),
    ("submodule-or-clone", "git clone --depth 1 https://github.com/rsync-ai/rsync-enterprise.git"),
]

# Mentions that are correct and must stay possible: the reserved image name, the
# repository named in prose or linked, and the bare word as English and as the plan tier.
_MUST_NOT_FIRE = [
    '_PAID_SERVICE = "rsync-enterprise"',
    "    image: ghcr.io/rsync-ai/rsync-enterprise:enterprise-v0.1.0",
    "# `rsync-enterprise` image from its own repository.",
    "The paid code lives in the private `rsync-ai/rsync-enterprise` repository.",
    "See [the paid repo](https://github.com/rsync-ai/rsync-enterprise) for access.",
    "    plan        VARCHAR(50) DEFAULT 'free',      -- free | pro | enterprise",
    '{"customer_id": 3, "country": "IT", "plan": "enterprise"}',
    '    reason: "Enterprise CRM with comprehensive bulk data import APIs"',
    "## Enterprise layer (option A1: separate private service, no runtime gate)",
    "an enterprise buyer pays for governance, not for a checkbox",
]


@pytest.mark.parametrize(
    ("expected", "line"),
    [pytest.param(k, s, id=f"{k}:{s.strip()[:40]}") for k, s in _MUST_FIRE],
)
def test_the_detector_fires_on_every_reference_shape_it_claims_to_catch(expected, line):
    """The positive control. Without it, a dead regex reads exactly like a clean tree."""
    names = {name for name, _no, _text in _scan(line)}
    assert expected in names, (
        f"the {expected!r} pattern did not fire on a reference it exists to catch:\n"
        f"  {line}\n\nThe census below would report a clean sweep over a tree this "
        "detector can no longer see into."
    )


@pytest.mark.parametrize("line", _MUST_NOT_FIRE, ids=lambda s: s.strip()[:40])
def test_the_detector_ignores_mentions_that_are_not_dependencies(line):
    """The negative control: the image name, a prose or linked repo name, the word.

    A detector widened to the name goes red on the image-name guard, on the docs that
    tell a reader where the paid code lives and on the plan tier -- and the repair anyone
    reaches for is to weaken it until it passes, at which point it protects nothing.
    """
    hits = _scan(line)
    assert not hits, (
        f"the detector fired on a line that is not a dependency:\n  {line}\n"
        f"  matched: {hits}\n\nNarrow the pattern rather than exempting the file."
    )


# --------------------------------------------------------------------------------------
# the census
# --------------------------------------------------------------------------------------


def test_the_census_enumerates_a_real_tree():
    """The anti-vacuity floor. An empty census passes the sweep below for free."""
    surviving = _surviving_files()
    assert len(surviving) >= _MIN_SURVIVING, (
        f"the surviving census found {len(surviving)} files, expected at least "
        f"{_MIN_SURVIVING} (2895 measured 2026-09-22). Either `git ls-files` returned "
        "nothing or the exclude subtraction is removing the whole tree; either way the "
        "sweep below is scanning nothing and would pass on any reference at all."
    )


def test_this_guard_scans_everything_except_itself():
    """Self-exclusion has to be exactly one file, and that file has to be in the census.

    This guard carries the patterns as data, so it must skip itself. Two things can go
    wrong quietly: the relative path stops matching after a move, in which case the guard
    fails on its own controls; or the guard is itself excluded from the cut, in which
    case it never runs in the public tree and its second, load-bearing assertion is gone.
    """
    surviving = _surviving_files()
    assert _SELF in surviving, (
        f"{_SELF} is not in the surviving set. This guard has to ship to the public "
        "repo -- there it is the assertion that no dependency survived the cut. Do not "
        "add it to excludes.txt."
    )
    assert _scan(_read(_SELF) or ""), (
        f"{_SELF} no longer matches its own patterns, which means the detector is not "
        "seeing the reference shapes written into this file as controls."
    )


def test_no_surviving_file_depends_on_the_paid_code():
    """The assertion. Every tracked file that ships, scanned for a dependency.

    A hit here is a public file that cannot build without access to a private
    repository, or that would link paid code into a community artifact.
    """
    offenders = []
    scanned = 0
    for rel in _surviving_files():
        if rel == _SELF:
            continue
        text = _read(rel)
        if text is None:
            continue
        scanned += 1
        for name, line_no, line in _scan(text):
            offenders.append(f"{rel}:{line_no}: [{name}] {line}")

    assert scanned >= _MIN_SURVIVING, (
        f"only {scanned} of the surviving files were readable as text; expected at "
        f"least {_MIN_SURVIVING}. The sweep did not cover the tree it claims to."
    )
    assert not offenders, (
        f"{len(offenders)} file(s) that survive the public cut depend on "
        f"{_OWNER}/{_REPO}:\n  "
        + "\n  ".join(offenders)
        + "\n\nPublic code must not reach the paid code -- not by import, not by module "
        "replace, not by build context, not as a dependency. Call a generic integration "
        "point with a working community default instead; the paid service fills it over "
        "the network. See §3.0 and §3.3 of docs/internal/public-vs-cloud-feature-split.md."
    )


def test_the_paid_code_is_not_committed_to_this_repository():
    """The retired in-tree directory stays retired, and no clone of the paid repo lands here.

    Tracked here, it ships to the public repository unless an exclude entry catches it --
    and nothing re-runs the cut on a pull request to check that one does.
    """
    tracked = [p for p in _git("ls-files", "--", *_RETIRED_PATHS).split("\n") if p]
    assert not tracked, (
        f"{len(tracked)} file(s) are tracked under {list(_RETIRED_PATHS)}: {tracked[:5]}\n"
        f"Paid code belongs in the private {_OWNER}/{_REPO} repository, which publishes "
        "its own image. It does not come back in-tree."
    )
