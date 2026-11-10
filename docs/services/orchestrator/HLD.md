## Backend Orchestrator — HLD

### Purpose
The Orchestrator is the **execution brain** of rsync-ai. It:
- runs pipeline execution logic (Temporal-driven V2, Kafka messaging),
- validates connections/configs and routes MCP tool calls,
- manages agent workers, retries, and operational safety checks.

### Runtime Interface
- **Container**: `rsync-ai-orchestrator`
- **Internal port**: `8080`
- **Host port**: `8081` (debug/dev)

### Responsibilities
- **Pipeline execution** (agentic):
  - executes plan steps via MCP connector client(s),
  - publishes and consumes pipeline/agent events via Kafka,
  - coordinates with Temporal (workflows run in Temporal; Orchestrator provides services used by activities and runtime lookups).
- **Connection/config validation**:
  - validates against **version-specific** connector metadata/spec.
- **Connector runtime management**:
  - reads `shared/mcp-connectors` for connector metadata and routing,
  - can call tool-generator for self-healing deploy/start when connector containers are missing (best-effort).
- **Operational agents**:
  - retention, sentinel, schedulers.

### Dependencies
- **Postgres**: pipeline metadata, connection configs, status tables
- **Kafka**: the data plane (CDC and batch topics) and pipeline domain events. Agent
  requests do not travel over Kafka: the agent command/result topics were removed in #1227.
- **Redis**: correlation store. Agent stages hand off their requests and replies through it.
- **Shared volume**: `shared/mcp-connectors` (connector artifacts)
- **Tool Generator**: self-healing deployment URL (`TOOL_GENERATOR_URL`)

### Scaling / HA Notes (Important)
- The orchestrator runs as **one replica**. Nothing in compose blocks a second one (the
  per-agent Kafka topics behind the old "do not scale" warning were removed in #1227), but
  more than one replica is **untested**.
  - There is no leader election, so each replica would run its own schedulers, retention
    and sentinel loops.

### Observability
- OpenTelemetry tracing + JSON logs (fluent-bit ingestion).
- Prometheus metrics endpoint exists (see LLD for exact path).

### Failure Modes
- **Kafka issues**: agent messaging delays; pipelines may stall/retry.
- **Connector container missing**: executor failures unless self-healing can redeploy.
- **Redis correlation store down**: V2 activities degrade/fail; may fall back depending on workflow.


