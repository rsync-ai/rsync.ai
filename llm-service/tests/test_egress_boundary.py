"""No LLM provider call anywhere in llm-service except inside src/egress.

The guard (src/egress/guard.py) is what makes "metadata only" true: it renders
each prompt variable by its field class, scrubs secrets, caps the size and
refuses parameters that could carry text or a destination past it. A provider
call that does not go through it gets none of that. Before the guard, the
service had over a dozen `*.completions.create` call sites, each building its
own messages, and three of them sent row counts, enum labels or raw error text.

Two censuses, each armed with a control so neither can pass on nothing:

1. No provider SDK call outside src/egress.
2. Every guarded call names a component in the census (call_sites.py), and
   every component in the census has a caller -- so the census, and the
   generated docs/security/llm-egress.md, describe the code that runs.
"""

from __future__ import annotations

import ast
from pathlib import Path

import pytest

from _cut_collection import removed_by_the_cut
from src.egress.call_sites import CALL_SITES

SRC = Path(__file__).resolve().parents[1] / "src"
EGRESS = SRC / "egress"

# The last two attributes of a provider SDK request: `client.chat.completions.create`,
# `client.completions.create`, Anthropic's `client.messages.create`, OpenAI's
# `client.responses.create` and `client.beta.chat.completions.parse`.
PROVIDER_CALLS = frozenset(
    {
        ("completions", "create"),
        ("completions", "parse"),
        ("completions", "stream"),
        ("messages", "create"),
        ("messages", "stream"),
        ("responses", "create"),
        ("responses", "stream"),
        ("models", "generate_content"),
    }
)

# Endpoint paths of the providers' HTTP APIs. A string holding one, outside
# src/egress, is a request built by hand around the SDK.
PROVIDER_PATHS = ("/chat/completions", "/v1/completions", "/v1/messages", "/api/generate", "/api/chat")

GUARDED = {"guarded_call", "guarded_acall"}
# A helper whose first positional argument is the component.
GUARDED_HELPERS = {"_guarded_explorer_call"}


def _attr_chain(node: ast.AST) -> list[str]:
    names = []
    while isinstance(node, ast.Attribute):
        names.append(node.attr)
        node = node.value
    return list(reversed(names))


def _docstring_nodes(tree: ast.AST) -> set[int]:
    ids = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant):
            ids.add(id(node.value))
    return ids


def provider_calls(tree: ast.AST) -> list[int]:
    """Lines of every provider SDK call or hand-built provider request."""
    docstrings = _docstring_nodes(tree)
    hits = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            chain = _attr_chain(node.func)
            if len(chain) >= 2 and tuple(chain[-2:]) in PROVIDER_CALLS:
                hits.append(node.lineno)
        elif isinstance(node, ast.Constant) and isinstance(node.value, str) and id(node) not in docstrings:
            if any(p in node.value for p in PROVIDER_PATHS):
                hits.append(node.lineno)
    return sorted(set(hits))


def _source_files():
    for path in sorted(SRC.rglob("*.py")):
        rel = path.relative_to(SRC)
        if rel.parts[0] == "egress" or "tests" in rel.parts:
            continue
        yield path, rel.as_posix()


def test_the_census_sees_every_shape_a_provider_call_has_taken():
    tree = ast.parse(
        "async def f(client, explorer_client, self):\n"
        "    a = await client.chat.completions.create(model='m', messages=[])\n"
        "    b = await explorer_client.completions.create(model='m', prompt='p')\n"
        "    c = self.client.chat.completions.create(model='m', messages=[])\n"
        "    d = client.messages.create(model='m', messages=[])\n"
        "    e = client.beta.chat.completions.parse(model='m', messages=[])\n"
        "    f = httpx.post(base + '/v1/chat/completions', json={})\n"
        "    g = client.resource.create(name='x')\n"
        "    '''a docstring may say /chat/completions'''\n"
    )
    assert provider_calls(tree) == [2, 3, 4, 5, 6, 7]


def test_the_census_is_live_on_the_guard_itself():
    """The guard is the one place provider calls live. If the walk found none
    there, it would find none anywhere, and the test below would pass on nothing."""
    hits = provider_calls(ast.parse((EGRESS / "guard.py").read_text()))
    assert len(hits) >= 6, hits


def test_no_provider_call_outside_the_egress_guard():
    offenders = []
    for path, rel in _source_files():
        offenders += [f"{rel}:{ln}" for ln in provider_calls(ast.parse(path.read_text()))]
    assert not offenders, (
        f"LLM provider call(s) outside src/egress at {offenders}. Route them through "
        "src.egress.guarded_call / guarded_acall with a component from "
        "src/egress/call_sites.py: a direct call skips the field projection, the "
        "secret scrub, the size cap and the parameter allowlist."
    )


def _components_called(tree: ast.AST) -> list[tuple[int, object]]:
    # Inside a helper, the guarded call forwards the helper's own parameter;
    # the helper's callers are the sites.
    helper_lines = set()
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name in GUARDED_HELPERS:
            helper_lines |= set(range(node.lineno, (node.end_lineno or node.lineno) + 1))
    found = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call) or node.lineno in helper_lines:
            continue
        name = node.func.id if isinstance(node.func, ast.Name) else getattr(node.func, "attr", None)
        if name in GUARDED:
            for kw in node.keywords:
                if kw.arg == "component":
                    found.append((node.lineno, kw.value.value if isinstance(kw.value, ast.Constant) else None))
        elif name in GUARDED_HELPERS and node.args:
            first = node.args[0]
            found.append((node.lineno, first.value if isinstance(first, ast.Constant) else None))
    return found


def test_every_guarded_call_names_a_census_component_and_every_component_is_called():
    called: dict[str, list[str]] = {}
    not_literal = []
    for path, rel in _source_files():
        for lineno, component in _components_called(ast.parse(path.read_text())):
            if not isinstance(component, str):
                not_literal.append(f"{rel}:{lineno}")
                continue
            called.setdefault(component, []).append(f"{rel}:{lineno}")

    assert sum(len(v) for v in called.values()) >= 15, called
    assert not not_literal, (
        f"guarded call(s) at {not_literal} pass a computed component; name it literally "
        "so this census and the generated egress doc can see it"
    )
    unknown = sorted(set(called) - set(CALL_SITES))
    assert not unknown, f"components with no census entry (the guard refuses them): {unknown}"
    # The public cut strips the tool generator's agents, and with them its one call.
    uncalled = sorted(
        c for c in set(CALL_SITES) - set(called) if not removed_by_the_cut(CALL_SITES[c].where.split(":")[0])
    )
    assert not uncalled, (
        f"census entries nothing calls: {uncalled}. Delete them, or the published "
        "egress doc describes a path that does not exist."
    )


@pytest.mark.parametrize("component", sorted(CALL_SITES))
def test_a_messages_path_component_has_no_prompts_and_a_prompt_path_one_has_some(component):
    site = CALL_SITES[component]
    assert site.path in ("prompt", "messages", "tools")
    if site.path == "messages":
        assert site.prompts == ()
    else:
        assert site.prompts
