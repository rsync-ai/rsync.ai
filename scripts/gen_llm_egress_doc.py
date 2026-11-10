#!/usr/bin/env python3
"""Render docs/security/llm-egress.md: what rsync.ai sends to an LLM.

The page is generated from the code that enforces it, so it cannot describe a
check that is not there:

- the call sites and the refused prompts: llm-service/src/egress/call_sites.py
- the field classes and the text mask: the docstring of
  llm-service/src/egress/fields.py, checked against its FIELD_CLASSES tuple
- the fixed checks every request passes: the docstring of
  llm-service/src/egress/stages.py
- sample rows, the one row data a model may get: the docstring and
  PRESIDIO_ENTITIES of llm-service/src/egress/sample_rows.py
- the variables each prompt sends and their classes: ``llm_fields`` in
  llm-service/prompts/**/*.yaml (tests/test_egress_prompts.py holds each map
  to exactly the variables the template reads)
- the Go files allowed to name a provider: ``egressExceptions`` in
  backend-orchestrator/pkg/llmscrub/egress_boundary_test.go

Stdlib and PyYAML only, so the doc-guards job runs it without llm-service's
dependencies. llm-service/tests/test_llm_egress_doc_is_current.py regenerates
the page and fails when the committed copy differs.

Usage:
    python3 scripts/gen_llm_egress_doc.py          # write the page
    python3 scripts/gen_llm_egress_doc.py --check  # exit 1 if it is stale
"""

from __future__ import annotations

import ast
import importlib.util
import re
import sys
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parents[1]
EGRESS = REPO / "llm-service" / "src" / "egress"
PROMPTS = REPO / "llm-service" / "prompts"
GO_BOUNDARY = REPO / "backend-orchestrator" / "pkg" / "llmscrub" / "egress_boundary_test.go"
DOC = REPO / "docs" / "security" / "llm-egress.md"

# Links in the page are relative to docs/security/.
UP = "../../"


