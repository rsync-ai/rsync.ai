"""The request parameters a model takes, fitted per model.

Every call site sets ``temperature`` and ``max_tokens`` from its prompt YAML,
and the model is whichever one an admin or the environment names (Admin ->
Settings -> AI models, ``LLM_MODEL``, the per-feature models). OpenAI's
reasoning models -- the o-series and GPT-5 on -- answer a ``temperature`` other
than 1 with a 400 and take ``max_completion_tokens`` in place of
``max_tokens``; ``stop`` is refused on o3 and o4-mini. So the guard fits each
request here, once, for every call site: a reasoning model is sent no
sampling and no stop. Dropping a sampling parameter never fails a call;
sending one can. Its reasoning counts inside ``max_completion_tokens``, so it
gets the answer length the caller asked for plus ``THINKING_HEADROOM``, as a
thinking Claude model does.

The model id decides, whoever serves it: OpenAI, an Azure deployment named
for the model, or a gateway in front of either. Claude's rule is in
anthropic_http.py, which every Claude call goes through; no Gemini, Groq or
Ollama model id matches, so those get what the call site set.
"""

from __future__ import annotations

import re

from .anthropic_http import THINKING_HEADROOM

__all__ = ["fit_params"]

# o1, o3-mini, o4-mini, gpt-5, gpt-5.1, gpt-6 ..., and a fine-tune of one.
# gpt-4o, gpt-4.1 and gpt-oss are not: no digit 5-9 after "gpt-".
_REASONING_MODEL = re.compile(r"^(ft:)?(o[1-9]|gpt-[5-9])")
_NOT_TAKEN = ("temperature", "top_p", "stop")


def fit_params(model: str, params: dict) -> dict:
    """``params`` as ``model`` takes them. A new dict; ``params`` is not changed."""
    fitted = dict(params)
    if not _REASONING_MODEL.match((model or "").lower()):
        return fitted
    for name in _NOT_TAKEN:
        fitted.pop(name, None)
    max_tokens = fitted.pop("max_tokens", None)
    if max_tokens is not None:
        # One the call site set itself wins: it already counts the reasoning.
        fitted.setdefault("max_completion_tokens", int(max_tokens) + THINKING_HEADROOM)
    return fitted
