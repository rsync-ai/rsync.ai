"""Every SQL-writing prompt tells the model it has not seen the values it filters on.

Found in the 2026-10-11 Ask rsync.ai eval: asked for "the most declined card
transactions", the writer, which sees names and types and never values, wrote
status = 'DECLINED' (stored as 'declined') and txn_type = 'card' (stored as
'card_purchase'). The query ran and matched nothing.

Bug class: **a guessed text value turns a right query into 0 rows.** The
writer cannot be shown the values (llm-egress.md: metadata only), so each of
the four prompts that write or repair SQL says so, prefers a column whose name
already holds the state, and has a guessed value compared without case. The
Explorer names the text matches of a 0-row result
(frontend/src/lib/explorer/textFilters.ts) for the guesses that remain.

This checks the prompt as rendered for the provider, not the model's answer:
whether a model follows it is the Ask eval's job.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from src.egress.call_sites import CALL_SITES
from src.egress.render import render_for_egress
from src.utils.prompt_registry import PromptRegistry

REGISTRY = PromptRegistry(str(Path(__file__).resolve().parents[1] / "prompts"))

WRITERS = [
    "explorer/sql_generate",
    "explorer/sql_generate_offline",
    "explorer/sql_repair",
    "explorer/sql_repair_offline",
]

VARIABLES = {
    "dialect": "postgres",
    "question": "Which merchants have the most declined card transactions?",
    "tables": [
        {
            "schema": "fintech_silver",
            "name": "transactions",
            "alias": "t",
            "columns": [{"name": "status", "type": "text"}, {"name": "txn_type", "type": "text"}],
        }
    ],
    "previous_sql": "SELECT 1",
    "issues": [],
}


def _sent(prompt: str) -> str:
    messages, _ = render_for_egress(REGISTRY, prompt, VARIABLES, dialect="postgres")
    return "\n".join(m["content"] for m in messages)


def test_the_writers_are_every_prompt_of_the_sql_call_site():
    # A prompt added to the call that writes SQL joins WRITERS, or the rule
    # does not reach it.
    assert set(CALL_SITES["explorer.sql"].prompts) == set(WRITERS)


@pytest.mark.parametrize("prompt", WRITERS)
def test_the_prompt_says_the_values_are_unseen_and_how_to_filter_without_them(prompt):
    sent = _sent(prompt)
    assert "never the values a column holds" in sent
    assert "whose name already holds it" in sent
    assert "LOWER(" in sent
