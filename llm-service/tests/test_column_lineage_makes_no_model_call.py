"""Column lineage calls no model, so it needs no egress call site.

docs/internal/lineage-catalog-design.md § 3.2: ``POST /v1/lineage/columns`` reads a
model's SQL with sqlglot. It is sent the SQL and the column names of the tables the
model reads, and it is not registered in ``src/egress/call_sites.py`` because nothing
it is sent goes to a model. That stays true only while neither module can reach one:
if either ever imports a model SDK or the egress guard, this test turns red, and the
change has to register a call site with its ``llm_fields`` instead.

Run in a fresh interpreter, because the test session itself has the SDKs loaded.
Armed with a control: the same probe over a module that does load the egress guard
must report it, or a probe that matched nothing would pass forever.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

MODEL_PACKAGES = ("openai", "anthropic", "litellm", "langchain", "google.genai", "google.generativeai", "src.egress")

PROBE = """
import importlib, json, sys
for name in sys.argv[2:]:
    importlib.import_module(name)
watch = json.loads(sys.argv[1])
print(json.dumps(sorted(m for m in sys.modules if any(m == w or m.startswith(w + ".") for w in watch))))
"""


def _loaded_after_importing(*modules: str) -> list[str]:
    out = subprocess.run(
        [sys.executable, "-c", PROBE, json.dumps(MODEL_PACKAGES), *modules],
        cwd=ROOT,
        env={"PYTHONPATH": str(ROOT), "PATH": "/usr/bin:/bin"},
        capture_output=True,
        text=True,
        timeout=120,
        check=True,
    )
    return json.loads(out.stdout.strip().splitlines()[-1])


def test_column_lineage_loads_no_model_sdk_and_no_egress_guard():
    assert _loaded_after_importing("src.lineage.columns", "src.gateway.lineage_routes") == []


def test_control_the_probe_sees_a_module_that_does_load_the_egress_guard():
    assert "src.egress" in _loaded_after_importing("src.gateway.assistant_routes")
