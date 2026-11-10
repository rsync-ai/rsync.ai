# Ask rsync.ai (the in-app assistant)

**Ask rsync.ai** is a chat panel inside the rsync.ai UI. You ask a question in plain words,
such as "why did this pipeline's last run fail?", and it answers by reading your
connections, table schemas, pipelines and runs.

It reads metadata, never data. What else it can do follows your workspace role. For any
member it can propose a pipeline, which you create yourself from the link it gives. For an
owner or admin it can also create, run and schedule models, draft a workflow for you to test
and turn on, and [generate a connector](#generate-a-connector) from a public API's OpenAPI
document or docs page when you type the URL. It uses the same tools as the
[agent gateway](agent-gateway.md#tools), which lists each one with the role it needs, run as
you: it sees what you can see in your active workspace, and nothing more.

## Turn it on

The assistant is off by default. While it is off, its routes do not exist, the header
shows no **Ask rsync.ai** button, and Admin → Settings shows no **AI models** card.

Set `RSYNC_ASSISTANT_ENABLED=true` on the `api-gateway` service and restart it:

- **Docker Compose** (every bundled compose file passes it through): add the line to
  the `.env` file compose reads, then recreate `api-gateway`.
- **Helm**: set it in the chart's top-level `extraEnv`:

```yaml
extraEnv:
  RSYNC_ASSISTANT_ENABLED: "true"
```

`GET /api/v1/features` then reports `"assistant": true`, which is what the UI reads.

The assistant then needs at least one model key (next section). With none, the panel
says that an admin can add one.

## Give it a model

The panel offers up to three models. Each one is listed once it has a key:

| Model | Key | Model id when none is set |
|---|---|---|
| Claude | an Anthropic API key | `claude-sonnet-5-5` |
| ChatGPT | an OpenAI API key | `gpt-4o-mini` |
| Gemini | a Google AI Studio (Gemini API) key | `gemini-2.5-flash` |

With more than one, each person picks a model in the panel, and the browser remembers
the choice.

### In the admin panel (recommended)

A platform admin opens **Admin → Settings → AI models** and, for each model:

1. Selects **Set key**, pastes the key, and optionally types a model id.
2. Selects **Save**.
3. Selects **Test**, which sends one short question and shows the model that answered,
   or the reason it failed.

What happens to the key:

- It is stored encrypted with the same `ENCRYPTION_KEY` as connection passwords, and
  the key rotation covers it.
- No screen or route ever returns it. The card shows only where each model's key
  comes from: **Key saved here**, **Key from the environment file**, or **No key**.
- Setting, removing and testing a key each write an audit row. The row names the
  model and the model id, never the key.
- Each question sends the key to llm-service, the service that calls the model, over
  the internal network. llm-service never holds `ENCRYPTION_KEY`.

A model id is a name only, such as `claude-opus-5-5` or `gpt-5`, never an address.
**Change** with the key field left empty keeps the saved key and changes only the
model id.

### In the environment file

The same keys can be set on the `llm-service` service instead:

| Model | Key variable | Model id variable |
|---|---|---|
| Claude | `ANTHROPIC_API_KEY` | `ASSISTANT_CLAUDE_MODEL` |
| ChatGPT | the stack's own `OPENAI_API_KEY` (or its Azure OpenAI setup) | `ASSISTANT_OPENAI_MODEL` |
| Gemini | `GEMINI_API_KEY` | `ASSISTANT_GEMINI_MODEL` |

With no model id variable, Claude and Gemini use the defaults above, and ChatGPT from
the environment file uses the stack's own model (`LLM_MODEL`, else its provider's
default).

A key saved in the admin panel wins over the environment file's. **Remove** in the
panel falls back to the environment file's key, if one is set.

ChatGPT never follows `OPENAI_BASE_URL`. If the stack's OpenAI setup points at another
endpoint (Groq, Ollama, a gateway), the environment file offers no ChatGPT entry; save
an OpenAI key in the admin panel instead.

### Gemini on Vertex AI, with no key

A stack set up for [Vertex AI on a Google Cloud VM](../deployment/self-hosting.md#groq-azure-openai-and-vertex-ai)
(`OPENAI_API_KEY_SOURCE=gcp-metadata` and a Vertex AI `OPENAI_BASE_URL`) offers
**Gemini (Vertex)** with no key. llm-service calls Vertex AI with the stack's own client,
which sends the VM's access token, and names the model `google/<model id>`. The model id
is `ASSISTANT_GEMINI_MODEL`, else `gemini-2.5-flash`. Such a stack offers no ChatGPT
from the environment file (above).

- A Gemini key, saved in the panel or set as `GEMINI_API_KEY`, wins: Gemini then goes to
  Google AI Studio with that key.
- Sample rows are never sent to Vertex AI. An admin cannot approve it as a destination,
  so a `sample_rows` result reaches the model as "Not sent".

### Gemini: use a paid key

Google's terms let it use what is sent to the Gemini API on its free tier to improve
its products. The assistant sends only metadata (below), but that still includes your
table, column and pipeline names. Use a key from a Google Cloud project with billing
turned on.

## Ask a question

Select **Ask rsync.ai** in the header, or press ⌘/ (Ctrl+/ on Windows and Linux). The
panel opens down the right side, and on a wide screen the page moves over to make room.
**Expand** makes it fill the page; Escape or × closes it.

- A new chat suggests questions to start from, the current page's first. Select one,
  or type your own.
- On a pipeline, connection, run or dashboard page, the question is about that page: the panel
  shows **About: This pipeline** and sends the page's id. Select the × on that chip to
  ask without it. On a pipeline's **Steps/DAG** tab, the tab on the right edge also
  opens the panel about that pipeline.
- Press Enter to send, or Shift+Enter for a new line. ↑ in an empty box brings back
  your last question.
- While it looks things up, **Stop** ends the question on the server as well as in
  the browser: it runs no further model step or tool, and an answer that lands
  anyway is not kept.
- Each answer says which tools it looked at and which model answered, and has **Copy**.
- A follow-up question continues the conversation. The server keeps the last five
  questions and answers for 30 minutes. After that the chat says so, and the next
  question starts a fresh conversation.

### Chats

**New chat** starts over. **History** lists your chats, newest first, and opens any of
them. Closing the panel keeps them.

Chats are kept in the browser tab, up to 20, and only in the workspace each one
started in. Reloading the page or signing out clears them; nothing about them is
stored on the server beyond its 30-minute copy of each conversation.

One question is bounded: at most 6 model turns, 12 tool calls, and 120 seconds. A
question that reaches a bound still gets an answer, marked "Stopped at the limit for
one question". A narrower question goes further.

The assistant counts against the same monthly LLM spend quota as the rest of rsync.ai,
and shares the chat rate limit: 20 messages a minute per person, chat and assistant
together.

### Generate a connector

Ask for it in your own words, with the link in the same message, for example
"add a connector for Acme from https://api.acme.com/openapi.json". The assistant:

- checks first that no connector for that API already exists;
- acts only on a URL you typed in this chat, never one it found by itself, and asks
  for the link when your message has none;
- starts the run and says so. It runs in the background for a few minutes; ask "is it
  done?" and the assistant looks the run up.

Only a workspace owner or admin can generate; for anyone else the assistant says so.
The same limits apply as on the Generate page: 10 runs an hour per workspace, 2 at
once. The new connector is built but not started, and every workspace sees it.

The assistant never asks for credentials and never takes any. Add them on the
connection form (`/connections/new`) once the connector is there. The full rule list
is in [agent-gateway.md](agent-gateway.md#generate-a-connector).

### Write SQL

Ask for a query in your own words, for example "orders by status last month on the
shop database". The assistant writes it with its `generate_sql` tool, which runs the
same two steps the Explorer's question box did: pick the tables from their names, then
write the SQL. Under the answer a card shows the SQL, marked **Not run**, with the tables
it reads, **Copy**, and **Open in Explorer**, which opens it in a new query tab on that
connection. Nothing runs until you run it there.

- The SQL goes to your browser only. The model is told which connection and tables it
  used, never the statement, so it cannot say what the query returns.
- Only a single read-only statement is shown; anything else is withheld and the
  assistant is told to ask again.
- Each SQL written counts as one NL→SQL query on the workspace's plan, as the Explorer's
  did, and is refused once the month's queries are used up. A connection that holds
  documents (MongoDB) gets no SQL.
- Once sample rows are in the conversation (below), the assistant writes no SQL in it:
  the question it would send could carry a value. Start a new chat for SQL.
- Each call that runs writes an `assistant_generate_sql` audit row with its connection, the
  number of tables and whether SQL was written, never the question or the SQL.

### Draft a dashboard

Ask for one in your own words, for example "a sales dashboard from the daily orders
query, with a date filter". The assistant lists what can be charted
(`list_chart_sources`), reads the columns of the models and saved queries it picks
(`describe_chart_source`), then drafts the dashboard with `plan_dashboard`. Under the
answer a card lists the draft's charts and filters, with **Preview**, **Who sees it**
and **Save dashboard**.

- Nothing is saved until you select **Save dashboard**. Saving creates the charts and the
  dashboard as you, private unless you pick the workspace, then opens the dashboard.
- A draft has up to 12 charts and 4 filters. Each chart gets the same checks as a chart
  you save yourself, and its query runs once, as you, to check it. A refused chart sends
  the assistant the rule and the column, never a value, so it can try again.
- An unsaved draft is kept for 24 hours, and you can have 20 unsaved drafts at a time.
- Only a workspace owner or admin can draft or save a dashboard; for anyone else the
  assistant says so, and a draft's card shows no Save.
- The model sees each source's name and its columns' names, kinds and whether they are
  masked, never a row. `describe_chart_source` runs the source for one row to read the
  column kinds, and that row is never sent. The model never picks a series colour.
- Each draft writes a `plan_dashboard` audit row with its number of charts, their types
  and its number of filters.

The page itself is described in
[Charts and dashboards](../explorer/charts-and-dashboards.md#draft-one-with-ask-rsyncai).

### Change a dashboard tile

On a dashboard, ask for a change to one of its tiles, for example "make the orders chart
a line, full width". The assistant finds the dashboard (`list_dashboards`), reads its
tiles (`get_dashboard`), then proposes the change with `plan_dashboard_change`. Under the
answer a card shows the tile now and after, what changes, the tiles that move down to
make room, and whether **Apply** changes the chart in place or copies it.

- One change is one tile: its chart's type, columns, measure, sort, colour and title, and
  the tile's width and height. The changed chart gets the same checks as a save, and its
  query runs once, as you. A refusal sends the assistant the rule and the column, never a
  value.
- Nothing changes until an owner or admin selects **Apply**. Applying keeps the dashboard
  as it was in its [History](../explorer/charts-and-dashboards.md#history).
  A chart on another dashboard too is copied for this tile.
- A change is kept for 24 hours, 20 open at a time. A dashboard or chart saved after the
  change was drafted makes it out of date, and it cannot be applied.
- `get_dashboard` gives the model each tile's place and each chart's name, source and
  settings, never a row, a series colour or a text tile's text.
- Each change writes a `plan_dashboard_change` audit row; applying writes
  `apply_dashboard_change` and, when the chart changes, `update_chart` (in place) or
  `create_chart` (a copy, with `copied_from`).

## What the model sees

The model is a third-party service, so by default it gets metadata only:

- your question, and the earlier questions and answers in the conversation;
- the names, types and settings the tools return: connections, schemas, pipelines,
  run states, and error text with literal values scrubbed out;
- the workspace's plan and how much of it is used: the plan's name, its pipeline and
  NL→SQL query limits, how many pipelines and members the workspace has, and the NL→SQL
  queries it used this month. Counts only, never a member's e-mail;
- rsync.ai's own topic guides: how pipelines, workflows, dashboards, models and checks,
  and connectors work. Before the model's first step the server loads the ones a
  question needs (at most three, chosen by the page, the question's words and the
  previous question); the model reads another with `read_guide`. A drafting tool
  (`plan_…`) called before its guide is in the conversation is not run: it answers
  with the guide, and the model calls it again.

It never gets query results, row counts, credentials or PII, and no row values unless you
turn on sample rows (below). Every call goes through llm-service's egress guard, which
enforces this list per call site. See [llm-egress.md](../security/llm-egress.md)
(`assistant.step`).

### Sample rows (off by default)

Sometimes names and types do not say what a column holds: a status code, a unit, how a JSON
column is shaped. With sample rows on, the assistant may read up to 5 rows of one table to
see. Two switches must both be on:

1. A platform admin allows sample rows for the model, in **Admin > Settings > AI models**.
2. The workspace owner turns them on, in **Workspace settings > Data**.

Then the assistant gets a `sample_rows` tool: at most 3 reads a question, 5 rows each, values
cut to 64 characters, never a row count. Columns whose name looks personal or secret, or that
a PII scan flagged, are left out, and llm-service checks every value with Presidio before it is
sent, leaving out any column where it finds personal data. If a switch is off, or the check
cannot run, the model gets a line saying so instead of the values. "Looked at" shows what was
read, for example "sample rows of public.orders (3 rows; 2 columns withheld)", and each read
writes an `assistant_sample_rows` audit row. An answer formed from sample rows is sent again
only to the model that read them. The rsync.ai MCP server has no such tool.

Each tool the assistant runs writes an audit row, `assistant_tool_call`, marked
`actor_kind=assistant`, so your own actions can be told apart from what the assistant
looked at for you.

## Routes

Session routes, which a script can call too:

- `GET /api/v1/assistant/models` returns `{"entries": [{"entry", "label", "model"}], "default"}`:
  only the models that have a key.
- `POST /api/v1/assistant/messages` takes `{"question", "conversation_id"?, "page"?, "model"?, "request_id"?}`.
  `page` is `{"kind": "pipeline" | "connection" | "run" | "dashboard", "id": "<uuid>"}`, and the
  question is 1 to 4,000 characters. It returns `{"conversation_id", "answer", "model",
  "looked_at", "stopped"?, "cost_cents", "usage"}`. `request_id` (1 to 64 letters, digits, `_`
  or `-`) names the question so it can be stopped. `usage` is what the question sent, in
  the provider's own token counts: `input_tokens`, `cached_tokens`, `output_tokens` and
  `reasoning_tokens`, then `steps`, one per model turn, each with those four, `chars` (how
  many characters of the request were the `system` prompt, the `tools` list, and the `user`,
  `assistant` and `tool` messages), `guide_chars` (how many of the `tool` characters were
  topic guides) and `calls` (`{"tool", "outcome", "status"?}` for each tool call it asked
  for; `guide_missing` is a drafting tool called before its guide). It holds no text.
  Gemini reports its thinking in `reasoning_tokens`, outside `output_tokens`, and Google
  bills it as output, so a Gemini question's `cost_cents` includes it.
- `POST /api/v1/assistant/messages/:request_id/cancel` stops your question with that
  id and answers 204, whether or not one is running. With more than one api-gateway
  replica, the one running the question stops it within a second.
- `GET /api/v1/workspace/llm-data` returns `{"mode": "names_only" | "sample_rows",
  "approved_models", "can_edit"}` for the active workspace, to any member.
  `PUT /api/v1/workspace/llm-data` takes `{"mode"}`, owner only (403 otherwise), and is
  audited as `workspace.llm_data_mode_update`.

Platform admin routes:

- `GET /api/v1/admin/assistant/models`
- `PUT /api/v1/admin/assistant/models/:entry` takes `{"api_key"?, "model"}`
- `DELETE /api/v1/admin/assistant/models/:entry`
- `POST /api/v1/admin/assistant/models/:entry/test`
- `PUT /api/v1/admin/assistant/models/:entry/sample-rows` takes `{"allowed": true | false}`,
  audited as `admin_set_assistant_sample_rows`; the list then shows `sample_rows` per entry

A failure returns `{"error", "message"}`. The error codes are:

| Error | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | The body, question, model or page is not valid |
| `quota_exceeded` | 402 | The monthly LLM spend quota is used up |
| `model_not_configured` | 409 | No model has a key, or the picked one has none |
| `refused` | 422 | The egress guard refused to send the question |
| `request_in_use` | 409 | A question with this `request_id` is already running |
| `rate_limit_exceeded` | 429 | Too many questions; shared with the chat rate limit |
| `stopped`, `cancelled` | 499 | The question was stopped, or the browser went away before the answer |
| `provider_error` | 502 | The model provider refused the call: the key was rejected, the model id is unknown, the key is rate limited, or another error. A rate-limited step is tried again first, after 1, 2 and 4 seconds (or the provider's `Retry-After` when it is at most 10), so this means it still was. Never the provider's own text, which can echo part of the key |
| `llm_service_unreachable`, `llm_service_error` | 502 | api-gateway could not reach llm-service, or got no usable answer |
| `stop_unavailable` | 503 | Cancel could not reach the store that tells the other replicas; the question ends at its 120-second limit |
| `timeout` | 504 | The question took longer than 120 seconds |

The admin **Test** route reports a provider's refusal as `200` with `"ok": false` and the
reason, not as an error.
