# MCP Connector × Pipeline coverage

This document tracks **what level of test coverage every MCP connector
has**, so we can answer "does the pipeline work end-to-end for every
connector?" in one glance and prioritize which gaps to close next.

It is updated by hand. When you add a connector or a test, update the
relevant cell.

Three coverage tiers, in increasing strength:

| Tier | What it proves | How to add one |
|---|---|---|
| **Contract** | Connector imports, auth wires correctly, schema/pagination/type normalization behave — but the connector is never plugged into a real pipeline. Catches "the connector's code is internally broken." | A class in [llm-service/test_generated_connectors.py](../llm-service/test_generated_connectors.py), **or** an offline suite next to the connector (`versions/<v>/test_*.py`), which CI's "Per-connector regression tests" step runs ([ci.yml:663](../.github/workflows/ci.yml:663)) |
| **Stage** | Each pipeline stage (extract / transform / load) works in isolation against a real DB, but not chained together through Kafka. Catches "this connector emits a record the sink can't consume." | A pytest under `e2e/` that boots only the source MCP and a destination DB |
| **E2E** | Source MCP → Kafka → sink → destination, with row-count + content parity. Catches everything between. | A pytest under `e2e/test_*.py` that uses `docker-compose.e2e.dbs.yml` |

A connector is "**covered**" when it has at least **Stage** tier as
either a source or a destination. Only **E2E** tier proves a full
pipeline. **⚠️ (env-gated)** marks a test that skips unless a live
service or real credentials are set, so a default run never exercises it.

## As of main, 2026-10-07

### Sources

