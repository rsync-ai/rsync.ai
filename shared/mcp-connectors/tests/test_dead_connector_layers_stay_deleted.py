"""Two connector layers that never ran were deleted; they must not come back.

OPTIMIZATION TEMPLATES (KI-CONNECTOR-OPTIMIZATION-LAYER-IS-INERT). Every
base_connector.py loaded `optimization_templates.json` from its own directory
IF IT EXISTED -- and it never did, in any copy. So OPTIMIZATION_TEMPLATES was
always `{}`, `self.optimization_category` was assigned `None` and nothing else
anywhere, every helper keyed off them took its empty branch, and every
connector's get_capabilities() published `"optimization_category": null`. A
contributor who followed the base class's own instruction (set the category in
__init__) got no behaviour change and no error. An absent optional file and a
`None` field look exactly like a working layer with nothing to say, which is
why this survived: nothing ever failed.

The fix deleted the layer rather than authoring the missing JSON. This test is
the guard the KI asked for: if any of these names reappears in a connector
file, it has to arrive with a file that makes it do something -- and a review
that sees this test fail asks that question.

CONNECTOR-SIDE OAUTH TOKEN STORE
(KI-OAUTH-TOKEN-STORE-UNREACHABLE-BY-ITS-ONLY-USERS). The tool generator copied
the canonical `oauth/` package into connector build contexts, and
base_connector.py lazily imported `oauth.token_manager` to read, refresh and
persist tokens on a mounted volume. The two sets never met: the six connectors
that carried the package (mongodb, mysql, oracle, sqlserver, postgresql, aws-s3)
never use OAuth, and the ones that do (google-sheets, generated oauth2 REST
connectors) carried no copy -- so their import failed, at debug, and no
connector ever stored a token. OAuth tokens live in the gateway's oauth_tokens
table: the gateway injects a decrypted `access_token` into the connection
config, and the orchestrator refreshes it (executor.go executeWithOAuthRetry).
A connector only holds the token it is handed. The canonical package stays at
shared/mcp-connectors/oauth/ -- providers.json and oauth_registry.py are live
gateway/generator inputs -- but no connector may carry a copy or reach for the
store again.

WHY IT CANNOT PASS VACUOUSLY. The scan has to find the vendored
base_connector.py copies it exists to protect (22 when this was written). A
moved connector tree or a broken walk would otherwise make the assertion range
over nothing. The OAuth checks also confirm the canonical package is where the
walk expects it, so excluding it cannot exclude everything.
"""

import os

CONNECTORS_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# A name that means the inert layer is back. `optimization_category` is also
# the response-contract key, so the schema and kafka-mcp-sink are covered too.
OPTIMIZATION_LAYER_NAMES = (
    "optimization_category",
    "OPTIMIZATION_TEMPLATES",
    "optimization_templates",
    "_get_optimization_tips",
    "_check_anti_patterns",
)


# The canonical OAuth package. It stays: its registry is read by the gateway
# and the generator, and llm-service's /v1/oauth/* routes import its store.
CANONICAL_OAUTH_DIR = os.path.join(CONNECTORS_ROOT, "oauth")

# A name that means a connector is reaching for the token store again.
TOKEN_STORE_NAMES = (
    "oauth.token_manager",
    "TokenManager",
    "_token_manager",
    "create_refresh_callback",
)

_SKIPPED_DIRS = ("tests", "__pycache__", ".venv", "node_modules")


def _walk():
    for dirpath, dirnames, filenames in os.walk(CONNECTORS_ROOT):
        dirnames[:] = [d for d in dirnames if d not in _SKIPPED_DIRS]
        yield dirpath, dirnames, filenames


def _connector_python_files():
    for dirpath, _dirnames, filenames in _walk():
        for name in filenames:
            if name.endswith(".py"):
                yield os.path.join(dirpath, name)


def test_the_scan_reaches_every_vendored_base_connector():
    copies = [p for p in _connector_python_files() if os.path.basename(p) == "base_connector.py"]
    assert len(copies) >= 20, (
        f"found only {len(copies)} base_connector.py copies under {CONNECTORS_ROOT}; "
        "the walk is broken or the tree moved, and every check below is vacuous"
    )


def test_no_connector_file_carries_the_optimization_template_layer():
    hits = []
    for path in _connector_python_files():
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for name in OPTIMIZATION_LAYER_NAMES:
            if name in text:
                hits.append(f"{os.path.relpath(path, CONNECTORS_ROOT)}: {name}")
    assert hits == [], (
        "The optimization-template layer was deleted because it never ran "
        "(KI-CONNECTOR-OPTIMIZATION-LAYER-IS-INERT): its JSON never existed and its "
        "category was never set. Bring it back only together with the data that "
        "makes it do something, and update this guard in the same change. Found: "
        + "; ".join(hits)
    )


def test_the_canonical_oauth_package_is_where_the_exclusion_points():
    assert os.path.isfile(os.path.join(CANONICAL_OAUTH_DIR, "oauth_registry.py")), (
        f"{CANONICAL_OAUTH_DIR} has no oauth_registry.py; the exclusion below would "
        "be skipping the wrong directory"
    )


def test_no_connector_carries_a_copy_of_the_oauth_package():
    copies = []
    for dirpath, dirnames, _filenames in _walk():
        for d in dirnames:
            path = os.path.join(dirpath, d)
            if d == "oauth" and path != CANONICAL_OAUTH_DIR:
                copies.append(os.path.relpath(path, CONNECTORS_ROOT))
    assert copies == [], (
        "A connector carries a copy of the oauth/ package. The vendored copies were "
        "deleted (KI-OAUTH-TOKEN-STORE-UNREACHABLE-BY-ITS-ONLY-USERS): no connector "
        "stores or refreshes OAuth tokens -- the gateway injects them and the "
        "orchestrator refreshes them -- and a credential store shipped to connectors "
        "that never asked for one is how #833's plaintext fallback reached six "
        "images. Found: " + "; ".join(sorted(copies))
    )


def test_no_connector_file_reaches_for_the_token_store():
    hits = []
    for path in _connector_python_files():
        if os.path.commonpath([path, CANONICAL_OAUTH_DIR]) == CANONICAL_OAUTH_DIR:
            continue
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for name in TOKEN_STORE_NAMES:
            if name in text:
                hits.append(f"{os.path.relpath(path, CONNECTORS_ROOT)}: {name}")
    assert hits == [], (
        "A connector reaches for the OAuth token store, which no connector image "
        "carries (KI-OAUTH-TOKEN-STORE-UNREACHABLE-BY-ITS-ONLY-USERS). Hold the token "
        "the connection config hands in (set_oauth_token); leave refresh to the "
        "orchestrator. Found: " + "; ".join(hits)
    )
