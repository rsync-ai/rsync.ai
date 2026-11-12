"""Every rule on the LLM egress path runs in time linear in its input.

Bug class: a rule whose unanchored leading repetition rescans one long run
from every start position. Python's ``re`` backtracks, so ``(\\w+://)`` over
20 KB of word characters reads the run once from each of its 20 000 starts.
Go's RE2 is linear, so the same pattern is harmless in llmscrub and the bug
shows only here. Error text is scrubbed BEFORE it is truncated (a cut first
could split a credential open past the rule that takes it), so the scrubber
sees whatever length a driver wrote.

Each module-level pattern in the scrubbing modules -- masking, src.egress and
the connectors' log scrubber in base_connector, which mirrors masking -- is timed
on 20 KB runs of every character the rules key on, and of every pair of them. A linear rule
takes milliseconds there; the quadratic ones took 0.4 to 10 seconds.
"""

from __future__ import annotations

import importlib
import importlib.util
import itertools
import os
import pkgutil
import re
import time

import pytest

import src.egress
from src.utils import masking

N = 20_000
BUDGET_S = 0.1
TRIES = 3

_CHARS = list("aA0_.-%+/=:@'\" \\()#?&;,")
UNITS = (
    _CHARS
    + [a + b for a, b in itertools.product(_CHARS, repeat=2)]
    + ["a://", "x@y.", "password", "password=", "eyJa.", "Key (", '"a": ', "1.", "a.b:", "host ", "lookup a.", "dial tcp a."]
)

# Patterns that never see a long input, and why.
EXEMPT = {
    # Only ever .match()ed on a string literal of at most 32 characters
    # (sql_placeholders._is_readable_string).
    "src.egress.sql_placeholders._FORMAT_RE",
}


_BC_PATH = os.path.join(os.path.dirname(__file__), "..", "..", "shared", "mcp-connectors", "base_connector.py")


def _modules():
    yield masking
    for info in pkgutil.iter_modules(src.egress.__path__):
        yield importlib.import_module(f"src.egress.{info.name}")
    spec = importlib.util.spec_from_file_location("base_connector", _BC_PATH)
    bc = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bc)
    yield bc


def scrub_rules(module) -> dict[str, object]:
    """Every module-level object with a .sub method, alone or first in a
    (pattern, replacement) pair inside a list or tuple."""
    found: dict[int, tuple[str, object]] = {}
    for name, value in vars(module).items():
        if callable(getattr(value, "sub", None)) and hasattr(value, "pattern"):
            found[id(value)] = (name, value)
        elif isinstance(value, (list, tuple)):
            for i, item in enumerate(value):
                rule = item[0] if isinstance(item, tuple) and item else item
                if callable(getattr(rule, "sub", None)) and hasattr(rule, "pattern"):
                    found.setdefault(id(rule), (f"{name}[{i}]", rule))
    return {f"{module.__name__}.{label}": rule for label, rule in found.values()}


def slowest_run(rule) -> tuple[float, str]:
    """The slowest unit for this rule, and its best time over TRIES. A rule over
    budget on its first try is re-timed, so a busy runner does not fail it."""
    for unit in UNITS:
        text = (unit * (N // len(unit) + 1))[:N]
        best = float("inf")
        for _ in range(TRIES):
            start = time.perf_counter()
            rule.sub("", text)
            best = min(best, time.perf_counter() - start)
            if best <= BUDGET_S:
                break
        if best > BUDGET_S:
            return best, unit
    return 0.0, ""


RULES = {label: rule for module in _modules() for label, rule in scrub_rules(module).items()}


def test_the_census_finds_the_scrubber_rules():
    # A census that found nothing would pass every rule it did not see.
    labels = set(RULES)
    assert len(RULES) >= 30, sorted(labels)
    for expected in (
        "src.utils.masking._SECRET_PATTERNS_HEAD[0]",
        "src.utils.masking._SCRUB_PATTERNS[0]",
        "src.utils.masking.HOST_IN_ERROR_TEXT",
        "src.egress.fields._URL_HOST",
        "base_connector._SCRUB_PATTERNS[0]",
    ):
        assert expected in labels, expected
    assert EXEMPT <= labels, EXEMPT - labels


@pytest.mark.parametrize("label", sorted(set(RULES) - EXEMPT))
def test_a_scrubber_rule_is_linear_on_a_long_run(label):
    seconds, unit = slowest_run(RULES[label])
    assert not unit, (
        f"{label} took {seconds:.2f}s on {N} characters of {unit!r} (budget {BUDGET_S}s): "
        "a leading repetition is rescanned from every start. Anchor the start with a "
        "lookbehind for the run's own class, or cap the repetition."
    )
