"""H5/H6 — scrubber parity + canary-PII enforcement.

Two guarantees the compliance claim ("no customer row values/PII/credentials
reach the LLM or the log backend") rests on:

  * PARITY: the llm-service scrubber (masking.scrub_error_for_llm) and the
    connector scrubber (base_connector.scrub_sensitive) produce byte-identical
    output. They are hand-maintained copies; drift = a leak on one path. (The
    Go scrubber pkg/llmscrub/scrub.go shares the same rule set; its Go-side
    parity is guarded by go test + the shared intent documented here.)

  * ENFORCEMENT: a battery of canary PII/secret strings never survives
    scrubbing on either path.
"""

import importlib.util
import json
import os

from src.utils.masking import scrub_error_for_llm, scrub_secrets_for_llm

_BC_PATH = os.path.join(
    os.path.dirname(__file__), "..", "..", "shared", "mcp-connectors", "base_connector.py"
)
_spec = importlib.util.spec_from_file_location("base_connector_parity", _BC_PATH)
_bc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bc)
connector_scrub = _bc.scrub_sensitive

# (input, canary-that-must-not-survive)
FIXTURE = [
    ("duplicate key value violates unique constraint; Key (email)=(jane@acme.com)", "jane@acme.com"),
    ("null value in column violates not-null; Failing row contains (1, jane@acme.com, secret)", "jane@acme.com"),
    ("INSERT INTO t VALUES ('Jane', 'jane@acme.com', 41)", "jane@acme.com"),
    ("connect failed: postgres://user:hunter2@db:5432/app", "hunter2"),
    ("auth error: Bearer sk-abcdefghijklmnopqrstuvwxyz012345", "sk-abcdefghijklmnopqrstuvwxyz012345"),
    ('config: {"password": "hunter2", "api_key": "AKIA1234567890EXAMPLE"}', "hunter2"),
    ("ssn 123-45-6789 on file", "123-45-6789"),
    ("card number 4111111111111111 declined", "4111111111111111"),
    ("jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc for user", "eyJhbGciOiJIUzI1NiJ9"),
]


def test_scrubbers_are_byte_identical():
    for text, _canary in FIXTURE:
        a = scrub_error_for_llm(text)
        b = connector_scrub(text)
        assert a == b, f"scrubber drift on {text!r}:\n  masking={a!r}\n  connector={b!r}"


def test_canaries_never_survive_either_scrubber():
    for text, canary in FIXTURE:
        for name, fn in (("masking", scrub_error_for_llm), ("connector", connector_scrub)):
            out = fn(text)
            assert canary not in out, f"{name} leaked {canary!r} in {out!r}"


# Kafka SASL/TLS credentials. Kept separate from FIXTURE on purpose: these rows
# are checked against masking only, because the connector copy
# (shared/mcp-connectors/base_connector.py scrub_sensitive) has no
# compound-credential rule yet and the golden test below already fails on it —
# duplicating that same red here would add noise, not signal. Fold these into
# FIXTURE once the connector block is patched and re-synced.
#
# Every spelling below is one this codebase actually produces: env vars read by
# shared/go/kafkaclient, Go's %+v rendering of its Config, librdkafka/JAAS
# property names, and the AWS credentials the MSK IAM path reads. None of them
# has a word boundary before the credential word, so the \b-anchored generic KV
# rule matched none of them and the secret shipped verbatim. A broker credential
# is shared across all tenants, so one leaked connect error is cluster-wide.
KAFKA_FIXTURE = [
    ("kafka connect failed: KAFKA_SASL_PASSWORD=s3cret-broker-pw mechanism PLAIN", "s3cret-broker-pw"),
    ("env dump: SASL_PASSWORD: brokerpw123", "brokerpw123"),
    ("kafka-python: sasl_plain_password=hunter2 sasl_mechanism=SCRAM-SHA-512", "hunter2"),
    (
        "oauth: KAFKA_SASL_OAUTHBEARER_CLIENT_SECRET=cs-9f8e7d6c5b4a token endpoint unreachable",
        "cs-9f8e7d6c5b4a",
    ),
    ("dial error: kafkaclient.Config{Brokers:[b1:9092] OAuthClientSecret:topsecretpw TLS:true}", "topsecretpw"),
    ("msk iam: AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY signer failed", "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"),
    ("ssl: KAFKA_SSL_KEY_PASSWORD=keystorepw could not load keystore", "keystorepw"),
]


def test_kafka_credentials_never_survive_masking():
    for text, canary in KAFKA_FIXTURE:
        out = scrub_error_for_llm(text)
        assert canary not in out, f"masking leaked Kafka credential {canary!r} in {out!r}"


