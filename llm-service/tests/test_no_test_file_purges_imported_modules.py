"""
No test file drops imported modules from ``sys.modules`` while it is collected.

pytest imports every test file into one process (each xdist worker collects all
of them). A file that deletes ``src.*`` from ``sys.modules`` at import time
makes every module imported after it a second copy: files collected earlier
keep the first copy's exception classes, contextvars and registries, files
collected later, string ``monkeypatch`` targets and lazy imports get the
second. ``pytest.raises``, ``isinstance`` and patches then miss depending on
test order. ``test_explorer_endpoints.py`` did this for a "foreign ``src``"
that CI never has (PYTHONPATH is llm-service).

A test that loads a module for itself and removes it again afterwards does so
inside a function, which this does not flag.

VERSION: 1.0.0
"""

from __future__ import annotations

import ast
from pathlib import Path

_LLM = Path(__file__).resolve().parents[1]
_ROOTS = (_LLM / "tests", _LLM / "src", _LLM.parent / "shared" / "mcp-connectors")


def _test_files():
    for root in _ROOTS:
        for path in root.rglob("test_*.py"):
            if ".venv" not in path.parts and "node_modules" not in path.parts:
                yield path


def _import_time_nodes(body):
    """Statements that run when the file is imported: not inside a def or class."""
    for node in body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            continue
        yield node
        for field in ("body", "orelse", "finalbody", "handlers"):
            inner = getattr(node, field, None)
            if isinstance(inner, list):
                yield from _import_time_nodes(inner)


def _is_sys_modules(node) -> bool:
    return (isinstance(node, ast.Attribute) and node.attr == "modules"
            and isinstance(node.value, ast.Name) and node.value.id == "sys")


def _purges(tree: ast.Module):
    """Line numbers of import-time ``del sys.modules[...]`` / ``sys.modules.pop/clear``."""
    lines = []
    for stmt in _import_time_nodes(tree.body):
        if isinstance(stmt, ast.Delete):
            if any(isinstance(t, ast.Subscript) and _is_sys_modules(t.value) for t in stmt.targets):
                lines.append(stmt.lineno)
        elif isinstance(stmt, ast.Expr) and isinstance(stmt.value, ast.Call):
            func = stmt.value.func
            if isinstance(func, ast.Attribute) and func.attr in ("pop", "clear") and _is_sys_modules(func.value):
                lines.append(stmt.lineno)
    return lines


def test_no_test_file_drops_modules_from_sys_modules_at_import_time():
    files = list(_test_files())
    assert len(files) > 300, f"scanned only {len(files)} test files: the walk is broken"
    offenders = []
    for path in files:
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        except SyntaxError:
            continue
        offenders += [f"{path.relative_to(_LLM.parent)}:{n}" for n in _purges(tree)]
    assert not offenders, (
        "these test files drop modules from sys.modules while pytest collects them, "
        "so tests collected before and after them see two copies of the same module: "
        + ", ".join(offenders)
    )


def test_the_scan_sees_an_import_time_purge_and_not_a_function_cleanup():
    flagged = ast.parse(
        "import sys\n"
        "for k in list(sys.modules):\n"
        "    if k.startswith('src.'):\n"
        "        del sys.modules[k]\n"
        "sys.modules.pop('x', None)\n"
    )
    assert _purges(flagged) == [4, 5]
    cleanup = ast.parse(
        "import sys\n"
        "def _load():\n"
        "    try:\n"
        "        pass\n"
        "    finally:\n"
        "        del sys.modules['added']\n"
    )
    assert _purges(cleanup) == []
