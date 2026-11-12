# Agent gateway (MCP server)

rsync.ai runs an [MCP](https://modelcontextprotocol.io) server inside its API gateway.
An AI agent that speaks MCP over HTTP (Claude Code, Cursor, or any client that can
send an `Authorization` header) can use it to read a workspace's connections, table
schemas, pipelines and runs, and to ask why a pipeline is stale or a run failed.

It reads metadata, never data. A token writes only through a scope it was given
([What a token can do](#what-a-token-can-do)): `generate_connector` adds a connector
to the catalog from a public API's OpenAPI document or docs page
([Generate a connector](#generate-a-connector)); the model tools create, schedule,
run, pause and resume silver and gold models; `plan_pipeline` proposes a pipeline
and `plan_workflow` drafts a report or data alert, each of which a person checks and
turns on in rsync.ai. No tool runs, creates, edits or deletes a pipeline or a
connection.

## Turn it on

The gateway is off by default. While it is off, the MCP route and the agent token
routes do not exist, and Settings shows no agent section.

Set `RSYNC_AGENT_GATEWAY_ENABLED=true` on the `api-gateway` service and restart it:

- **Docker Compose** (every bundled compose file passes it through): add the line to
  the `.env` file compose reads, or export it in the shell that runs compose, then
  recreate `api-gateway`.
- **Helm**: set it in the chart's top-level `extraEnv`:

```yaml
extraEnv:
  RSYNC_AGENT_GATEWAY_ENABLED: "true"
```

`GET /api/v1/features` then reports `"agent_gateway": true`, which is what the UI
reads.

## Make a token

In **Settings → Connect your AI app**:

1. Name the token after where it will live, such as "Claude Code on my laptop".
2. Pick when it expires: 30 days, 90 days or 1 year. Every token expires, and a
   year is the longest.
3. Tick only what the agent should do besides read. Members and above see **Can
   propose pipelines** (`pipelines:plan`); owners and admins also see **Can generate
   connectors** (`connectors:generate`) and **Can build models** (`models:write`).
4. Select **Create token**. The token is shown once, with ready-made setup for
   Claude Code and Cursor. rsync.ai keeps only its SHA-256 hash, so nothing can show it
   again; the list shows its first 12 characters.

A token starts with `rsat_`. One person can hold 20 live tokens per workspace.

Settings drives three session routes, which a script can call too:
`GET /api/v1/agent-tokens`, `POST /api/v1/agent-tokens` with
`{"name": "...", "expires_in_days": 30, "scopes": ["connectors:generate"]}`
(1 to 365 days, 30 when left out; `scopes` is optional and every token gets
`metadata:read`; any member may add `pipelines:plan`, and only an owner or admin may
add `connectors:generate`, `models:write`, `workflows:write` or `dashboards:write`, or
the request gets 403; an unknown scope gets 400), and `DELETE /api/v1/agent-tokens/:id`.
Settings has no box for `workflows:write` or `dashboards:write` yet, so only this route
grants them.

## Connect an agent

Settings shows the exact address for your deployment: your API address followed by
`/api/agent/v1/mcp`. With `https://rsync.example.com` as that address:

Claude Code:

```bash
claude mcp add --transport http rsync-ai https://rsync.example.com/api/agent/v1/mcp --header "Authorization: Bearer rsat_..."
```

Cursor (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "rsync-ai": {
      "url": "https://rsync.example.com/api/agent/v1/mcp",
      "headers": { "Authorization": "Bearer rsat_..." }
    }
  }
}
```

Both configs hold the token in plain text. Keep it in a user-level config, never in
a file you commit, and revoke it if it leaks.

Settings also shows a card per AI app. Three more take a token today, and their files
never hold it:

Copilot in VS Code (`.vscode/mcp.json`) asks for the token when it starts the server:

```json
{
  "inputs": [
    { "type": "promptString", "id": "rsync-ai-token", "description": "rsync.ai agent token", "password": true }
  ],
  "servers": {
    "rsync-ai": {
      "type": "http",
      "url": "https://rsync.example.com/api/agent/v1/mcp",
      "headers": { "Authorization": "Bearer ${input:rsync-ai-token}" }
    }
  }
}
```

Gemini CLI (`~/.gemini/settings.json`) reads it from `RSYNC_AGENT_TOKEN` in your shell:

```json
{
  "mcpServers": {
    "rsync-ai": {
      "httpUrl": "https://rsync.example.com/api/agent/v1/mcp",
      "headers": { "Authorization": "Bearer ${RSYNC_AGENT_TOKEN}" }
    }
  }
}
```

Mistral Le Chat: Connectors → Add connector → Custom MCP, with the address above and
API key / Bearer authentication. Le Chat keeps the token, so give it a short expiry.

Clients that connect only through OAuth, such as the claude.ai and ChatGPT web
connectors, cannot use the gateway: it accepts agent tokens only.

## What a token can do

- **It acts as you.** Every tool reads through the same handlers the UI uses, as
  you, so an agent sees what you see in that workspace and never more.
- **One workspace.** A token is bound to the workspace that was open when you made
  it. `X-Workspace-ID` is ignored.
- **Reads, and writes only by scope.** Every token has `metadata:read`, which opens
  the read tools below. Each write tool needs the scope listed with it under
  [Tools](#tools): `pipelines:plan`, which any member can give, or
  `connectors:generate`, `models:write`, `workflows:write` or `dashboards:write`, which
  only an owner or admin can. Those four still need your role to be admin or owner on
  every call: demote the token's user and they stop working.
- **Checked on every call.** Each call re-checks that the token is unrevoked and
  unexpired, that your account is active and still a member of the workspace, and
  uses your role as it is at that moment. Remove or demote a member and their tokens
  follow on the next call.
- **Never a platform admin**, whatever your own account is.
- **Not a session.** The REST API under `/api/v1` does not accept it, and it cannot
  make or revoke agent tokens.
- **120 calls a minute** per token. Each request costs one, and each further
  `tools/call` in a batch one more. Past that the gateway answers 429 with
  `Retry-After`; a batch's call past it comes back as an `isError` result and
  reads nothing.

## Generate a connector

`generate_connector` takes a name and the https URL of a public API's OpenAPI or
Swagger document (`spec_url`) or documentation page (`docs_url`), and returns the
run's `run_id` at once; `get_connector_generation` reads that run until it says
`succeeded` or `failed`. The `run_id` names the generation run, not the connector:
the connector is `connector_name`. A run writes and validates the connector and adds it to the catalog, which
every workspace on the install shares; it does not start the connector. A person then
adds the credentials on the connection form, which says the connector was generated
by an AI agent. Neither tool takes or returns a credential.

It is refused, with a message the agent can pass on, when:

- the name is not lowercase kebab-case, is a built-in connector, or already exists;
- the URL is not https, carries a user name or password, or names an IP address or a
  private host;
- the generator is down, generates from an uploaded spec only (the community
  edition), or runs where a generated connector cannot be kept (each service reads
  its own catalog from the release image);
- the workspace already has 2 runs going or has started 10 in the last hour, or
  another run is generating the same name.

A run gets 15 minutes. One still marked running after 20 minutes was interrupted and
is marked failed.

## Revoke a token

Revoke a token in **Settings → Connect your AI app**; an agent using it gets 401 on its
next call. Every member sees and revokes their own tokens. Workspace admins and
owners also see every other member's tokens in the workspace, with the owner's
email, and can revoke any of them.

## What it never returns

Each tool's output type is an allowlist: a key the tool does not declare is dropped,
whatever the handler behind it returns. The keys each tool can return are listed
under [Tools](#tools). No tool returns:

- row values, sample rows, query results or row counts;
- connection settings or credentials;
- raw error text. Pipelines and runs carry an error class (auth, network, schema…);
  `diagnose_run` returns the message with quoted values, hosts, emails and
  credentials masked and every number replaced, so neither a row count nor a key
  value survives.

The gateway calls no model: `diagnose_run`'s hint is a fixed text per error class.
Names, descriptions and error messages are written by people and by source systems,
so every string is capped at 500 characters and stripped of control and invisible
characters, and the server's `initialize` instructions tell the agent to treat them
as data, not instructions.

## Audit

Every tool call writes an audit row as the token's user: action `agent_tool_call`,
`actor_kind` `agent`, the token's id, and the tool, outcome and HTTP status in its
details. Its resource is the one the call named (`pipeline_id`, `run_id`, …), a
refused call's included; a `generate_connector` call's is the run it started. A row
the tool's own handler writes, such as the saved query `create_model` makes, is
marked `agent` with the token's id too. A call the token's scope or role does not allow is recorded with outcome
`forbidden`, a batch call past the rate limit with `rate_limited`, and a call whose
request was already cancelled (the client went away) with `cancelled`; neither of
the last two reads anything. A connector generation also keeps a run row with the
workspace, the user, the token's id, the URL, and how the run ended. Creating and revoking a token write `create_agent_token` and
`revoke_agent_token`.

A request the token check refuses (see [Errors](#errors)) writes `agent_auth_refused`,
resource type `agent_token`, with the refusal code and status in its details. It is
written as the token's user, in its workspace and with its id, when the token was live,
and with neither when it was missing, unknown, revoked or expired. A caller retrying a
refusal writes one row a minute per token (or, with no live token, per client IP) and
code.

Ask rsync.ai runs the same tools in-process for the signed-in user. Its calls write
`assistant_tool_call` rows, and the rows its tools' handlers write, marked
`actor_kind` `assistant` with no token.

## Transport

`POST /api/agent/v1/mcp` serves MCP's Streamable HTTP transport in its stateless
form: each POST carries one JSON-RPC message or a batch, and the answer is a single
JSON body. There is no session id and no event stream; `GET` answers 405.

- Methods: `initialize`, `ping`, `tools/list` and `tools/call`. A notification gets
  202 and no body.
- An `MCP-Protocol-Version` header, when sent, must name a supported version (listed
  under [Tools](#tools)). `initialize` answers with the client's version when it is
  supported and the newest one otherwise.
- A body over 1 MiB is refused with 413, and a batch of more than 20 messages
  with 400.

## Errors

The token is checked before MCP sees the request. A refusal there is plain JSON,
`{"error": "<code>", "message": "..."}`, and a 401 carries
`WWW-Authenticate: Bearer`.

| Status | `error` | Why |
|---|---|---|
| 401 | `missing_agent_token` | No `Authorization: Bearer rsat_...` header. |
| 401 | `invalid_agent_token` | The token is unknown, revoked or expired. |
| 401 | `account_deactivated` | The account that made the token is deactivated. |
| 401 | `not_a_member` | That account is no longer in the token's workspace. |
| 403 | `unknown_workspace_role` | The member's role is not one rsync.ai knows. |
| 403 | `no_known_scope` | The token carries no scope this server knows. |
| 429 | `rate_limit_exceeded` | More than 120 calls in a minute; see `Retry-After`. |
| 500 | `db_error` | rsync.ai could not read the token or the membership. |
| 503 | `database_unavailable` | rsync.ai's database is not connected. |

Inside MCP, an unknown tool or unreadable params is a JSON-RPC error (`-32602`).
Anything else that goes wrong in a tool (a bad argument, a record outside the
token's workspace, a refused scope) comes back as a tool result with
`isError: true` and a fixed message the agent can act on, never the body of the
response behind it. A record that is not there, and a name passed where an id goes,
say which argument to check and where its id comes from.

## Tools


<!-- BEGIN GENERATED: agent gateway tools (go test ./internal/agentgw -run TestReferenceDocIsCurrent -update) -->
Protocol versions, newest first: `2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05`.

### `list_connections`: List connections

Lists the connections (sources and destinations) in the workspace: name, connector type, status, whether the last connection test passed and, for one queried with SQL, its SQL dialect, with the total. Never returns connection settings or credentials. Rules: read_guide('connectors').

| Argument | Type | Required | Description |
|---|---|---|---|
| `connector_type` | string | no | Only connections of this connector, by its id or the name the UI shows: postgresql, postgres, gcs, Google Cloud Storage, Amazon S3, BigQuery. At most 64 characters. |
| `type` | string | no | Only sources or only destinations. One of `source`, `destination`. Also accepts `dest`, `destinations`, `sink`, `sinks`, `sources`, `src`, `target`, `targets`, case and spacing ignored. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `connections[]`, `connections[].connector_type`, `connections[].connector_version`, `connections[].id`, `connections[].is_connected`, `connections[].is_expired`, `connections[].last_test_status`, `connections[].last_tested_at`, `connections[].name`, `connections[].slug`, `connections[].sql_dialect`, `connections[].status`, `connections[].type`, `note`, `total`, `truncated`.

### `describe_schema`: Describe a connection's schema

Lists the tables of a connection with their columns, column types, primary keys and foreign keys, from the schema index the Explorer uses. Names only: no row values, samples or row counts. Rules: read_guide('connectors').

| Argument | Type | Required | Description |
|---|---|---|---|
| `connection_id` | string (uuid) | yes | The connection's id, from list_connections. |
| `name_contains` | string | no | Only tables whose schema.table name contains this text (case-insensitive). At most 100 characters. |
| `max_tables` | integer | no | At most this many tables (1-200, default 50). |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `connection_id`, `foreign_keys[]`, `foreign_keys[].from_column`, `foreign_keys[].from_schema`, `foreign_keys[].from_table`, `foreign_keys[].inferred`, `foreign_keys[].to_column`, `foreign_keys[].to_schema`, `foreign_keys[].to_table`, `last_refreshed_at`, `tables[]`, `tables[].columns[]`, `tables[].columns[].is_nullable`, `tables[].columns[].is_primary_key`, `tables[].columns[].name`, `tables[].columns[].type`, `tables[].columns_truncated`, `tables[].name`, `tables[].primary_key[]`, `tables[].schema`, `tables_matched`, `truncated`.

### `list_pipelines`: List pipelines

Lists the workspace's pipelines with their state, source and destination, schedule and last run. Count from pagination.total (every page), not the rows. A failed run carries an error class, not its text: diagnose_run has that. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `query` | string | no | Only pipelines whose name contains this text. At most 100 characters. |
| `status` | string | no | Only pipelines in this state. passed means the last run succeeded; a pipeline's state is its last run, not its history (use list_runs). One of `running`, `passed`, `failed`, `stopped`, `idle`, `paused`, `waiting_for_user`. Also accepts `broken`, `canceled`, `cancelled`, `complete`, `completed`, `error`, `errored`, `failing`, `failure`, `in_progress`, `live`, `needs_input`, `ok`, `passing`, `processing`, `streaming`, `succeeded`, `success`, `successful`, `waiting`, case and spacing ignored. |
| `type` | string | no | Only pipelines of this type. One of `batch`, `cdc`, `etl`. |
| `page` | integer | no | Page number, from 1. |
| `per_page` | integer | no | Pipelines per page (1-100, default 25). |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `pagination`, `pagination.has_next`, `pagination.has_prev`, `pagination.page`, `pagination.per_page`, `pagination.total`, `pagination.total_pages`, `pipelines[]`, `pipelines[].cdc_mode`, `pipelines[].created_at`, `pipelines[].destination`, `pipelines[].destination.connector_type`, `pipelines[].destination.id`, `pipelines[].destination.name`, `pipelines[].id`, `pipelines[].last_run`, `pipelines[].last_run.completed_at`, `pipelines[].last_run.error_class`, `pipelines[].last_run.id`, `pipelines[].last_run.started_at`, `pipelines[].last_run.status`, `pipelines[].name`, `pipelines[].pipeline_status`, `pipelines[].schedule`, `pipelines[].schedule.cron`, `pipelines[].schedule.every_seconds`, `pipelines[].schedule.status`, `pipelines[].schedule.timezone`, `pipelines[].schedule.type`, `pipelines[].source`, `pipelines[].source.connector_type`, `pipelines[].source.id`, `pipelines[].source.name`, `pipelines[].state`, `pipelines[].sync_mode`, `pipelines[].updated_at`.

### `get_pipeline`: Get a pipeline

Returns one pipeline's definition: source and destination, sync and CDC mode, selected tables and loading strategy. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `pipeline_id` | string (uuid) | yes | The pipeline's id, from list_pipelines. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `cdc_initial_load`, `cdc_mode`, `created_at`, `data_loading_strategy`, `data_loading_strategy.effective_cdc_mode`, `data_loading_strategy.effective_sync_mode`, `data_loading_strategy.initial_load`, `data_loading_strategy.mode`, `data_loading_strategy.ongoing_sync`, `data_loading_strategy.rerun_default`, `data_loading_strategy.schedule_status`, `data_loading_strategy.schedule_type`, `default_run_mode`, `description`, `destination`, `destination.connector_type`, `destination.id`, `destination.name`, `id`, `name`, `selected_tables[]`, `selected_tables_truncated`, `source`, `source.connector_type`, `source.id`, `source.name`, `status`, `sync_mode`, `updated_at`.

### `get_pipeline_health`: Get a pipeline's health

Answers "why is this pipeline stale?": its state, the one thing blocking it (waiting for you, paused, failed with an error class, never run, no schedule…) with a fixed hint, the last run, the last success and the schedule. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `pipeline_id` | string (uuid) | yes | The pipeline's id, from list_pipelines. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `blocking`, `blocking.error_class`, `blocking.hint`, `blocking.kind`, `blocking.reason_type`, `blocking.since`, `checked_at`, `last_run`, `last_run.ended_at`, `last_run.error_class`, `last_run.id`, `last_run.started_at`, `last_run.status`, `last_success_at`, `mode`, `name`, `pipeline_id`, `schedule`, `schedule.cron`, `schedule.every_seconds`, `schedule.status`, `schedule.timezone`, `schedule.type`, `state`.

### `get_run_status`: Get a run's status

Returns one run's status and outcome, trigger, start and end times, and its error class if it failed. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `run_id` | string (uuid) | yes | The run's id: last_run.id from list_pipelines or get_pipeline_health, or runs[].run_id from list_runs. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `ended_at`, `error_class`, `live_stream`, `outcome`, `pipeline_id`, `pipeline_name`, `run_id`, `scheduled_time`, `started_at`, `status`, `trigger_source`.

### `diagnose_run`: Diagnose a failed run

Explains why a run failed: its error class, a fixed hint for that class, and the error message with quoted values, numbers, hosts, emails and credentials masked. Calls no model. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `run_id` | string (uuid) | yes | The run's id: last_run.id from list_pipelines or get_pipeline_health, or runs[].run_id from list_runs. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `error_class`, `hint`, `message`, `pipeline_id`, `retryable`, `run_id`, `status`.

### `list_runs`: List runs

Run history: how many runs succeeded, failed, were cancelled or are running (by_outcome, over every run, not just those listed) and the newest runs with their outcome and error class. Use it for "has it ever failed?"; a pipeline list shows only the last run. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `pipeline_id` | string (uuid) | no | Only this pipeline's runs; its id, from list_pipelines. |
| `status` | string | no | Only runs that ended this way. by_outcome still counts every run. One of `succeeded`, `failed`, `cancelled`, `running`, `pending`, `waiting_for_user`. Also accepts `canceled`, `complete`, `completed`, `error`, `errored`, `failure`, `in_progress`, `ok`, `passed`, `processing`, `queued`, `stopped`, `success`, `successful`, `waiting`, case and spacing ignored. |
| `limit` | integer | no | At most this many runs, newest first (1-100, default 25). |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `by_outcome`, `by_outcome.cancelled`, `by_outcome.failed`, `by_outcome.other`, `by_outcome.pending`, `by_outcome.running`, `by_outcome.succeeded`, `by_outcome.waiting_for_user`, `runs[]`, `runs[].ended_at`, `runs[].error_class`, `runs[].live_stream`, `runs[].outcome`, `runs[].pipeline_id`, `runs[].pipeline_name`, `runs[].run_id`, `runs[].scheduled_time`, `runs[].started_at`, `runs[].status`, `runs[].trigger_source`, `total`, `truncated`.

### `list_connectors`: List connectors

The connector catalog: every connector rsync.ai can connect to (built in, or made with Connectors → Generate New), and whether it works as a source, a destination or for CDC. Use it for "does rsync.ai support X?"; list_connections has only this workspace's. Rules: read_guide('connectors').

| Argument | Type | Required | Description |
|---|---|---|---|
| `name_contains` | string | no | Only connectors whose id or name contains this text (case-insensitive). At most 64 characters. |
| `supports` | string | no | Only connectors that work as a source, a destination, or for CDC. One of `source`, `destination`, `cdc`. Also accepts `change_data_capture`, `destinations`, `sources`, `streaming`, `target`, case and spacing ignored. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `connectors[]`, `connectors[].category`, `connectors[].description`, `connectors[].display_name`, `connectors[].lifecycle`, `connectors[].name`, `connectors[].origin`, `connectors[].supports_cdc`, `connectors[].supports_destination`, `connectors[].supports_source`, `connectors[].version`, `total`, `truncated`.

### `generate_connector`: Generate a connector

Generates a connector from a public API's OpenAPI or Swagger document (spec_url) or docs page (docs_url) and adds it to the catalog. Starts a background run and returns its run_id; follow it with get_connector_generation. Rules: read_guide('connectors').

| Argument | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | The new connector's id, lowercase kebab-case, such as acme-crm. At most 100 characters. |
| `spec_url` | string | no | https URL of the API's OpenAPI or Swagger document (JSON or YAML). Give this or docs_url. At most 2048 characters. |
| `docs_url` | string | no | https URL of the API's documentation page, when it has no OpenAPI document. Give this or spec_url. At most 2048 characters. |
| `description` | string | no | One line on what the API is for. At most 500 characters. |

Needs the token scope `connectors:generate` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `connector_name`, `error`, `finished_at`, `next`, `run_id`, `source_url`, `started_at`, `status`, `tool_count`, `validation_passed`, `version`.

### `get_connector_generation`: Get a connector generation

Returns one connector generation run of this workspace: running, succeeded (with the version and how many tools the connector has) or failed (with the reason). Rules: read_guide('connectors').

| Argument | Type | Required | Description |
|---|---|---|---|
| `run_id` | string (uuid) | yes | The run_id generate_connector returned. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `connector_name`, `error`, `finished_at`, `next`, `run_id`, `source_url`, `started_at`, `status`, `tool_count`, `validation_passed`, `version`.

### `list_models`: List models

Lists the workspace's models: saved queries a run writes to a table. Each has its target table, layer, schedule status and last run; layers lists the workspace's layers in order. Never returns a model's query text or a row count. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `connection_id` | string (uuid) | no | Only models on this connection; its id, from list_connections. |
| `layer` | string | no | Only models in this layer: one of the workspace's own layers, by name, in any case. At most 40 characters. |
| `name_contains` | string | no | Only models whose name contains this text (case-insensitive). At most 100 characters. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `layers[]`, `models[]`, `models[].connection_id`, `models[].connector_type`, `models[].description`, `models[].freshness_deadline_seconds`, `models[].last_run_at`, `models[].last_run_error`, `models[].last_run_status`, `models[].layer`, `models[].materialization`, `models[].model_id`, `models[].name`, `models[].next_run_at`, `models[].query_language`, `models[].schedule_status`, `models[].target_table`, `models[].updated_at`, `models[].visibility`, `note`, `total`, `truncated`.

### `get_model`: Get a model

Returns one model: its target table, layer, last run and schedule (cron, interval, or the pipelines and models it runs after), whether the schedule is paused or blocked and why, and the tables its query reads with the pipelines and models that write them. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `connection_id`, `connector_type`, `description`, `freshness_deadline_seconds`, `last_run_at`, `last_run_error`, `last_run_status`, `layer`, `materialization`, `model_id`, `name`, `next_run_at`, `query_language`, `reads`, `reads.ambiguous`, `reads.no_known_writer[]`, `reads.tables[]`, `reads.written_by[]`, `reads.written_by[].id`, `reads.written_by[].kind`, `reads.written_by[].name`, `reads.written_by[].table`, `reads_note`, `schedule`, `schedule.auto_paused_at`, `schedule.auto_paused_reason`, `schedule.blocked`, `schedule.blocked_reason`, `schedule.created_at`, `schedule.cron`, `schedule.every_seconds`, `schedule.paused_at`, `schedule.paused_reason`, `schedule.schedule_id`, `schedule.schedule_type`, `schedule.status`, `schedule.timezone`, `schedule.upstream_policy`, `schedule.upstreams[]`, `schedule.upstreams[].id`, `schedule.upstreams[].kind`, `schedule.upstreams[].name`, `schedule_status`, `target_table`, `updated_at`, `visibility`.

### `list_model_runs`: List a model's runs

A model's run history, newest first: each run's status (succeeded, failed or skipped), what triggered it (a person, its schedule, or the upstream that finished), why a skipped run did not rebuild, and a failed run's error with values masked. No row counts. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |
| `status` | string | no | Only runs that ended this way. One of `succeeded`, `failed`, `skipped`. Also accepts `completed`, `error`, `errored`, `failure`, `ok`, `passed`, `skip`, `success`, `successful`, `waiting`, case and spacing ignored. |
| `limit` | integer | no | At most this many runs, newest first (1-50, default 10). |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `model_id`, `runs[]`, `runs[].auto_pause_reason`, `runs[].coalesced_count`, `runs[].duration_ms`, `runs[].error`, `runs[].finished_at`, `runs[].run_id`, `runs[].skip_reason`, `runs[].started_at`, `runs[].status`, `runs[].target_table`, `runs[].trigger_depth`, `runs[].trigger_source`, `runs[].upstream_id`, `runs[].upstream_kind`, `runs[].upstream_name`, `truncated`.

### `create_model`: Create a model

Saves a model: one query on a connection whose result each run writes to target_table, which rsync.ai creates on the first run. For a cleaned copy of one table a pipeline writes, use create_silver_model. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `connection_id` | string (uuid) | yes | The connection the query runs on, and where target_table is written; from list_connections. |
| `name` | string | yes | The model's name, unique in the workspace. At most 200 characters. |
| `query` | string | yes | One SELECT for a SQL engine; for MongoDB, JSON such as {"collection": "orders", "pipeline": [...]}, with "database" for another one. It must only read. At most 65536 characters. |
| `target_table` | string | yes | The table each run writes, as schema.table or table (a collection on MongoDB). At most 300 characters. |
| `layer` | string | no | One of the workspace's own layers, by name (list_models lists them). Leave it out for none; a name the workspace has not made is refused. At most 40 characters. |
| `description` | string | no | One line on what the model is for. At most 500 characters. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `connection_id`, `layer`, `materialization`, `model_id`, `name`, `next`, `target_table`.

### `create_silver_model`: Create a silver model

Saves the silver model of one table a pipeline writes, with SQL rsync.ai generates from its columns (no AI): a CDC history table's latest version of each key, deleted keys dropped; a soft-delete table's live rows; else the table as it is. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `connection_id` | string (uuid) | yes | The connection holding the pipeline table: a pipeline's destination, from list_connections. |
| `table` | string | yes | The pipeline table, as schema.table or table. At most 300 characters. |
| `keys` | string | no | Comma-separated key column names. Only for a history table whose key rsync.ai cannot read from its catalog. At most 2000 characters. |
| `target_table` | string | no | The table each run writes. Defaults to the name rsync.ai suggests beside the pipeline table. At most 300 characters. |
| `name` | string | no | The model's name. Defaults to "silver <table>". At most 200 characters. |
| `description` | string | no | One line on what the model is for. At most 500 characters. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `columns[]`, `columns_truncated`, `connection_id`, `keys[]`, `layer`, `materialization`, `model_id`, `name`, `next`, `shape`, `source_table`, `target_table`.

### `schedule_model`: Schedule a model

Gives a model its one schedule, active at once: cron, interval (every_seconds, at least 60) or after_upstream (each time the listed pipelines or models finish a run). It runs as the token's user. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |
| `schedule_type` | string | yes | cron, interval, or after_upstream. One of `cron`, `interval`, `after_upstream`. Also accepts `after`, `after_model`, `after_pipeline`, `after_upstreams`, `chain`, `chained`, `crontab`, `event`, `every`, `on_upstream`, `periodic`, `repeat`, `upstream`, case and spacing ignored. |
| `cron` | string | no | For cron: a five-field cron expression, such as 0 * * * * for hourly. At most 100 characters. |
| `every_seconds` | integer | no | For interval: seconds between runs, at least 60. |
| `timezone` | string | no | For cron: an IANA timezone such as Europe/Berlin. Default UTC. At most 64 characters. |
| `after_pipelines` | string | no | For after_upstream: comma-separated pipeline ids, from list_pipelines. At most 640 characters. |
| `after_models` | string | no | For after_upstream: comma-separated model ids, from list_models. At most 640 characters. |
| `upstream_policy` | string | no | For after_upstream with several upstreams: run after any one finishes (default), or once all have. One of `any`, `all`. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `model_id`, `next`, `schedule`, `schedule.auto_paused_at`, `schedule.auto_paused_reason`, `schedule.blocked`, `schedule.blocked_reason`, `schedule.created_at`, `schedule.cron`, `schedule.every_seconds`, `schedule.paused_at`, `schedule.paused_reason`, `schedule.schedule_id`, `schedule.schedule_type`, `schedule.status`, `schedule.timezone`, `schedule.upstream_policy`, `schedule.upstreams[]`, `schedule.upstreams[].id`, `schedule.upstreams[].kind`, `schedule.upstreams[].name`.

### `run_model`: Run a model

Starts one run of a model in the background: its query runs and its result replaces target_table. Returns at once; follow the run with list_model_runs. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `model_id`, `name`, `next`, `status`, `target_table`.

### `pause_model_schedule`: Pause a model's schedule

Pauses a model's schedule: it stops running on its own until resume_model_schedule. A run in flight finishes. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |
| `reason` | string | no | Why, for the people who see the paused schedule. At most 200 characters. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `model_id`, `next`, `schedule`, `schedule.auto_paused_at`, `schedule.auto_paused_reason`, `schedule.blocked`, `schedule.blocked_reason`, `schedule.created_at`, `schedule.cron`, `schedule.every_seconds`, `schedule.paused_at`, `schedule.paused_reason`, `schedule.schedule_id`, `schedule.schedule_type`, `schedule.status`, `schedule.timezone`, `schedule.upstream_policy`, `schedule.upstreams[]`, `schedule.upstreams[].id`, `schedule.upstreams[].kind`, `schedule.upstreams[].name`.

### `resume_model_schedule`: Resume a model's schedule

Resumes a paused model schedule, including one rsync.ai paused after failed runs. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |

Needs the token scope `models:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `model_id`, `next`, `schedule`, `schedule.auto_paused_at`, `schedule.auto_paused_reason`, `schedule.blocked`, `schedule.blocked_reason`, `schedule.created_at`, `schedule.cron`, `schedule.every_seconds`, `schedule.paused_at`, `schedule.paused_reason`, `schedule.schedule_id`, `schedule.schedule_type`, `schedule.status`, `schedule.timezone`, `schedule.upstream_policy`, `schedule.upstreams[]`, `schedule.upstreams[].id`, `schedule.upstreams[].kind`, `schedule.upstreams[].name`.

### `list_model_checks`: List a model's checks

Lists a model's data checks: each one's name, type, columns, severity and whether it is on, and whether the model's engine can block at all. Never returns a check's SQL, a count or a result. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `can_block`, `cannot_block_reason`, `checks[]`, `checks[].check_id`, `checks[].columns[]`, `checks[].enabled`, `checks[].name`, `checks[].severity`, `checks[].type`, `max_checks`, `model_id`, `note`, `unsupported_reason`.

### `plan_model_checks`: Draft a model's checks

Drafts data checks for a model and validates them as a save would: types, settings, names, at most 20 a model, a custom_sql that only reads, and columns against the model's table. It saves and runs nothing. Rules: read_guide('models').

| Argument | Type | Required | Description |
|---|---|---|---|
| `model_id` | string (uuid) | yes | The model's id, from list_models or the tool that created it. |
| `checks` | array of at most 20 objects | yes | The checks to add, at most 20. |
| `checks[].name` | string | yes | A short name the person reads, unique on the model, such as order_id is unique. At most 100 characters. |
| `checks[].type` | string | yes | row_count (min to max rows), row_count_change (moved more than max_drop_pct since the last good run), not_null, unique (NULLs ignored) or custom_sql. One of `row_count`, `row_count_change`, `not_null`, `unique`, `custom_sql`. Also accepts `assertion`, `count`, `count_change`, `custom`, `distinct`, `min_rows`, `no_duplicates`, `no_nulls`, `non_null`, `not_empty`, `notnull`, `query`, `required`, `row_change`, `row_count_drop`, `rows`, `sql`, `uniqueness`, `volume`, case and spacing ignored. |
| `checks[].columns` | string | no | Comma-separated column names of the model's table: one for not_null, 1 to 10 for unique. At most 1300 characters. |
| `checks[].min` | string | no | For row_count: the fewest rows, a whole number such as "1". Default 1. At most 20 characters. |
| `checks[].max` | string | no | For row_count: the most rows, a whole number. Default no limit. At most 20 characters. |
| `checks[].max_drop_pct` | string | no | For row_count_change, required: the largest drop in percent (0 to 100), such as "50". At most 20 characters. |
| `checks[].max_growth_pct` | string | no | For row_count_change: the largest growth in percent, such as "200". Default no limit. At most 20 characters. |
| `checks[].sql` | string | no | For custom_sql: a read-only SELECT of rule-breaking rows, {{this}} for the model's table, such as SELECT * FROM {{this}} WHERE amount < 0. Any row fails it. At most 20000 characters. |
| `checks[].severity` | string | no | block keeps a failing build from replacing the live table; warn publishes it and alerts. Default block; warn for row_count_change and engines that cannot block. One of `block`, `warn`. Also accepts `alert`, `blocking`, `error`, `fail`, `hard`, `soft`, `warning`, case and spacing ignored. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `can_block`, `cannot_block_reason`, `checks[]`, `checks[].columns[]`, `checks[].name`, `checks[].severity`, `checks[].type`, `checks[].verdict`, `columns_unchecked`, `existing_checks`, `max_checks`, `model_id`, `next`, `note`, `target_table`.

### `plan_pipeline`: Propose a pipeline

Proposes a pipeline from one connection to another, for the person to create in rsync.ai. rsync.ai checks the sync mode, schedule and tables against the source and returns a plan with a url. It creates nothing and moves no data. Rules: read_guide('pipelines').

| Argument | Type | Required | Description |
|---|---|---|---|
| `source_connection_id` | string (uuid) | yes | The connection the data comes from; from list_connections. |
| `destination_connection_id` | string (uuid) | yes | The connection the data goes to; from list_connections. |
| `tables` | string | no | Comma-separated source tables, as table or schema.table, at most 100. Leave it out to let the person pick when the pipeline first runs. At most 16384 characters. |
| `sync_mode` | string | no | batch copies the tables on each run; cdc streams each change as it happens. Leave it out for the source connection's default. One of `batch`, `cdc`. Also accepts `ad_hoc`, `change_data_capture`, `full`, `full_load`, `once`, `one_off`, `one_time`, `onetime`, `periodic`, `real_time`, `realtime`, `replication`, `scheduled`, `single_run`, `snapshot`, `stream`, `streaming`, case and spacing ignored. |
| `schedule_type` | string | no | For a batch pipeline that runs on a schedule: cron or interval. Leave it out for a pipeline that runs once when created. One of `cron`, `interval`. Also accepts `crontab`, `every`, `periodic`, `repeat`, case and spacing ignored. |
| `cron` | string | no | For cron: a five-field cron expression, such as 0 * * * * for hourly. At most 100 characters. |
| `every_seconds` | integer | no | For interval: seconds between runs, at least 60. |
| `timezone` | string | no | For cron: an IANA timezone such as Europe/Berlin. Default UTC. At most 64 characters. |
| `destination_namespace` | string | no | The schema or database the data lands in. Leave it out to let rsync.ai choose. At most 200 characters. |

Needs the token scope `pipelines:plan` and the workspace role `member` or higher.

**Writes.** This tool changes rsync.ai. Any member can give a token its scope.

Returns only these keys: `cdc_mode`, `destination`, `destination.connection_id`, `destination.connector_type`, `destination.name`, `destination_namespace`, `expires_at`, `missing_tables[]`, `next`, `notes[]`, `plan_id`, `schedule`, `schedule.cron`, `schedule.description`, `schedule.every_seconds`, `schedule.timezone`, `schedule.type`, `source`, `source.connection_id`, `source.connector_type`, `source.name`, `status`, `sync_mode`, `table_choices[]`, `table_choices[].name`, `table_choices[].options[]`, `tables[]`, `tables_checked`, `url`.

### `plan_workflow`: Draft a workflow

Drafts a workflow for the person to test and turn on in rsync.ai: on a trigger, it runs one read-only SQL query on a connection and sends a message to the workspace's Slack or email channels when a condition on the result is true. Rules: read_guide('workflows').

| Argument | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | A short name, such as Weekly revenue digest. At most 200 characters. |
| `connection_id` | string (uuid) | no | The SQL connection the query runs on; from list_connections. Left out only with steps and no query. |
| `sql` | string | no | One read-only SELECT (or WITH … SELECT) in the connection's dialect, naming as a column every number the message states. Left out only with connection_id. At most 8000 characters. |
| `trigger` | string | yes | schedule runs on a cron; after_pipeline runs each time a pipeline finishes; manual runs only when the person clicks Run now. One of `schedule`, `after_pipeline`, `manual`. Also accepts `after`, `after_load`, `cron`, `daily`, `hourly`, `none`, `on_demand`, `on_pipeline`, `pipeline`, `pipeline_finished`, `run_now`, `scheduled`, `weekly`, case and spacing ignored. |
| `cron` | string | no | For schedule: five-field cron, such as 0 9 * * 1 for Mondays at 09:00. At most 100 characters. |
| `timezone` | string | no | For schedule: an IANA timezone such as Europe/Berlin. Default UTC. At most 64 characters. |
| `pipeline_id` | string (uuid) | no | For after_pipeline: the pipeline; from list_pipelines. |
| `branches` | array of at most 100 objects | yes | The conditions, top to bottom; the first that is true sends. Most workflows have one. |
| `branches[].condition` | string | no | always (the last only: a digest, or the otherwise), rows_exist (an exception list), no_rows (nothing arrived), value (a number vs threshold). Default always. One of `always`, `rows_exist`, `no_rows`, `value`. Also accepts `any_rows`, `default`, `digest`, `else`, `empty`, `every_time`, `has_rows`, `metric`, `none`, `not_empty`, `nothing`, `number`, `otherwise`, `report`, `rows`, `threshold`, `zero_rows`, case and spacing ignored. |
| `branches[].condition_column` | string | no | For value: the column to compare. Default the first column. At most 63 characters. |
| `branches[].condition_op` | string | no | For value: the comparison. One of `>`, `>=`, `<`, `<=`, `=`, `!=`. Also accepts `<>`, `==`, `above`, `at_least`, `at_most`, `below`, `eq`, `equals`, `fewer_than`, `greater_than`, `gt`, `gte`, `less_than`, `lt`, `lte`, `more_than`, `ne`, `not_equal`, case and spacing ignored. |
| `branches[].threshold` | string | no | For value: the number to compare with, such as 50 or 0.2. At most 40 characters. |
| `branches[].subject` | string | no | The email subject and the Slack message's first line. May use {{name}} slots. At most 200 characters. |
| `branches[].message` | string | yes | The message, short plain text with {{name}} slots: {{total_users}} for a column of the first row, {{table}} for the rows. It shows only what its slots name. At most 4000 characters. |
| `branches[].channels` | string | yes | Comma-separated delivery channel names, such as #finance, Finance team. Only names the workspace has; a wrong name is refused with the list. At most 600 characters. |
| `quiet_minutes` | integer | no | For an alert: after a condition sends, it does not send again for this many minutes. 0 sends every time. |
| `steps` | array of at most 20 objects | no | Steps that run before the query (or, with no query, before the message), each when the steps its after names end, by its rule. Leave it out for none. |
| `steps[].id` | string | yes | A short id for the step, lower-case letters, digits and _, such as load_orders. Another step's after names it. At most 40 characters. |
| `steps[].type` | string | yes | run_pipeline runs a batch pipeline and waits; refresh_model builds a model, runs its checks; wait pauses; chart or dashboard draws a saved one into the message. One of `run_pipeline`, `refresh_model`, `wait`, `chart`, `dashboard`. Also accepts `attach_chart`, `attach_dashboard`, `board`, `build_model`, `delay`, `draw_chart`, `draw_dashboard`, `graph`, `load`, `model`, `pause`, `pipeline`, `plot`, `rebuild_model`, `refresh`, `run`, `run_model`, `sleep`, `start_pipeline`, `sync`, case and spacing ignored. |
| `steps[].after` | string | no | Comma-separated ids of the steps this one runs after. Leave it out for a step that runs first. At most 600 characters. |
| `steps[].rule` | string | no | all_success (default): each step it is after succeeded; all_done: each ended either way; any_failed: one failed, for a cleanup or a fallback. One of `all_success`, `all_done`, `any_failed`. Also accepts `all_finished`, `all_succeeded`, `always`, `any_failure`, `done`, `failed`, `finished`, `on_failure`, `on_success`, `one_failed`, `succeeded`, `success`, case and spacing ignored. |
| `steps[].pipeline_id` | string (uuid) | no | For run_pipeline: the batch pipeline to run; from list_pipelines. |
| `steps[].model_id` | string (uuid) | no | For refresh_model: the model to build; from list_models. |
| `steps[].chart_id` | string (uuid) | no | For chart: the saved chart to draw, a chart tile's chart_id from get_dashboard. Drawn as the person who turns the workflow on, so one they see. |
| `steps[].dashboard_id` | string (uuid) | no | For dashboard: the dashboard to draw; from list_dashboards. It is drawn as the person who turns the workflow on, so it must be a dashboard they see. |
| `steps[].minutes` | integer | no | For wait: how long to wait, 1 to 1440 minutes (24 hours). |
| `steps[].timeout_minutes` | integer | no | For run_pipeline and refresh_model: how long the step waits for its run, retries included, before it stops the run and fails. Default 720 (12 hours). |
| `steps[].retries` | integer | no | For run_pipeline: how many times to start the pipeline again when its run fails, 0 to 3. Default 0. A model is never retried. |
| `steps_rule` | string | no | With steps: when the query (or, with no query, the message) runs after the last steps: all_success (default), all_done or any_failed, as for a step's rule. One of `all_success`, `all_done`, `any_failed`. Also accepts `all_finished`, `all_succeeded`, `always`, `any_failure`, `done`, `failed`, `finished`, `on_failure`, `on_success`, `one_failed`, `succeeded`, `success`, case and spacing ignored. |

Needs the token scope `workflows:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `branches[]`, `branches[].channels[]`, `branches[].number`, `branches[].when`, `check`, `connection`, `connection.connector_type`, `connection.id`, `connection.name`, `name`, `next`, `pipeline`, `pipeline.connector_type`, `pipeline.id`, `pipeline.name`, `status`, `steps[]`, `steps[].after[]`, `steps[].chart`, `steps[].chart.connector_type`, `steps[].chart.id`, `steps[].chart.name`, `steps[].dashboard`, `steps[].dashboard.connector_type`, `steps[].dashboard.id`, `steps[].dashboard.name`, `steps[].id`, `steps[].minutes`, `steps[].model`, `steps[].model.connector_type`, `steps[].model.id`, `steps[].model.name`, `steps[].pipeline`, `steps[].pipeline.connector_type`, `steps[].pipeline.id`, `steps[].pipeline.name`, `steps[].retries`, `steps[].rule`, `steps[].timeout_minutes`, `steps[].type`, `steps_rule`, `trigger`, `url`, `workflow_id`.

### `list_workflows`: List workflows

Lists the workspace's workflows, newest first: each one's id, name, status, trigger, connection, steps, and its conditions top to bottom with when each sends and where. Never shows a workflow's SQL or messages. Rules: read_guide('workflows').

No arguments.

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `truncated`, `workflows[]`, `workflows[].branches[]`, `workflows[].branches[].channels[]`, `workflows[].branches[].number`, `workflows[].branches[].when`, `workflows[].connection`, `workflows[].connection.connector_type`, `workflows[].connection.id`, `workflows[].connection.name`, `workflows[].id`, `workflows[].name`, `workflows[].next_run_at`, `workflows[].pipeline`, `workflows[].pipeline.connector_type`, `workflows[].pipeline.id`, `workflows[].pipeline.name`, `workflows[].status`, `workflows[].steps[]`, `workflows[].steps[].after[]`, `workflows[].steps[].chart`, `workflows[].steps[].chart.connector_type`, `workflows[].steps[].chart.id`, `workflows[].steps[].chart.name`, `workflows[].steps[].dashboard`, `workflows[].steps[].dashboard.connector_type`, `workflows[].steps[].dashboard.id`, `workflows[].steps[].dashboard.name`, `workflows[].steps[].id`, `workflows[].steps[].minutes`, `workflows[].steps[].model`, `workflows[].steps[].model.connector_type`, `workflows[].steps[].model.id`, `workflows[].steps[].model.name`, `workflows[].steps[].pipeline`, `workflows[].steps[].pipeline.connector_type`, `workflows[].steps[].pipeline.id`, `workflows[].steps[].pipeline.name`, `workflows[].steps[].retries`, `workflows[].steps[].rule`, `workflows[].steps[].timeout_minutes`, `workflows[].steps[].type`, `workflows[].steps_rule`, `workflows[].trigger`, `workflows[].url`.

### `plan_workflow_change`: Propose a change to a workflow's conditions

Proposes a change to an existing workflow's conditions, for the person to apply in rsync.ai: add, remove or reorder conditions, or change one's test, threshold, message or channels. It never edits the query, trigger, schedule or steps. Rules: read_guide('workflows').

| Argument | Type | Required | Description |
|---|---|---|---|
| `workflow_id` | string (uuid) | yes | The workflow to change; from list_workflows. |
| `branches` | array of at most 100 objects | yes | Every condition the workflow should have after the change, top to bottom. |
| `branches[].keep` | integer | no | The condition's number (from list_workflows) to start from: its test, message and channels; a field you also set replaces that part. Out for a new one. |
| `branches[].condition` | string | no | always (the last only: a digest, or the otherwise), rows_exist (an exception list), no_rows (nothing arrived), value (a number vs threshold). Default always. One of `always`, `rows_exist`, `no_rows`, `value`. Also accepts `any_rows`, `default`, `digest`, `else`, `empty`, `every_time`, `has_rows`, `metric`, `none`, `not_empty`, `nothing`, `number`, `otherwise`, `report`, `rows`, `threshold`, `zero_rows`, case and spacing ignored. |
| `branches[].condition_column` | string | no | For value: the column to compare. Default the first column. At most 63 characters. |
| `branches[].condition_op` | string | no | For value: the comparison. One of `>`, `>=`, `<`, `<=`, `=`, `!=`. Also accepts `<>`, `==`, `above`, `at_least`, `at_most`, `below`, `eq`, `equals`, `fewer_than`, `greater_than`, `gt`, `gte`, `less_than`, `lt`, `lte`, `more_than`, `ne`, `not_equal`, case and spacing ignored. |
| `branches[].threshold` | string | no | For value: the number to compare with, such as 50 or 0.2. At most 40 characters. |
| `branches[].subject` | string | no | The email subject and the Slack message's first line. May use {{name}} slots. At most 200 characters. |
| `branches[].message` | string | no | The message, short plain text with {{name}} slots: {{total_users}} for a column of the first row, {{table}} for the rows. It shows only what its slots name. At most 4000 characters. |
| `branches[].channels` | string | no | Comma-separated delivery channel names, such as #finance, Finance team. Only names the workspace has; a wrong name is refused with the list. At most 600 characters. |
| `summary` | string | no | One sentence for the person: what the change does, such as Adds a Slack alert above 100 failures. At most 300 characters. |

Needs the token scope `workflows:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `branches[]`, `branches[].channels[]`, `branches[].number`, `branches[].when`, `change_id`, `check`, `next`, `summary`, `url`, `workflow_id`, `workflow_name`.

### `list_chart_sources`: List what can be charted

Lists the models and saved queries the person may chart: each one's source_id, name, kind (model or saved query), connection, and whether a chart can read it now, with the reason when not. Rules: read_guide('dashboards').

No arguments.

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `sources[]`, `sources[].chartable`, `sources[].connection`, `sources[].connector_type`, `sources[].kind`, `sources[].name`, `sources[].reason`, `sources[].source_id`, `truncated`.

### `describe_chart_source`: Describe a chart source's columns

Lists a model's or saved query's columns: each one's name, kind (number, time, text, bool or unknown) and whether it is masked. rsync.ai runs the source once, one row, to read them; the row is never shown. Rules: read_guide('dashboards').

| Argument | Type | Required | Description |
|---|---|---|---|
| `source_id` | string (uuid) | yes | The model or saved query; from list_chart_sources. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `columns[]`, `columns[].kind`, `columns[].masked`, `columns[].name`, `kind`, `name`, `source_id`.

### `plan_dashboard`: Draft a dashboard

Drafts a dashboard for the person to review and save in rsync.ai: up to 12 charts, each drawing one model or saved query from list_chart_sources, and up to 4 filters. rsync.ai checks each chart as a save would. Rules: read_guide('dashboards').

| Argument | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | The dashboard's name, such as Sales overview. At most 200 characters. |
| `description` | string | no | One or two sentences on what it is for. At most 1000 characters. |
| `charts` | array of at most 12 objects | yes | The charts, in the order they are laid out. |
| `charts[].name` | string | yes | The chart's name, such as Orders per day. At most 200 characters. |
| `charts[].source_id` | string (uuid) | yes | The model or saved query it draws; from list_chart_sources. |
| `charts[].type` | string | yes | The kind of chart; the guide lists what each type takes. One of `bar`, `hbar`, `line`, `area`, `pie`, `donut`, `waterfall`, `funnel`, `histogram`, `heatmap`, `scatter`, `bubble`, `scorecard`, `table`, `pivot`, `treemap`, `sunburst`, `sankey`, `calendar`, `bullet`, `boxplot`, `candlestick`, `range`, `map`. Also accepts `area_chart`, `band`, `bands`, `bar_chart`, `big_number`, `bins`, `box_and_whisker`, `box_plot`, `bridge`, `bubble_chart`, `bubbles`, `bullet_chart`, `calendar_heatmap`, `candle`, `candles`, `choropleth`, `column`, `columns`, `contribution`, `contributions`, `conversion`, `correlation`, `country_map`, `cross_tab`, `crosstab`, `distribution`, `donut_chart`, `doughnut`, `error_band`, `filled_map`, `flow`, `flows`, `funnel_chart`, `geo`, `goal`, `grid`, `heat_map`, `histogram_chart`, `horizontal`, `horizontal_bar`, `kpi`, `line_chart`, `list`, `matrix`, `matrix_table`, `metric`, `min_max`, `number`, `ohlc`, `pie_chart`, `pivot_table`, `points`, `quartiles`, `range_band`, `ring`, `rings`, `row`, `sankey_diagram`, `scatter_chart`, `scatter_plot`, `share`, `stacked_area`, `stages`, `stat`, `state_map`, `stock`, `sunburst_chart`, `target`, `time_series`, `tree_map`, `treemap_chart`, `trend`, `us_map`, `waterfall_chart`, `whisker`, `world_map`, `xy`, case and spacing ignored. |
| `charts[].x` | string | no | The x column: a heatmap's columns, waterfall's steps, funnel's stages, histogram's numbers, sankey's sources, calendar's dates, box plot's groups, map's codes. At most 128 characters. |
| `charts[].grain` | string | no | Buckets a date x. One of `day`, `week`, `month`, `quarter`, `year`. |
| `charts[].y` | string | no | Heatmap: the column down its rows; scatter, bubble: the number up the y axis; sankey: the targets; boxplot: its number; candlestick: open, high, low, close. At most 128 characters. |
| `charts[].aggregate` | string | no | How the measure is computed: count (rows), count_distinct, sum, avg, min, max. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `charts[].measure_column` | string | no | The column measured; left out for count. At most 128 characters. |
| `charts[].measure_title` | string | no | What the measure is called on the chart, such as Revenue. At most 120 characters. |
| `charts[].number_format` | string | no | How the measure's numbers are written; percent for a share such as a rate (0.081 is 8.1%). One of `integer`, `decimal`, `compact`, `percent`. |
| `charts[].series` | string | no | bar, hbar, line, area: a series per value of this column; pie, donut: the slices; scatter, bubble: the colour. Seven are drawn, the rest fold into Other. At most 128 characters. |
| `charts[].size` | string | no | Bubble, or a scatter: the number that sizes each point. At most 128 characters. |
| `charts[].stack` | string | no | bar, hbar, area with a series: stack the series, or stack them to 100% (count or sum). Default not_stacked. One of `stacked`, `not_stacked`, `percent`. Also accepts `100`, `100_percent`, `false`, `full`, `grouped`, `no`, `percent_stacked`, `side_by_side`, `true`, `yes`, case and spacing ignored. |
| `charts[].second_aggregate` | string | no | bar, line, area with no series: a second measure on its own axis (a combo); pivot: its second; bullet: the target; range: the high. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `charts[].second_measure_column` | string | no | The second measure's column. At most 128 characters. |
| `charts[].second_measure_title` | string | no | The second measure's axis title. At most 120 characters. |
| `charts[].second_mark` | string | no | Default line. One of `bar`, `line`, `area`. |
| `charts[].second_axis` | string | no | Default right; left shares the first measure's scale. One of `left`, `right`. |
| `charts[].third_aggregate` | string | no | Pivot only: a third measure. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `charts[].third_measure_column` | string | no | The third measure's column. At most 128 characters. |
| `charts[].third_measure_title` | string | no | The third measure's title. At most 120 characters. |
| `charts[].bins` | integer | no | Histogram only: 1 to 100 equal bins from the least value to the greatest; default 20. |
| `charts[].bin_width` | string | no | Histogram only, instead of bins: each bin's width, such as 25 or 0.5. At most 32 characters. |
| `charts[].target` | string | no | Bullet only, instead of a second measure: the target number, such as 1000. At most 32 characters. |
| `charts[].map` | string | no | Map only: world, x a country's ISO code (FR, FRA or 250), or us_states, x a state's (US-CA or CA). A name is not read. One of `world`, `us_states`. Also accepts `countries`, `country`, `states`, `united_states`, `us`, `us_state`, `usa`, `world_countries`, case and spacing ignored. |
| `charts[].row_fields` | string | no | Pivot: its row fields; treemap, sunburst: its levels, outermost first. Comma-separated, 1 to 3. At most 400 characters. |
| `charts[].columns` | string | no | Table: the columns shown, comma-separated, at most 20; pivot: its column fields, at most 2. At most 2000 characters. |
| `charts[].sort` | string | no | x_asc or x_desc orders by the x column (by a table's first); value_asc or value_desc by the measure. One of `x_asc`, `x_desc`, `value_asc`, `value_desc`. |
| `charts[].compare_column` | string | no | Scorecard only: a date column, to compare the last compare_days days with the days before. At most 128 characters. |
| `charts[].compare_days` | integer | no | Scorecard only, with compare_column: 1, 7, 30, 90 or 365. |
| `charts[].sparkline` | string | no | Scorecard compared over 7 days or more: yes draws the period's trend under the number. One of `yes`, `no`. Also accepts `false`, `hide`, `off`, `on`, `show`, `true`, case and spacing ignored. |
| `charts[].description` | string | no | One sentence on what the chart shows. At most 500 characters. |
| `filters` | array of at most 4 objects | no | Filters above the charts, such as a date range on order_date. |
| `filters[].label` | string | yes | What the person sees, such as Order date or Region. At most 60 characters. |
| `filters[].kind` | string | yes | date_range filters a date column to a period; values to the values the person picks. One of `date_range`, `values`. Also accepts `category`, `date`, `date_range_filter`, `dates`, `dropdown`, `list`, `period`, `select`, `time`, `value`, case and spacing ignored. |
| `filters[].column` | string | yes | The column it filters, by name; it applies to each chart whose source has that column. At most 128 characters. |

Needs the token scope `dashboards:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `charts[]`, `charts[].name`, `charts[].source_id`, `charts[].source_name`, `charts[].type`, `check`, `expires_at`, `filters[]`, `filters[].column`, `filters[].id`, `filters[].kind`, `filters[].label`, `name`, `next`, `proposal_id`, `tile_count`, `url`.

### `list_dashboards`: List dashboards

Lists the dashboards the person may see, the most recently saved first: each one's dashboard_id, name, who sees it (private or workspace) and how many tiles it has. Use it to find a dashboard the person names; get_dashboard gives its tiles. Rules: read_guide('dashboards').

No arguments.

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `dashboards[]`, `dashboards[].dashboard_id`, `dashboards[].name`, `dashboards[].tile_count`, `dashboards[].updated_at`, `dashboards[].visibility`, `truncated`.

### `get_dashboard`: Read a dashboard's tiles

Reads one dashboard: its filters and each tile's tile_id, kind (chart or text) and place on the 12-column grid (x, y, w, h), and each chart's name, source and settings in plan_dashboard's words. It shows no data. Rules: read_guide('dashboards').

| Argument | Type | Required | Description |
|---|---|---|---|
| `dashboard_id` | string (uuid) | yes | The dashboard; from list_dashboards. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `dashboard_id`, `description`, `filters[]`, `filters[].column`, `filters[].id`, `filters[].kind`, `filters[].label`, `name`, `tiles[]`, `tiles[].chart`, `tiles[].chart.aggregate`, `tiles[].chart.bin_width`, `tiles[].chart.bins`, `tiles[].chart.color`, `tiles[].chart.columns`, `tiles[].chart.compare_column`, `tiles[].chart.compare_days`, `tiles[].chart.description`, `tiles[].chart.grain`, `tiles[].chart.map`, `tiles[].chart.measure_column`, `tiles[].chart.measure_title`, `tiles[].chart.number_format`, `tiles[].chart.row_fields`, `tiles[].chart.second_aggregate`, `tiles[].chart.second_axis`, `tiles[].chart.second_mark`, `tiles[].chart.second_measure_column`, `tiles[].chart.second_measure_title`, `tiles[].chart.series`, `tiles[].chart.size`, `tiles[].chart.sort`, `tiles[].chart.sparkline`, `tiles[].chart.stack`, `tiles[].chart.target`, `tiles[].chart.third_aggregate`, `tiles[].chart.third_measure_column`, `tiles[].chart.third_measure_title`, `tiles[].chart.type`, `tiles[].chart.x`, `tiles[].chart.y`, `tiles[].chart_id`, `tiles[].chart_name`, `tiles[].chart_state`, `tiles[].h`, `tiles[].kind`, `tiles[].shared_elsewhere`, `tiles[].source_id`, `tiles[].source_name`, `tiles[].tile_id`, `tiles[].w`, `tiles[].x`, `tiles[].y`, `updated_at`, `url`, `visibility`.

### `plan_dashboard_change`: Propose a change to one dashboard tile

Proposes a change to one chart tile of a dashboard, for the person to apply in rsync.ai: its chart's type, columns, measure, sort, colour or name, or the tile's width or height. rsync.ai checks it as a save would. Rules: read_guide('dashboards').

| Argument | Type | Required | Description |
|---|---|---|---|
| `dashboard_id` | string (uuid) | yes | The dashboard; from list_dashboards. |
| `tile_id` | string (uuid) | yes | The chart tile to change; from get_dashboard. |
| `name` | string | no | The chart's new name; left out, it keeps its name. At most 200 characters. |
| `type` | string | no | Send it to change the chart, with every setting it should have after; left out, the settings stay. The types are plan_dashboard's. One of `bar`, `hbar`, `line`, `area`, `pie`, `donut`, `waterfall`, `funnel`, `histogram`, `heatmap`, `scatter`, `bubble`, `scorecard`, `table`, `pivot`, `treemap`, `sunburst`, `sankey`, `calendar`, `bullet`, `boxplot`, `candlestick`, `range`, `map`. Also accepts `area_chart`, `band`, `bands`, `bar_chart`, `big_number`, `bins`, `box_and_whisker`, `box_plot`, `bridge`, `bubble_chart`, `bubbles`, `bullet_chart`, `calendar_heatmap`, `candle`, `candles`, `choropleth`, `column`, `columns`, `contribution`, `contributions`, `conversion`, `correlation`, `country_map`, `cross_tab`, `crosstab`, `distribution`, `donut_chart`, `doughnut`, `error_band`, `filled_map`, `flow`, `flows`, `funnel_chart`, `geo`, `goal`, `grid`, `heat_map`, `histogram_chart`, `horizontal`, `horizontal_bar`, `kpi`, `line_chart`, `list`, `matrix`, `matrix_table`, `metric`, `min_max`, `number`, `ohlc`, `pie_chart`, `pivot_table`, `points`, `quartiles`, `range_band`, `ring`, `rings`, `row`, `sankey_diagram`, `scatter_chart`, `scatter_plot`, `share`, `stacked_area`, `stages`, `stat`, `state_map`, `stock`, `sunburst_chart`, `target`, `time_series`, `tree_map`, `treemap_chart`, `trend`, `us_map`, `waterfall_chart`, `whisker`, `world_map`, `xy`, case and spacing ignored. |
| `x` | string | no | The x column: a heatmap's columns, waterfall's steps, funnel's stages, histogram's numbers, sankey's sources, calendar's dates, box plot's groups, map's codes. At most 128 characters. |
| `grain` | string | no | Buckets a date x. One of `day`, `week`, `month`, `quarter`, `year`. |
| `y` | string | no | Heatmap: the column down its rows; scatter, bubble: the number up the y axis; sankey: the targets; boxplot: its number; candlestick: open, high, low, close. At most 128 characters. |
| `aggregate` | string | no | How the measure is computed: count (rows), count_distinct, sum, avg, min, max. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `measure_column` | string | no | The column measured; left out for count. At most 128 characters. |
| `measure_title` | string | no | What the measure is called on the chart, such as Revenue. At most 120 characters. |
| `number_format` | string | no | How the measure's numbers are written; percent for a share such as a rate (0.081 is 8.1%). One of `integer`, `decimal`, `compact`, `percent`. |
| `series` | string | no | bar, hbar, line, area: a series per value of this column; pie, donut: the slices; scatter, bubble: the colour. Seven are drawn, the rest fold into Other. At most 128 characters. |
| `size` | string | no | Bubble, or a scatter: the number that sizes each point. At most 128 characters. |
| `stack` | string | no | bar, hbar, area with a series: stack the series, or stack them to 100% (count or sum). Default not_stacked. One of `stacked`, `not_stacked`, `percent`. Also accepts `100`, `100_percent`, `false`, `full`, `grouped`, `no`, `percent_stacked`, `side_by_side`, `true`, `yes`, case and spacing ignored. |
| `second_aggregate` | string | no | bar, line, area with no series: a second measure on its own axis (a combo); pivot: its second; bullet: the target; range: the high. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `second_measure_column` | string | no | The second measure's column. At most 128 characters. |
| `second_measure_title` | string | no | The second measure's axis title. At most 120 characters. |
| `second_mark` | string | no | Default line. One of `bar`, `line`, `area`. |
| `second_axis` | string | no | Default right; left shares the first measure's scale. One of `left`, `right`. |
| `third_aggregate` | string | no | Pivot only: a third measure. One of `count`, `count_distinct`, `sum`, `avg`, `min`, `max`. Also accepts `average`, `count_unique`, `distinct`, `maximum`, `mean`, `minimum`, `total`, `unique`, case and spacing ignored. |
| `third_measure_column` | string | no | The third measure's column. At most 128 characters. |
| `third_measure_title` | string | no | The third measure's title. At most 120 characters. |
| `bins` | integer | no | Histogram only: 1 to 100 equal bins from the least value to the greatest; default 20. |
| `bin_width` | string | no | Histogram only, instead of bins: each bin's width, such as 25 or 0.5. At most 32 characters. |
| `target` | string | no | Bullet only, instead of a second measure: the target number, such as 1000. At most 32 characters. |
| `map` | string | no | Map only: world, x a country's ISO code (FR, FRA or 250), or us_states, x a state's (US-CA or CA). A name is not read. One of `world`, `us_states`. Also accepts `countries`, `country`, `states`, `united_states`, `us`, `us_state`, `usa`, `world_countries`, case and spacing ignored. |
| `row_fields` | string | no | Pivot: its row fields; treemap, sunburst: its levels, outermost first. Comma-separated, 1 to 3. At most 400 characters. |
| `columns` | string | no | Table: the columns shown, comma-separated, at most 20; pivot: its column fields, at most 2. At most 2000 characters. |
| `sort` | string | no | x_asc or x_desc orders by the x column (by a table's first); value_asc or value_desc by the measure. One of `x_asc`, `x_desc`, `value_asc`, `value_desc`. |
| `compare_column` | string | no | Scorecard only: a date column, to compare the last compare_days days with the days before. At most 128 characters. |
| `compare_days` | integer | no | Scorecard only, with compare_column: 1, 7, 30, 90 or 365. |
| `sparkline` | string | no | Scorecard compared over 7 days or more: yes draws the period's trend under the number. One of `yes`, `no`. Also accepts `false`, `hide`, `off`, `on`, `show`, `true`, case and spacing ignored. |
| `description` | string | no | One sentence on what the chart shows. At most 500 characters. |
| `color` | string | no | The measure's colour, for a chart with no series: series-1 blue, series-2 orange, series-3 sky, series-4 ochre, series-5 pink, series-6 violet, series-7 green. One of `series-1`, `series-2`, `series-3`, `series-4`, `series-5`, `series-6`, `series-7`. Also accepts `blue`, `cyan`, `gold`, `green`, `light_blue`, `ochre`, `orange`, `pink`, `purple`, `series_1`, `series_2`, `series_3`, `series_4`, `series_5`, `series_6`, `series_7`, `sky`, `sky_blue`, `violet`, `yellow`, case and spacing ignored. |
| `width` | integer | no | The tile's new width in columns, 2 to 12 (12 is the whole row); 0 keeps it. |
| `height` | integer | no | The tile's new height in rows, 2 to 24 (a scorecard at least 3); 0 keeps it. |
| `summary` | string | yes | One sentence for the person: what the change does, such as Shows revenue by week as a line. At most 300 characters. |

Needs the token scope `dashboards:write` and the workspace role `admin` or higher.

**Writes.** This tool changes rsync.ai. Only a workspace owner or admin can give a token its scope.

Returns only these keys: `after`, `after.chart_name`, `after.h`, `after.type`, `after.w`, `before`, `before.chart_name`, `before.h`, `before.type`, `before.w`, `change_id`, `check`, `copy`, `dashboard_id`, `dashboard_name`, `expires_at`, `moved`, `next`, `shared_elsewhere`, `summary`, `tile_id`, `url`.

### `get_usage`: Get the plan and its usage

The workspace's plan: its name, its pipeline limit and pipelines used, its NL→SQL query limit and queries used this month, and how many members the workspace has. A null limit is unlimited. Counts only, no member's e-mail. Rules: read_guide('pipelines').

No arguments.

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `member_count`, `nl_sql_queries_used`, `nl_sql_query_limit`, `pipeline_limit`, `pipelines_used`, `plan`.

### `read_guide`: Read a guide

rsync.ai's rules for one area: how it works and what a good draft looks like. Each tool's description names its guide; read it before you draft or answer in that area.

| Argument | Type | Required | Description |
|---|---|---|---|
| `topic` | string | yes | The area. One of `workflows`, `dashboards`, `pipelines`, `models`, `connectors`. |

Needs the token scope `metadata:read` and the workspace role `viewer` or higher.

Returns only these keys: `rules[]`, `topic`.

<!-- END GENERATED: agent gateway tools -->
