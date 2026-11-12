# What you get when you self-host

This page answers one question: **if I run rsync.ai on my own server or cluster, what
works, what needs me to bring something, and what is not there?**

Every claim below is cited to the file that decides it. There is **no edition gate** in
the code — self-host and the hosted service run the *same* api-gateway, orchestrator,
adapter and frontend images. The boundary is drawn in three places only:

| Mechanism | Where | What it decides |
|---|---|---|
| Which compose file you run | `docker-compose.quickstart.yml` (22 services) vs `docker-compose.yml` (27) | which containers exist |
| Which image artifact ships | `llm-service/oss-strip-list.txt` (20 entries) | which Python packages are inside the image |
| Feature flags, defaulting to the hosted behavior | `api-gateway/internal/config/features.go`, `plan_quota.go:70` | runtime behavior |

There is no `if edition == …` branch anywhere. `RSYNC_EDITION` is cosmetic and has zero
functional reads.

## A. Works self-hosted, no extra setup

Everything the product is for. These are on by default in
`docker-compose.quickstart.yml`.

| Capability | Notes |
|---|---|
| Batch pipelines (source → destination) | full executor, Temporal-backed |
| **CDC / streaming replication** | 5 source families: PostgreSQL, MySQL, SQL Server, Oracle, MongoDB — see [connector reference](../connectors/reference.md#cdc-prerequisites) |
| The 21-connector catalogue | databases, warehouses, object storage, SaaS APIs — [full table](../connectors/reference.md#catalogue) |
| Schema drift detection | `RSYNC_SCHEMA_DRIFT_ENABLED`, off by default in both editions |
| CDC self-heal / sentinel | detection always on; the two autonomy flags default off in both editions |
| Data explorer + NL→SQL | raw SQL needs nothing; NL→SQL needs an LLM provider — see section B |
| SQL models, lineage, charts, dashboards, workflows, alerts | no flag; on in every edition |
| Ask rsync.ai (the in-app assistant) | on by default (`RSYNC_ASSISTANT_ENABLED`); needs a model key — see section B. It never sends a row value to the model on a self-hosted install — see the last section |
| MCP agent gateway (`/api/agent/v1/mcp`, agent tokens) | on by default (`RSYNC_AGENT_GATEWAY_ENABLED`) |
| Connector generation from an uploaded OpenAPI 3.x or Swagger 2.0 spec | no model and no outbound request — see below |
| Workspaces, members, roles, invites | **one workspace per install** (`RSYNC_SINGLE_WORKSPACE`): the first sign-up owns it, later users join it by invite, and a second workspace is refused — see the last section |
| Direct SQL queries | unlimited and **not metered** in any edition |
| Unlimited pipelines | see the quota section below |
| Bring-your-own Postgres | `docker-compose.byo-postgres.yml`, full TLS (`POSTGRES_TLS_ENABLED`, `SQL_TLS_CA_FILE`) |
| Bring-your-own Kafka | `docker-compose.byo-kafka.yml`, SASL/TLS |
| Kubernetes | `deploy/helm/rsync-ai` — AKS values at `values-aks.yaml` |

## B. Works, but you supply the credential or endpoint

Nothing here is disabled. The stack needs a value only you can provide.

| Capability | You supply | Where |
|---|---|---|
| Ask rsync.ai | a Claude, ChatGPT or Gemini key | `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `GEMINI_API_KEY` on `llm-service`, or **Admin > Settings > AI models**, where a key overrides the environment's. With none, Ask answers `model_not_configured` (`api-gateway/internal/assistant/http.go:112`) |
| LLM features (explorer, NL→SQL, planner) | an API key, **or** a local model — optional: with neither, these features answer `Set up an LLM first` and the rest of the stack works | `LLM_PROVIDER` (default `openai`; `none` to run without one), `AZURE_OPENAI_ENDPOINT`, `GROQ_API_KEY` with `LLM_PROVIDER=groq`, `OPENAI_BASE_URL` for Vertex AI or another OpenAI-compatible endpoint, or `docker-compose.ollama.yml` with `LLM_PROVIDER=ollama` for a fully offline stack. Order: [which LLM is used](self-hosting.md#which-llm-is-used) |
| OAuth connectors (Google, GitHub, Slack, Salesforce, HubSpot) | **your own** client ID + secret, registered with that vendor | `${GOOGLE_CLIENT_ID:-}` etc. in `docker-compose.quickstart.yml` — all default empty, so the stack starts without them |
| Outbound email (invites, notifications) | SMTP or Resend credentials | unset in quickstart; invites render a URL but do not send |
| Object-storage destinations (S3, GCS, Azure Blob) | your bucket + credentials | [cloud-storage-config.md](../connectors/cloud-storage-config.md) |

**On vendor credentials:** self-hosting means you register your own OAuth app with each
vendor. The hosted service's client IDs are not shipped and would not work for your
domain anyway, since the redirect URI is bound to the host.

## C. Not in a self-hosted stack

Short list, and none of it is product function withheld on purpose — it is the hosted
service's own operational plumbing.

| Absent | Why | Decided at |
|---|---|---|
| **AI connector generation** (a connector from a docs URL, API prose or a GraphQL schema) | the agentic generator is stripped from the image; generating from an uploaded OpenAPI spec stays | `llm-service/oss-strip-list.txt` |
| Monitoring UI's Infrastructure and Traces tabs | Infrastructure is an admin view of the platform's own workers across every workspace; Traces needs an observability backend, and this repo ships none | `features.go:85-88` — Infrastructure defaults `false`; Traces defaults to `isDevelopment()`, which quickstart makes false by pinning `ENVIRONMENT=production`. The pipeline page's Overview tiles stay on (`:70`) |
| OpenTelemetry export and log shipping (`fluent-bit`) | neither default stack runs a collector or a log shipper, so `OTEL_ENABLED` defaults to `false` and `docker logs` is the log path; point `OTEL_EXPORTER_OTLP_ENDPOINT` at your own collector and set `OTEL_ENABLED=true` to export | `docker-compose.yml`, `docker-compose.quickstart.yml` |
| Avro `schema-registry`, Temporal web UI + admin tools, MinIO lifecycle init, Docker socket proxy, connector FS init | hosted-only plumbing | services quickstart omits |
| Internal connectors as pipeline endpoints (MinIO, Debezium, Kafka sink) | blocked by default in **both** editions; the hosted compose opts in with `RSYNC_ALLOW_INTERNAL_CONNECTORS=true` | `connections.go:59-63` |
| Billing and plan quotas | `RSYNC_BILLING_ENFORCED=false` | `docker-compose.quickstart.yml:1520` |
| The Usage panel (`/usage`, `/admin/usage`) | it reports plan, quota and trial numbers that a deployment enforcing no plans does not have; the flag defaults to whatever billing does, so it switches itself off | `features.go` → `resolveUsagePanel()`; set `FEATURE_USAGE_PANEL=true` to show it anyway |

### Connector generation: what exactly is missing

Only the AI half. Generating from an uploaded spec works.

In a self-hosted stack the container named `tool-generator` runs a **different program**:
`ghcr.io/rsync-ai/connector-lifecycle` with entrypoint `python -m src.lifecycle.main`,
not `src.agents.tool_generator.service`. It serves these routes:

| Route | Self-hosted |
|---|---|
| `GET /health` | ✅ |
| `GET /version` | ✅ |
| `POST /v1/deploy` (build + start a connector container) | ✅ — requires `X-Internal-Secret` |
| `POST /v1/generate` | ✅ deterministic: an OpenAPI 3.x or Swagger 2.0 document in, a connector out, with no model call and no outbound request. A spec URL, docs, GraphQL or a discovery session is refused with a 400 that says why (`llm-service/src/lifecycle/scaffold_routes.py`) |

`POST /v1/deploy` is deliberately kept: the Go data plane calls it on every self-heal and
just-in-time connector build, so stripping it would break CDC recovery. Only the
*generation* half is absent. (A second, separate service also serves a deploy route —
`connector-deployer:5011`, `POST /v1/deploy` and `/v1/undeploy`, the BuildKit-backed
builder. It ships self-hosted too.)

The api-gateway route `POST /api/v1/connectors/generate`
(`api-gateway/cmd/server/main.go:1209`) proxies to it, so the **/connectors/generate** page takes
an uploaded spec. Ask rsync.ai's `generate_connector` tool, which generates from a URL, refuses on
this image with `upload_spec_required` and points at that page
(`api-gateway/internal/handlers/connector_generations.go:190-192`). For an API with no OpenAPI
spec, write the connector by hand: see the
[connector developer guide](../connectors/developer-guide.md).

## Quotas and limits

**Self-hosted, every quota is off.** Two independent reasons, either sufficient:

1. `RSYNC_BILLING_ENFORCED=false` in `docker-compose.quickstart.yml:1520` →
   `billingEnforced()` returns false → `resolvePlanQuota` returns `unlimitedQuota`
   (`billingEnforced()` at `plan_quota.go:70`, the early return at `:84`).
2. Even with billing on, `loadPlans` reads the `plans` table; when it is empty the code
   returns `unlimitedQuota` (`plan_quota.go:94-95`).

The flag **fails closed to the hosted behavior**: unset or unparseable → enforced. Never
set it `false` in `docker-compose.yml` or `docker-compose.prod.yml`.

**The UI follows the flag.** Because nothing is enforced, the Usage panel would report a plan
you do not have, so it is withheld: the `Usage` entry disappears from the sidebar and from the
admin nav, and `/usage` and `/admin/usage` say so instead of rendering. That is the
`FEATURE_USAGE_PANEL` flag, whose default is *derived* from `RSYNC_BILLING_ENFORCED` rather than
set separately — so the quickstart compose and the Helm chart get it for free, and the display
can never disagree with the enforcement. Set `FEATURE_USAGE_PANEL=true` on the api-gateway to
show the page anyway; the numbers it reports are real, they are just not limits.

Metering is unaffected either way: transfer bytes and NL-query counts are still recorded, and
the dashboard's query-count tile still shows them, because `/api/v1/usage` is not gated — only
the plan/quota page is.

For reference, these are the hosted plan rows — they live in migrations, not in Go:

| Dimension | trial | free | starter | pro | Actually enforced? |
|---|---|---|---|---|---|
| Pipelines (`pipeline_limit`) | 1 | 2 | 5 | unlimited | **Yes** — creation is blocked |
| Data transfer (`included_gb`/mo) | 10 | 10 | 100 | 500 | **No** — metered only |
| NL queries (`included_queries`/mo) | 1000 | 1000 | 3000 | 10000 | **No** — metered only |
| LLM spend | $50/mo, per **user**, not per plan | | | | **Yes** |
| Plan duration (`duration_days`) | 14 | 30 | — | — | expiry cascades trial → free |

Sources: `api-gateway/migrations/060_user_plans.sql:37-41`,
`071_starter_plan_and_workspace_plans.sql:27-28`, `072_plan_gb_allowances.sql:18-21`,
`075_nl_query_usage.sql:20-23`, `057_llm_usage_quotas.sql:35`.

⚠️ **Read the last column.** Data transfer and NL-query counts are recorded but never
refused: `charge_workspace_bytes()` never refuses, and the NL-query charger is documented
in-migration as "enforcement is Phase 2". Only the pipeline count and the per-user LLM
spend actually block. Direct SQL is neither limited nor metered.

## Where self-host is stricter than hosted

Worth stating, because the usual assumption runs the other way:

| Setting | Hosted | Self-hosted |
|---|---|---|
| `RSYNC_CSRF_ENFORCE` | `false` by default | **`true`** |
| `RSYNC_TRUST_CLOUDFLARE` | on (runs behind Cloudflare) | `false` |
| `ENVIRONMENT` | `development` in the base compose | `production` |
| Internal connectors | selectable | blocked |
| `RSYNC_LLM_SAMPLE_ROWS_ALLOWED` | unset: Ask rsync.ai may send up to 5 sample rows once an admin approves the model and the workspace owner turns them on | **`false`**: no row value reaches a model, and the switches are hidden |
| `RSYNC_SINGLE_WORKSPACE` | unset: every user gets a personal workspace and may create more | **`true`**: one workspace for the install; a second is refused |
| Sign-up after the first user | `open` unless an admin changes it | **`invite_only`** unless an admin changes it |

**Sample rows.** The self-host compose sets `RSYNC_LLM_SAMPLE_ROWS_ALLOWED: "false"` on both
containers that check it, `api-gateway` and `llm-service` (`docker-compose.quickstart.yml:893`,
`:1532`); the Helm chart sets it from one value, `generation.llm.sampleRowsAllowed: false`. With it
off, api-gateway answers 403 `sample_rows_disabled` to the switches and llm-service's egress guard
sends no row whatever the stored approvals say. Turning it on is an operator decision made in the
install's configuration; what a model then receives is listed in
[llm-egress.md](../security/llm-egress.md).

**One workspace.** The self-host compose sets `RSYNC_SINGLE_WORKSPACE: "true"` on `api-gateway`
(`docker-compose.quickstart.yml:1535`); the Helm chart sets it from `apiGateway.singleWorkspace`
(`values.yaml:609`), and a chart with that value removed still renders `true`. The first person
to sign up becomes the platform admin and owns the install workspace. After that, sign-up is
invite-only until an admin changes Registration in admin settings: an instance invite (which
joins the install workspace) or a workspace invite (which joins the workspace it names) lets a
new user in. A user who signs up
while registration is open, with no invite, joins as a viewer. Creating a second workspace
answers 403 `single_workspace_mode`; deleting the install workspace, or the user who owns it,
answers 409. An install upgraded from a release without this keeps every workspace its users
already have; new users join the earliest admin's oldest team workspace (or a new one, if that
admin owns none). How the gateway picks and
guards the install workspace:
[multi-tenancy.md § Single-workspace mode](../architecture/multi-tenancy.md#11-single-workspace-mode-self-host).

## See also

- [self-hosting.md](self-hosting.md) — the actual install procedure
