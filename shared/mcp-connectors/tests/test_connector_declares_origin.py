"""Every public connector's metadata.json declares its origin (``"source"``).

The api-gateway reads ``source`` to decide whether a connector is
tool-generated (confidence chip shown, lifecycle computed) or hand-written
(chip suppressed). When the key is missing it infers the origin, and that
inference once read a key the generator never wrote: github-rest, notion-rest,
stripe and widgets-graphql all listed as ``built_in``. A connector that states
its origin cannot be misread that way, so the rule is "declare it", checked
for every connector this repo ships at its current version — a new connector
inherits the check without anyone adding it here.

Offline: reads metadata.json only.
"""
from __future__ import annotations

import glob
import json
import os

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
PUBLIC = os.path.join(os.path.dirname(HERE), "public")

# The origins the api-gateway understands (tools.go inferConfidenceLevel and the
# spec's `source` field). "hand-curated" is bigquery's spelling; the gateway
# treats it like built_in.
KNOWN_ORIGINS = {"built_in", "generated", "partner", "custom", "hand-curated"}

# Floor: a discovery regression returns nothing, and nothing passes every check.
MIN_CONNECTORS = 20


def _current_metadata():
    """(name, path) of each public connector's current-version metadata.json.

    Recursive: database/ and storage/ sit one level deeper than the API
    connectors, and a flat glob misses them without any error.
    """
    found = []
    for manifest in sorted(glob.glob(os.path.join(PUBLIC, "**", "latest.json"), recursive=True)):
        if os.sep + "versions" + os.sep in manifest:
            continue
        root = os.path.dirname(manifest)
        with open(manifest) as handle:
            current = json.load(handle)["current_version"]
        found.append((os.path.basename(root), os.path.join(root, "versions", str(current), "metadata.json")))
    return found


CONNECTORS = _current_metadata()


def test_discovery_finds_every_connector():
    assert len(CONNECTORS) >= MIN_CONNECTORS, CONNECTORS


@pytest.mark.parametrize("name,path", CONNECTORS, ids=[n for n, _ in CONNECTORS])
def test_metadata_declares_a_known_origin(name, path):
    with open(path) as handle:
        meta = json.load(handle)
    assert meta.get("source") in KNOWN_ORIGINS, (
        f"{name}: metadata.json source={meta.get('source')!r}; declare one of "
        f"{sorted(KNOWN_ORIGINS)} so the api-gateway never has to infer it"
    )