def _load_call_sites():
    """Import call_sites.py by path: it is stdlib-only, the package is not."""
    spec = importlib.util.spec_from_file_location("_egress_call_sites", EGRESS / "call_sites.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod  # dataclasses resolve annotations through sys.modules
    spec.loader.exec_module(mod)
    return mod


def _rst_to_md(text: str) -> str:
    text = re.sub(r":\w+:`~?\.?([^`]+)`", r"`\1`", text)
    return text.replace("``", "`")


def _paragraphs(lines: list[str]) -> list[str]:
    out, cur = [], []
    for line in lines:
        if line.strip():
            cur.append(line.strip())
        elif cur:
            out.append(" ".join(cur))
            cur = []
    if cur:
        out.append(" ".join(cur))
    return [_rst_to_md(p) for p in out]


def _docstring(path: Path) -> str:
    doc = ast.get_docstring(ast.parse(path.read_text()))
    if not doc:
        raise SystemExit(f"{path} has no module docstring to render")
    return doc


def field_classes() -> tuple[list[tuple[str, str]], list[str]]:
    """``[(class, description)]`` and the text-mask paragraphs, from fields.py."""
    src = (EGRESS / "fields.py").read_text()
    declared = None
    for node in ast.parse(src).body:
        if isinstance(node, ast.Assign) and any(
            isinstance(t, ast.Name) and t.id == "FIELD_CLASSES" for t in node.targets
        ):
            declared = list(ast.literal_eval(node.value))
    if not declared:
        raise SystemExit("fields.py: FIELD_CLASSES not found")

    lines = _docstring(EGRESS / "fields.py").splitlines()
    try:
        start = next(i for i, l in enumerate(lines) if l.startswith("The classes, and what each lets through"))
        mask = next(i for i, l in enumerate(lines) if l.startswith("The text mask"))
    except StopIteration:
        raise SystemExit("fields.py docstring lost its class list or its text-mask section")

    classes: list[tuple[str, list[str]]] = []
    for line in lines[start + 1 : mask]:
        m = re.fullmatch(r"``(\w+)``", line.strip())
        if m and not line.startswith(" "):
            classes.append((m.group(1), []))
        elif classes:
            classes[-1][1].append(line)
    described = [(name, " ".join(_paragraphs(body))) for name, body in classes]

    if [n for n, _ in described] != declared:
        raise SystemExit(
            "fields.py: the docstring describes "
            f"{[n for n, _ in described]} but FIELD_CLASSES is {declared}. Keep them in the same order."
        )
    return described, _paragraphs(lines[mask:])


def fixed_checks() -> tuple[str, list[str], list[str]]:
    """The lead sentence, the numbered checks and the closing text of stages.py."""
    lines = _docstring(EGRESS / "stages.py").splitlines()
    lead = _rst_to_md(lines[0])
    steps: list[list[str]] = []
    rest: list[str] = []
    for line in lines[1:]:
        m = re.match(r"(\d+)\. (.*)", line)
        if m and not rest:
            steps.append([m.group(2)])
        elif steps and line.startswith("   ") and not rest:
            steps[-1].append(line.strip())
        elif steps and line.strip():
            rest.append(line)
        elif rest:
            rest.append(line)
    if len(steps) < 3:
        raise SystemExit(f"stages.py docstring: found {len(steps)} numbered checks; the parser is stale")
    return lead, [_rst_to_md(" ".join(s)) for s in steps], _paragraphs(rest)


def _blocks(lines: list[str]) -> list[str]:
    """Paragraphs, with a ``- `` list kept as a list."""
    out: list[str] = []
    cur: list[str] = []
    bullets: list[list[str]] = []

    def flush():
        nonlocal cur, bullets
        if cur:
            out.append(_rst_to_md(" ".join(cur)))
        if bullets:
            out.append("\n".join(_rst_to_md("- " + " ".join(b)) for b in bullets))
        cur, bullets = [], []

    for line in lines:
        s = line.strip()
        if not s:
            flush()
        elif s.startswith("- "):
            if cur:
                out.append(_rst_to_md(" ".join(cur)))
                cur = []
            bullets.append([s[2:]])
        elif bullets:
            bullets[-1].append(s)
        else:
            cur.append(s)
    flush()
    return out


def sample_rows() -> tuple[list[str], list[str]]:
    """The sample-rows paragraphs and the entities Presidio looks for."""
    path = EGRESS / "sample_rows.py"
    entities = None
    for node in ast.parse(path.read_text()).body:
        if isinstance(node, ast.Assign) and any(
            isinstance(t, ast.Name) and t.id == "PRESIDIO_ENTITIES" for t in node.targets
        ):
            entities = list(ast.literal_eval(node.value))
    if not entities:
        raise SystemExit("sample_rows.py: PRESIDIO_ENTITIES not found")
    # The first line is the module's title; the page has its own heading.
    return _blocks(_docstring(path).splitlines()[1:]), entities


def go_exceptions() -> list[tuple[str, str]]:
    """``[(file, reason)]`` from egressExceptions, the reason being its comment."""
    src = GO_BOUNDARY.read_text()
    m = re.search(r"var egressExceptions = map\[string\]string\{\n(.*?)\n\}", src, re.S)
    if not m:
        raise SystemExit(f"{GO_BOUNDARY.name}: egressExceptions not found")
    out, comment = [], []
    for line in m.group(1).splitlines():
        s = line.strip()
        if s.startswith("//"):
            comment.append(s[2:].strip())
        elif s.startswith('"'):
            path = s.split('"')[1]
            out.append((path, " ".join(comment)))
            comment = []
    return out


def prompt_fields(name: str) -> dict[str, str]:
    data = yaml.safe_load((PROMPTS / f"{name}.yaml").read_text()) or {}
    fields = data.get("llm_fields") or {}
    if not isinstance(fields, dict) or not fields:
        raise SystemExit(f"prompts/{name}.yaml has no llm_fields map")
    return {str(k): str(v) for k, v in fields.items()}


def _cell(text: str) -> str:
    return text.replace("|", "\\|").replace("\n", " ")


def render() -> str:
    sites = _load_call_sites()
    classes, mask = field_classes()
    lead, steps, after_steps = fixed_checks()
    samples, entities = sample_rows()
    exceptions = go_exceptions()

    out: list[str] = []
    w = out.append
    w("# What rsync.ai sends to an LLM")
    w("")
    w(
        "<!-- Generated by scripts/gen_llm_egress_doc.py from llm-service/src/egress/, the prompt "
        "files and the Go boundary test. Do not edit: change the code, then run "
        "`python3 scripts/gen_llm_egress_doc.py`. "
        "llm-service/tests/test_llm_egress_doc_is_current.py fails when this page is stale. -->"
    )
    w("")
    w(
        "rsync.ai uses a language model to plan pipelines, answer chat, write Explorer SQL and "
        "diagnose failures. This page lists every place it calls one and exactly what each call "
        "may carry. It is generated from the code that enforces it."
    )
    w("")
    w(
        "**The short answer:** names, types and settings -- never a value read from your systems, "
        "unless you turn on sample rows. "
        "Table, column and connection names, column types, keys, SQL with every literal taken "
        "out, statuses, error text with secrets, counts, IP addresses and `host:port` masked "
        "(a bare host name is not -- see the text mask below), and what you type in chat. "
        "No row, query result, row count, config value or credential. "
        "The one exception is [sample rows](#sample-rows), off by default, and set off "
        "in the self-host compose and Helm chart: while the install allows them, a platform admin "
        "has approved a model and a workspace's owner has turned them on, Ask rsync.ai may send that "
        "model up to 5 rows of a table, with personal and secret columns withheld. "
        "Every provider gets the same prompt, including a model you host yourself."
    )
    w("")
    w("## Where the boundary is")
    w("")
    w(
        f"- **Only llm-service calls a model.** Go code that names a provider endpoint or "
        f"imports a provider SDK fails [the boundary test]({UP}backend-orchestrator/pkg/llmscrub/egress_boundary_test.go)."
    )
    if exceptions:
        w("  The files it allows:")
        for path, reason in exceptions:
            w(f"  - [`{path}`]({UP}{path}): {reason}")
    else:
        w("  It allows no exceptions.")
    w(
        f"- **Inside llm-service, every call goes through one guard** "
        f"([`src/egress/guard.py`]({UP}llm-service/src/egress/guard.py)). It refuses a caller "
        "that is not in the table below, and a prompt that caller does not list."
    )
    w(
        "- **A prompt is rendered by the guard, field by field.** Every variable a prompt reads "
        "carries a field class in the prompt file's `llm_fields`. A variable with no class, or a "
        "value that does not fit its class, refuses the call before anything is sent. A variable "
        "the caller passes but the prompt does not read is dropped."
    )
    w(
        "- **A caller that builds its own messages** is allowed only when its input is not about "
        "your systems: rsync.ai's connector registry, public API documentation, a connector name you typed."
    )
    w(
        "- **A tool-using caller sends both.** The guard renders its prompt as the only system "
        "message, then appends the conversation: your typed question, the model's own turns, and "
        "the results of its tools (the read tools, and connector generation) -- names, ids, types, "
        "statuses and scrubbed error text, "
        "never row values, except a sample_rows result while [sample rows](#sample-rows) are on. "
        "A Gemini tool call goes back with the thought signature Gemini put on it, an opaque token "
        "of Google's that is sent to Gemini only, or Google's documented placeholder on a call "
        "Gemini did not make. "
        "A conversation that carries its own system message is refused."
    )
    w("")
    w("## Field classes")
    w("")
    w(
        f"From [`src/egress/fields.py`]({UP}llm-service/src/egress/fields.py). Each class bounds "
        "what a prompt variable may look like on its way to the model."
    )
    w("")
    w("| Class | What it lets through |")
    w("|---|---|")
    for name, desc in classes:
        w(f"| `{name}` | {_cell(desc)} |")
    w("")
    for p in mask:
        w(p)
        w("")
    w("## The checks every request passes")
    w("")
    w(f"From [`src/egress/stages.py`]({UP}llm-service/src/egress/stages.py). {lead}")
    w("")
    for i, s in enumerate(steps, 1):
        w(f"{i}. {s}")
    w("")
    for p in after_steps:
        w(p)
        w("")
    w("## Sample rows")
    w("")
    w(f"From [`src/egress/sample_rows.py`]({UP}llm-service/src/egress/sample_rows.py).")
    w("")
    for p in samples:
        w(p)
        w("")
    w("Presidio looks for: " + ", ".join(f"`{e}`" for e in entities) + ".")
    w("")
    w("## Every call site")
    w("")
    w(f"From [`src/egress/call_sites.py`]({UP}llm-service/src/egress/call_sites.py).")
    w("")
    w("| Caller | Where | Path | What it sends |")
    w("|---|---|---|---|")
    for site in sorted(sites.CALL_SITES.values(), key=lambda s: s.component):
        path = {"prompt": "guard renders a prompt", "tools": "guard renders a prompt, then a tool conversation"}.get(
            site.path, "own messages"
        )
        w(f"| `{site.component}` | {_cell(site.where)} | {path} | {_cell(site.sends)} |")
    w("")
    w("## Every prompt and what it sends")
    w("")
    w(
        "Each variable a prompt reads, and its field class. Held to exactly the variables the "
        f"template reads by [`tests/test_egress_prompts.py`]({UP}llm-service/tests/test_egress_prompts.py)."
    )
    w("")
    w("| Prompt | Caller | Variables (class) |")
    w("|---|---|---|")
    owner = {p: s.component for s in sites.CALL_SITES.values() for p in s.prompts}
    for name in sorted(owner):
        fields = prompt_fields(name)
        cells = ", ".join(f"`{k}` ({v})" for k, v in sorted(fields.items()))
        w(f"| [`{name}`]({UP}llm-service/prompts/{name}.yaml) | `{owner[name]}` | {_cell(cells)} |")
    w("")
    w("## Prompts the guard refuses")
    w("")
    w(
        "These prompt files have no caller, and the guard will not render them: their variables "
        "are free text of unknown origin. A new prompt file is on a caller above or on this list "
        "before it can ship."
    )
    w("")
    # Not linked: the public tree strips prompts/tool_generator.
    for name in sorted(sites.REFUSED_PROMPTS):
        w(f"- `prompts/{name}.yaml`")
    w("")
    return "\n".join(out)


def main(argv: list[str]) -> int:
    text = render()
    if "--check" in argv:
        if not DOC.exists() or DOC.read_text() != text:
            print(f"{DOC.relative_to(REPO)} is stale: run python3 scripts/gen_llm_egress_doc.py", file=sys.stderr)
            return 1
        return 0
    DOC.parent.mkdir(parents=True, exist_ok=True)
    DOC.write_text(text)
    print(f"wrote {DOC.relative_to(REPO)}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