| Connector | Contract | Stage | E2E (as source) | Notes |
|---|---|---|---|---|
| MySQL | ✅ | ✅ | ✅ → Postgres CDC, ✅ → S3 CDC | strongest coverage |
| PostgreSQL | ✅ | ✅ | ✅ → MySQL CDC, ✅ → S3 CDC | strongest coverage |
| Oracle | ✅ | ❌ | ⚠️ → Postgres CDC (env-gated) | [e2e/test_oracle_cdc_to_postgres.py](../e2e/test_oracle_cdc_to_postgres.py) skips unless `ORA_LIVE_HOST` points at a LogMiner-ready Oracle. Connector unit tests in its `versions/v1.0.0/` |
| MongoDB | ✅ | ❌ | ✅ → Postgres CDC, ✅ → GCS CDC, ✅ → GCS batch | → Postgres via [e2e/test_mongodb_cdc_to_postgres.py](../e2e/test_mongodb_cdc_to_postgres.py) (Debezium and the sink driven directly); → GCS through the control plane against fake-gcs-server via [e2e/test_mongodb_cdc_to_gcs.py](../e2e/test_mongodb_cdc_to_gcs.py) and [e2e/test_mongodb_multi_collection_to_gcs_batch.py](../e2e/test_mongodb_multi_collection_to_gcs_batch.py). Connector unit tests in its `versions/v1.0.0/` |
| SQL Server | ✅ | ❌ | ⚠️ → Postgres CDC (env-gated) | [e2e/test_sqlserver_cdc_to_postgres.py](../e2e/test_sqlserver_cdc_to_postgres.py) skips unless `SS_LIVE_HOST` points at a real SQL Server or Azure SQL with CDC. Connector unit tests in its `versions/v1.0.0/` |
| ClickHouse | ✅ | ❌ | ⚠️ → Postgres batch (env-gated) | phase B of [e2e/test_clickhouse_batch_roundtrip.py](../e2e/test_clickhouse_batch_roundtrip.py), which skips unless `CH_LIVE_HOST` is set. Connector unit tests in its `versions/v1.0.0/` |
| Stripe | ⚠️ shared paging handler only (`TestPaginationHandlerFixes`), no connector class | ❌ | ❌ | `starting_after` / `has_more` paging is tested on the shared `PaginationHandler`, not on the Stripe connector |
| notion-rest | ❌ | ❌ | ❌ | no contract class; `start_cursor` / `next_cursor` paging untested |
| github-rest (REST/OpenAPI, **generated**) | — | ✅ | ✅ → Postgres batch, ✅ → S3/MinIO batch, ✅ → Postgres via **OAuth2 refresh** | **first generated-connector → DB E2E**, **first generated-connector → object-store E2E**, *and* **first OAuth2 token-refresh E2E.** Real public GitHub `/licenses` (~13 rows). → Postgres via [e2e/test_github_rest_to_postgres_batch.py](../e2e/test_github_rest_to_postgres_batch.py) (guards PR #270's executor `records`-key fix). → aws-s3/MinIO via [e2e/test_github_rest_to_s3_batch.py](../e2e/test_github_rest_to_s3_batch.py) (lands `part-000000.jsonl` + `_MANIFEST.json` + `_SUCCESS`). **OAuth2 refresh** via [e2e/test_github_rest_oauth_refresh.py](../e2e/test_github_rest_oauth_refresh.py): mints a token through the real authorize→callback flow (mock provider), forces expiry, and proves the platform runs the `refresh_token` grant and the refreshed token lands rows (asserts the mock's refresh counter moved + zero unauthorized calls). The batch tests are runnable/nightly (external dep on api.github.com), **not** merge-blocking. The OAuth test is **deterministic + mock-backed** and now wired as an **opt-in CI lane** — `E2E_INCLUDE_OAUTH=1 e2e/run_gate.sh` brings up `mock-github` + the github-rest MCP (private-net overlay) and runs it; it self-skips if that wiring is absent. Kept opt-in (nightly/on-demand), not per-PR, to avoid adding OAuth-mock flakiness to every PR. |
| Shopify (admin-graphql) | ✅ | ❌ | ⚠️ → Postgres batch (env-gated) | GraphQL contract; E2E via [e2e/test_shopify_to_postgres_batch.py](../e2e/test_shopify_to_postgres_batch.py), which skips unless `SHOPIFY_SHOP` / `SHOPIFY_ACCESS_TOKEN` (real store) are set |
| widgets-graphql (GraphQL, **generated**) | — | ✅ | ✅ → Postgres batch | **first generated-GRAPHQL-connector → DB E2E** — closes the last generator protocol-coverage gap (REST already proven via github-rest above; this is the GraphQL twin). Deterministic + mock-backed: [e2e/mock_graphql_server.py](../e2e/mock_graphql_server.py) serves a 5-row Relay `widgets` connection; the connector is generated from [e2e/fixtures/widgets_graphql_introspection.json](../e2e/fixtures/widgets_graphql_introspection.json) via `/v1/generate` (protocol=graphql, spec-first/deterministic); [e2e/test_graphql_connector_to_postgres_batch.py](../e2e/test_graphql_connector_to_postgres_batch.py) asserts **exact** row parity (== 5) + content (`widget-3`==`Charlie`). Wired as an **opt-in CI lane** — `E2E_INCLUDE_GRAPHQL=1 e2e/run_gate.sh` brings up `mock-graphql` + the widgets-graphql MCP (private-net overlay [docker-compose.mcp.graphql.e2e.yml](../docker-compose.mcp.graphql.e2e.yml)); self-skips if absent. Opt-in (nightly/on-demand), not per-PR. |
| google-sheets | ❌ | ❌ | ❌ | no connector-specific test |

### Destinations

| Connector | Contract | Stage | E2E (as destination) | Notes |
|---|---|---|---|---|
| PostgreSQL | ✅ | ✅ | ✅ ← MySQL CDC | strongest |
| MySQL | ✅ | ✅ | ✅ ← Postgres CDC | strongest |
| S3 / MinIO | ✅ | ✅ | ✅ ← MySQL CDC, ✅ ← Postgres CDC, ✅ ← github-rest batch (**generated** source) | strongest; batch-from-generated-source proven via [e2e/test_github_rest_to_s3_batch.py](../e2e/test_github_rest_to_s3_batch.py) |
| Snowflake | ✅ | ❌ | ❌ | connector unit tests in its `versions/v1.0.0/`; nothing under `e2e/` references it |
| BigQuery | ✅ | ❌ | ❌ | connector and adapter unit tests in its `versions/v1.0.0/`; no pipeline test under `e2e/` |
| Redshift | ✅ | ❌ | ❌ | connector unit tests in its `versions/v1.0.0/`; no pipeline test under `e2e/` |
| Databricks | ✅ | ❌ | ❌ | connector unit tests in its `versions/v1.0.0/`; no pipeline test under `e2e/` |
| Azure Blob | ✅ | ❌ | ✅ ← MySQL CDC (Azurite) | shell probe [e2e/test_db_cdc_to_emulated_gcs_azure.sh](../e2e/test_db_cdc_to_emulated_gcs_azure.sh) against a local Azurite emulator |
| GCS | ✅ | ❌ | ✅ ← MySQL CDC, ✅ ← MongoDB CDC, ✅ ← MongoDB batch | all against fake-gcs-server: MySQL via the shell probe [e2e/test_db_cdc_to_emulated_gcs_azure.sh](../e2e/test_db_cdc_to_emulated_gcs_azure.sh); MongoDB via [e2e/test_mongodb_cdc_to_gcs.py](../e2e/test_mongodb_cdc_to_gcs.py) and [e2e/test_mongodb_multi_collection_to_gcs_batch.py](../e2e/test_mongodb_multi_collection_to_gcs_batch.py) |
| MongoDB | ✅ | ❌ | ✅ ← Postgres CDC, ✅ ← MySQL CDC | the sink's document-DB lane: [e2e/test_postgres_to_mongodb_dest_cdc.py](../e2e/test_postgres_to_mongodb_dest_cdc.py), [e2e/test_mysql_to_mongodb_dest_cdc.py](../e2e/test_mysql_to_mongodb_dest_cdc.py) |
| ClickHouse | ✅ | ❌ | ⚠️ ← Postgres batch (env-gated) | phase A of [e2e/test_clickhouse_batch_roundtrip.py](../e2e/test_clickhouse_batch_roundtrip.py), which skips unless `CH_LIVE_HOST` is set |

### Internal MCP services

| Connector | Tests |
|---|---|
| `mcp-debezium` | exercised transitively by every CDC E2E test |
| `mcp-kafka-sink` | exercised transitively by every CDC E2E test |
| `mcp-minio` | exercised transitively by `*_to_s3_cdc.py` |
| `mcp-context7` | no test |

## Prioritized gaps

In order of (impact × tractability):

0. **Generated-connector protocol coverage (REST + GraphQL).** ✅ *Closed.* A generated **REST** connector was already proven E2E (github-rest, gap #2). A generated **GraphQL** connector is now proven E2E too — `widgets-graphql → Postgres` batch ([e2e/test_graphql_connector_to_postgres_batch.py](../e2e/test_graphql_connector_to_postgres_batch.py)), deterministic + mock-backed, opt-in via `E2E_INCLUDE_GRAPHQL=1`. Both of the generator's real output protocols (REST/OpenAPI + GraphQL/introspection) now have a live pipeline proof.
1. **One data warehouse destination, end-to-end.** Snowflake, BigQuery, Redshift, and Databricks all have zero pipeline tests. Pick one (Snowflake is the most-requested) and prove `MySQL → Snowflake CDC` runs cleanly. Establishes the pattern; the other three are then near-copies.
2. **One SaaS source → non-DB destination, end-to-end.** ✅ *Both landed:* a generated REST connector now proven into **two** destination classes — `github-rest → Postgres` batch ([e2e/test_github_rest_to_postgres_batch.py](../e2e/test_github_rest_to_postgres_batch.py), relational) and `github-rest → aws-s3/MinIO` batch ([e2e/test_github_rest_to_s3_batch.py](../e2e/test_github_rest_to_s3_batch.py), object-store). Together they prove the executor's dest-type-agnostic routing works from a generated source. Both excluded from the merge-gate (external dep on api.github.com), so runnable/nightly, not blocking. ✅ **Deterministic mock-backed variant landed** (as an OAuth-refresh source test, [e2e/test_github_rest_oauth_refresh.py](../e2e/test_github_rest_oauth_refresh.py)): `mock-github` now serves `/licenses`, and the CI-only overlay `docker-compose.mcp.e2e.yml` grants the github-rest MCP `MCP_ALLOW_PRIVATE_NETWORKS=true` + attaches it to the app network so it resolves the private mock — closing the SSRF-forwarding gap. Run it via `E2E_INCLUDE_OAUTH=1 e2e/run_gate.sh`. Kept **opt-in** (nightly/on-demand) rather than per-PR gate-blocking, a deliberate call to keep OAuth-mock flakiness off every PR.
3. **Oracle source.** Real customer ask. An E2E test exists ([e2e/test_oracle_cdc_to_postgres.py](../e2e/test_oracle_cdc_to_postgres.py)) but skips unless `ORA_LIVE_HOST` points at a LogMiner-ready Oracle; the gap is an Oracle a default run can reach.
4. **MongoDB source.** ✅ *Closed:* → Postgres CDC, → GCS CDC and → GCS batch (Sources table above).
5. **Azure Blob / GCS destinations.** ✅ *Closed against local emulators* (Azurite, fake-gcs-server) by [e2e/test_db_cdc_to_emulated_gcs_azure.sh](../e2e/test_db_cdc_to_emulated_gcs_azure.sh) and the MongoDB → GCS tests; not against a real cloud bucket.
6. **Connector deletion path.** Outside this matrix, but worth tracking: regenerated/superseded connectors (Pillar 2 follow-up).

## Adding a new test

- **Contract:** open [llm-service/test_generated_connectors.py](../llm-service/test_generated_connectors.py), copy an existing `TestXxxConnector` class, swap the vendor dir + mock response, add to the suite. Runs in the existing `llm-service-unit` CI lane.
- **Stage / E2E:** add a `e2e/test_<source>_to_<dest>_<mode>.py`. Use [e2e/test_type_fidelity_mysql_to_postgres_cdc.py](../e2e/test_type_fidelity_mysql_to_postgres_cdc.py) as a template. Requires `docker-compose.e2e.dbs.yml` running locally; not yet wired into PR CI.
