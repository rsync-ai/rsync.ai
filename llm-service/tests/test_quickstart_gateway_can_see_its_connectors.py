"""A self-host install showed "Running (0)" on the Connectors page with every
connector up.

api-gateway learns which MCP containers are running from the Docker API
(getMCPDockerContainers in api-gateway/internal/handlers/tools.go). It reaches that
API through MCP_DOCKER_API_URL, a read-only docker-socket-proxy; the fallback is the
raw unix socket, which the non-root gateway image neither mounts nor could read. Of
the containers it lists, it keeps only those labelled rsync-ai.mcp=true or belonging
to the compose project rsync-ai-mcp.

docker-compose.yml wires both halves: the proxy, and the variable. The quickstart --
the one standalone compose, and the only one install.sh ships -- wired neither. Its
own connector services also sit in project `rsync-ai` with no label, so a proxy alone
would still have found none of them. Every card fell through to
"auto_deploys_on_use", which the UI renders as "Ready", and the running count read 0
on a host where every connector was up. Found on the v0.1.7 2-core/8 GB VM install,
2026-09-28.

The variable name, the label and the container-name pattern are read out of tools.go
rather than copied here, so renaming any of them there fails this guard instead of
silently disconnecting the quickstart a second time.

Static and cheap: YAML and text parsing, no docker.
"""

import json
import os
import re
import urllib.parse

import pytest
import yaml

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
QUICKSTART = os.path.join(REPO_ROOT, "docker-compose.quickstart.yml")
TOOLS_GO = os.path.join(REPO_ROOT, "api-gateway/internal/handlers/tools.go")

PROXY_IMAGE = "tecnativa/docker-socket-proxy:"
# The network the quickstart's connector containers join. The proxy must stay off it:
# a connector runs code this product did not write, and the Docker API it would reach
# there can read every container's environment, credentials included.
MCP_NETWORK = "rsync-ai-mcp"


def _tools_go():
    return open(TOOLS_GO).read()


def _quickstart_services():
    return yaml.safe_load(open(QUICKSTART))["services"]


def _gateway_env_var():
    """The variable dockerAPIClient reads for the Docker API base URL."""
    names = re.findall(r'os\.Getenv\("(MCP_DOCKER[A-Z_]*)"\)', _tools_go())
    assert len(set(names)) == 1, (
        f"expected one MCP_DOCKER* variable read in tools.go, found {sorted(set(names))}; "
        "the parser is stale or the gateway now reads the Docker API from somewhere else"
    )
    return names[0]


def _gateway_label_filters():
    """Every `label` value in the /containers/json filters tools.go sends."""
    labels = set()
    for q in re.findall(r"/containers/json\?[^\"]*filters=([^\"&]+)", _tools_go()):
        labels |= set(json.loads(urllib.parse.unquote(q)).get("label", []))
    assert labels, "found no /containers/json label filter in tools.go; the parser is stale"
    return labels


def _connector_name_patterns():
    """The container-name regexes getMCPDockerContainers accepts as a connector."""
    pats = re.findall(r"regexp\.MustCompile\(`(\^rsync-ai-[^`]*-mcp\$)`\)", _tools_go())
    assert pats, "found no rsync-ai-...-mcp container-name regex in tools.go; the parser is stale"
    return [re.compile(p) for p in pats]


def _labels(svc):
    raw = svc.get("labels") or []
    if isinstance(raw, dict):
        return {f"{k}={v}" for k, v in raw.items()}
    return set(raw)


def _networks(svc):
    raw = svc.get("networks")
    if raw is None:
        return {"default"}
    return set(raw)


def _proxy_service_name():
    env = _quickstart_services()["api-gateway"].get("environment") or {}
    var = _gateway_env_var()
    url = env.get(var)
    assert url, (
        f"api-gateway in docker-compose.quickstart.yml does not set {var}, so it cannot "
        "reach the Docker API and every connector card reads 'Ready' with Running (0)"
    )
    host = urllib.parse.urlparse(url).hostname
    assert host in _quickstart_services(), (
        f"{var}={url} names host {host!r}, which is not a service in "
        "docker-compose.quickstart.yml"
    )
    return host


def test_the_gateway_reaches_a_docker_api_in_this_file():
    name = _proxy_service_name()
    svc = _quickstart_services()[name]
    assert str(svc.get("image", "")).startswith(PROXY_IMAGE), (
        f"{name} is not a docker-socket-proxy (image {svc.get('image')!r}); the gateway "
        "would be pointed at something that does not serve the Docker API"
    )
    assert "profiles" not in svc, (
        f"{name} is behind a profile, so a default `up` does not start it and the "
        "gateway's depends_on on it fails the whole stack"
    )
    gateway = _quickstart_services()["api-gateway"]
    assert name in (gateway.get("depends_on") or {}), f"api-gateway does not depend on {name}"
    assert _networks(svc) & _networks(gateway), f"api-gateway and {name} share no network"


def test_the_proxy_is_read_only_and_off_the_connector_network():
    name = _proxy_service_name()
    svc = _quickstart_services()[name]
    env = svc.get("environment") or {}
    assert str(env.get("POST")) == "0", f"{name} must set POST: 0 (GET/HEAD only)"
    socket_mounts = [v for v in svc.get("volumes") or [] if "docker.sock" in str(v)]
    assert socket_mounts and all(str(v).endswith(":ro") for v in socket_mounts), (
        f"{name} must mount the Docker socket read-only, got {socket_mounts}"
    )
    assert "ports" not in svc, f"{name} publishes a port; the Docker API must stay internal"
    assert MCP_NETWORK not in _networks(svc), (
        f"{name} joins {MCP_NETWORK}, so every connector container can query the "
        "Docker API and read other containers' environment"
    )


def _connector_services():
    pats = _connector_name_patterns()
    return sorted(
        name
        for name, svc in _quickstart_services().items()
        if any(p.match(str(svc.get("container_name", ""))) for p in pats)
    )


def test_the_quickstart_defines_connector_services_to_check():
    # A guard over an empty set passes on nothing. Four today: minio, sample-data,
    # debezium, kafka-mcp-sink.
    assert len(_connector_services()) >= 4, _connector_services()


@pytest.mark.parametrize("service", _connector_services())
def test_every_quickstart_connector_carries_the_gateway_label(service):
    svc = _quickstart_services()[service]
    wanted = _gateway_label_filters()
    assert _labels(svc) & wanted, (
        f"{service} ({svc.get('container_name')}) carries none of {sorted(wanted)}. It runs "
        "in compose project `rsync-ai`, not `rsync-ai-mcp`, so without the label "
        "api-gateway never lists it and its card reads 'Ready' while it runs"
    )