def test_kafka_key_names_and_non_credentials_are_preserved():
    # The key NAME is metadata an operator needs (it says which variable to
    # rotate), and not every KAFKA_* value is a secret. Over-redacting here would
    # make broker misconfiguration undiagnosable.
    preserved = [
        (
            "SASL_SSL requires both KAFKA_SASL_USERNAME and KAFKA_SASL_PASSWORD to be set",
            "SASL_SSL requires both KAFKA_SASL_USERNAME and KAFKA_SASL_PASSWORD to be set",
        ),
        ("KAFKA_SSL_CA_LOCATION=/etc/ssl/ca.pem", "KAFKA_SSL_CA_LOCATION=/etc/ssl/ca.pem"),
        ("partition_key=customer_id", "partition_key=customer_id"),
    ]
    for text, expected in preserved:
        assert scrub_error_for_llm(text) == expected
    # ...and the key name survives on the rows that ARE redacted.
    assert "KAFKA_SASL_PASSWORD=" in scrub_error_for_llm(
        "kafka connect failed: KAFKA_SASL_PASSWORD=s3cret-broker-pw mechanism PLAIN"
    )


_GOLDEN_PATH = os.path.join(os.path.dirname(__file__), "..", "..", "shared", "scrubber_golden.json")


def test_both_python_scrubbers_match_cross_language_golden():
    """Pin masking + connector to the SAME shared/scrubber_golden.json the Go
    test (pkg/llmscrub/scrub_golden_parity_test.go) asserts against. The golden
    is generated from masking.py, so:
      * masking != golden  → masking changed without regenerating the golden
        (regenerate with: python3 -c 'from src.utils.masking import
        scrub_error_for_llm as S; ...' — see repo docs), which then forces the
        Go test to prove Go changed in lockstep;
      * connector != golden → the connector scrubber drifted.
    Together with the Go test this binds all THREE scrubbers to one contract.
    """
    with open(_GOLDEN_PATH) as f:
        golden = json.load(f)
    assert golden, "golden fixture is empty"
    for row in golden:
        for name, fn in (("masking", scrub_error_for_llm), ("connector", connector_scrub)):
            got = fn(row["input"])
            assert got == row["expected"], (
                f"{name} scrubber diverged from golden on {row['input']!r}:\n"
                f"  got:  {got!r}\n  want: {row['expected']!r}"
            )


def test_diagnostic_context_preserved():
    # Table names, ISO timestamps, and 32-hex trace ids must survive so errors
    # stay diagnosable — over-redaction would defeat the point.
    text = 'relation "orders" does not exist at 2026-07-17T10:00:00 trace=abc123def4567890abc123def4567890'
    for fn in (scrub_error_for_llm, connector_scrub):
        out = fn(text)
        assert '"orders"' in out and "2026-07-17T10:00:00" in out


# URLs. The 40-character blob rule read a long URL path as a token: a spec URL
# in a generator error came back as `https://raw.githubusercontent.com/HubSpot/
# HubSpot-public-api-spec-[redacted].json`, unusable to the person who pasted it.
# And it missed what a URL does carry: credential-named query parameters
# shorter than 40 characters and a password with an empty user name. URLs now
# get their own rule. Its host and readable path stay, and token-shaped
# segments and credential parameters go.
# Bug class: a URL read as a secret, and a secret in a URL read as text.
URL_READABLE = (
    "https://raw.githubusercontent.com/HubSpot/HubSpot-public-api-spec-collection/main/PublicApiSpecs/CRM/Contacts/Rollouts/424/v3/contacts.json",
    "https://raw.githubusercontent.com/github/rest-api-description/main/descriptions/api.github.com/api.github.com.json",
    "https://api.example.com/v1/items?page=2&sort=name",
)
URL_CANARIES = (
    ("redis connect failed: redis://:hunter2@cache:6379/0", "hunter2"),
    ("GET https://maps.googleapis.com/maps/api/geocode/json?key=AIzaSyA1b2C3d4E5f6G7h8I9j0KlMnOpQrStUvW&a=x", "AIzaSy"),
    ("callback https://app.example.com/oauth/callback?code=4/0AY0e-g7abcDEF&state=xyz", "0AY0e-g7abcDEF"),
    ("GET https://b.s3.amazonaws.com/o.csv?X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1", "AKIAIOSFODNN7EXAMPLE"),
    ("POST https://hooks.example.com/services/T0ABCDE12/B0ABCDE34/aB3dE5fG7hI9jK1lM3nO5pQ7 failed", "aB3dE5fG7hI9jK1lM3nO5pQ7"),
    ("GET https://docs.google.com/spreadsheets/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms/edit", "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms"),
)


def test_a_url_stays_readable_on_every_python_scrubber():
    for url in URL_READABLE:
        text = f"spec fetch failed: {url} returned 404"
        for name, fn in (("masking", scrub_error_for_llm), ("connector", connector_scrub), ("egress", scrub_secrets_for_llm)):
            assert fn(text) == text, f"{name} scrubber changed a readable URL: {fn(text)!r}"


def test_a_secret_inside_a_url_never_survives_any_python_scrubber():
    for text, canary in URL_CANARIES:
        for name, fn in (("masking", scrub_error_for_llm), ("connector", connector_scrub), ("egress", scrub_secrets_for_llm)):
            out = fn(text)
            assert canary not in out, f"{name} leaked {canary!r} in {out!r}"
