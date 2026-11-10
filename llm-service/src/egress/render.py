"""Render a prompt YAML with every variable projected by its field class.

A template variable with no ``llm_fields`` tag refuses the render, and so does
a tag naming an unknown class. Variables the caller passed but the template
never reads are dropped before rendering, so an extra key cannot reach the
provider through a template that happens to loop over everything.
"""

from __future__ import annotations

from typing import Any, Optional

from jinja2 import meta

from .fields import FIELD_CLASSES, FieldRefused, project
from .sql_placeholders import LiteralMap

__all__ = ["prompt_fields", "render_for_egress", "template_variables"]

_TEMPLATE_KEYS = ("system", "user", "system_prompt", "user_prompt")


def _template_sources(prompt_def: dict) -> list[str]:
    sources = [str(prompt_def[k]) for k in _TEMPLATE_KEYS if prompt_def.get(k)]
    for msg in prompt_def.get("messages") or []:
        if isinstance(msg, dict) and msg.get("content"):
            sources.append(str(msg["content"]))
    return sources


def template_variables(registry, prompt_name: str) -> set[str]:
    """Every variable the prompt's templates read."""
    prompt_def = registry.load_prompt(prompt_name)
    env = registry._jinja
    names: set[str] = set()
    for src in _template_sources(prompt_def):
        names |= meta.find_undeclared_variables(env.parse(src))
    return names


def prompt_fields(registry, prompt_name: str) -> dict[str, str]:
    fields = registry.load_prompt(prompt_name).get("llm_fields") or {}
    if not isinstance(fields, dict):
        raise FieldRefused(f"{prompt_name}: llm_fields is not a map")
    return {str(k): str(v) for k, v in fields.items()}


def render_for_egress(
    registry,
    prompt_name: str,
    variables: Optional[dict],
    dialect: Optional[str] = None,
) -> tuple[list[dict], LiteralMap]:
    """Return the prompt's messages and the SQL literals taken out of it.

    Raises :class:`FieldRefused` when a variable is untagged, a tag is
    unknown, or a value does not fit its class.
    """
    variables = dict(variables or {})
    fields = prompt_fields(registry, prompt_name)
    used = template_variables(registry, prompt_name)
    untagged = sorted(used - set(fields))
    if untagged:
        raise FieldRefused(f"{prompt_name}: variable(s) with no llm_fields tag: {', '.join(untagged)}")
    unknown = sorted(f"{k}={v}" for k, v in fields.items() if v not in FIELD_CLASSES)
    if unknown:
        raise FieldRefused(f"{prompt_name}: unknown field class: {', '.join(unknown)}")

    sql_dialect = dialect if dialect is not None else variables.get("dialect")
    literals = LiteralMap()
    projected: dict[str, Any] = {}
    for name in sorted(used):
        if name not in variables:
            continue  # the template's own default applies
        projected[name] = project(
            fields[name],
            variables[name],
            name=f"{prompt_name}:{name}",
            dialect=sql_dialect if isinstance(sql_dialect, str) else None,
            literals=literals,
        )
    return registry.render_messages(prompt_name, projected), literals
