"""The bundled Kafka never creates a topic on first use, and no test relies on it.

`auto.create.topics.enable` is a broker setting, and on a customer-managed
cluster it is not ours: several managed offerings ship it off. With it on, a
topic nobody creates explicitly is made quietly by the broker on the first
produce or metadata request, so a missing creator stays invisible until the
first cluster with it off runs a pipeline that completes and moves zero rows
(KI-KAFKA-DATAPLANE-AUTOCREATE-ONLY). Every broker this repo bundles therefore
has it off, so a missing creator fails here first.

The e2e harness is the half that broke when that flipped: its CDC tests register
Debezium connectors straight with Kafka Connect, bypassing the orchestrator --
whose `topic.creation.default.*` shape (shared/mcp-connectors/internal/debezium
`_topic_creation`) and per-pipeline pre-creates are what make the product's
topics. A connector config without that shape leaves Connect's producer waiting
on UNKNOWN_TOPIC_OR_PARTITION for every topic it writes: the 2026-10-01 local
gate logged 408 such warnings and failed all three CDC tests it ran, while the
batch test, whose topic the harness pre-creates, passed.
"""

import pathlib
import re

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
E2E = REPO / "e2e"
COMPOSES = ("docker-compose.yml", "docker-compose.quickstart.yml")
HELM_KAFKA = REPO / "deploy" / "helm" / "rsync-ai" / "templates" / "infra" / "kafka.yaml"

DEBEZIUM_CLASS = re.compile(r'"connector\.class"\s*:\s*"io\.debezium\.')
CREATION_KEYS = (
    "topic.creation.default.replication.factor",
    "topic.creation.default.partitions",
)


@pytest.mark.parametrize("compose", COMPOSES)
def test_bundled_compose_broker_has_auto_create_off(compose):
    env = yaml.safe_load((REPO / compose).read_text())["services"]["kafka"]["environment"]
    value = env.get("KAFKA_AUTO_CREATE_TOPICS_ENABLE")
    assert str(value).lower() == "false", (
        f"{compose}: the kafka service sets KAFKA_AUTO_CREATE_TOPICS_ENABLE={value!r}. "
        "Unset means Kafka's default, which is ON -- and with it on, a topic with no "
        "explicit creator is invisible here and moves zero rows on a customer cluster "
        "that has it off. Keep it 'false'."
    )


def test_bundled_chart_broker_has_auto_create_off():
    text = HELM_KAFKA.read_text()
    match = re.search(
        r'-\s*name:\s*KAFKA_AUTO_CREATE_TOPICS_ENABLE\s*\n\s*value:\s*"?(\w+)"?', text
    )
    assert match, f"{HELM_KAFKA.relative_to(REPO)} no longer sets KAFKA_AUTO_CREATE_TOPICS_ENABLE"
    assert match.group(1).lower() == "false", match.group(0)


def _debezium_configs():
    found = {}
    for path in sorted(E2E.rglob("*")):
        if path.suffix not in (".py", ".sh") or not path.is_file():
            continue
        text = path.read_text(errors="replace")
        n = len(DEBEZIUM_CLASS.findall(text))
        if n:
            found[path.relative_to(REPO).as_posix()] = (n, text)
    return found


def test_the_scan_finds_the_harness_connectors():
    # A pattern that silently matched nothing would pass every assertion below.
    configs = _debezium_configs()
    assert sum(n for n, _ in configs.values()) >= 20, sorted(configs)
    assert "e2e/test_mysql_cdc_to_postgres.sh" in configs  # the per-PR smoke gate's CDC test


def test_every_e2e_debezium_connector_creates_its_own_topics():
    missing = []
    for rel, (n, text) in _debezium_configs().items():
        for key in CREATION_KEYS:
            have = len(re.findall(rf'"{re.escape(key)}"\s*:', text))
            if have < n:
                missing.append(f"{rel}: {n} Debezium connector config(s), {have} carry {key}")
    assert not missing, (
        "These e2e connector configs name no topic.creation.default.* shape, so with "
        "broker auto-create off Kafka Connect never creates the topics they write and "
        "the test moves zero rows. Add both keys -- \"-1\" asks for the broker's own "
        "defaults, the shape auto-create used to give them:\n  " + "\n  ".join(missing)
    )
