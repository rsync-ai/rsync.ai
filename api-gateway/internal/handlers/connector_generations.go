package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

// Connector generation runs (docs/internal/ask-rsync-and-mcp-roadmap.md §2.2).
//
// One core serves every way a connector is generated. The Generate page (REST)
// waits for its answer, as it always has; an agent token's generate_connector,
// Ask rsync.ai and the chat "generate <name>" command start a run in the
// background and get its id back at once. Every one that writes to the
// registry records a connector_generation_runs row, so only one run per
// connector name is in flight across workspaces, at most
// maxRunningGenerations per workspace, and a background caller can read its
// run back (GetConnectorGeneration).
//
// Generation saves a connector but does not start it: the infra preflight
// starts it (and the connector-deployer builds its image) when the first
// pipeline that uses it runs. Code an agent
// caused to be written does not run until someone uses it, and it has no
// credentials until a person creates a connection on the connection form.

const (
	generationRunLimit    = 15 * time.Minute
	generationStaleAfter  = 20 * time.Minute
	maxRunningGenerations = 2
	maxGenerationURLLen   = 2048
	maxGenerationDescLen  = 500
	maxGenerationErrRunes = 500
	generatorProbeTimeout = 3 * time.Second
	generationSweepEvery  = 5 * time.Minute

	actorKindChat = "chat"
	actorKindUI   = "ui"
)

// GenerationInput is what an agent, Ask rsync.ai or the chat asks to generate.
// SpecURL is an OpenAPI or Swagger document; DocsURL is an API docs page. At
// most one is set.
type GenerationInput struct {
	Name        string
	SpecURL     string
	DocsURL     string
	Description string
}

// GenerationRun is one connector_generation_runs row as a caller sees it.
type GenerationRun struct {
	ID               string     `json:"id"`
	ConnectorName    string     `json:"connector_name"`
	Status           string     `json:"status"`
	SourceURL        string     `json:"source_url,omitempty"`
	ActorKind        string     `json:"actor_kind"`
	Version          string     `json:"version,omitempty"`
	ValidationPassed *bool      `json:"validation_passed,omitempty"`
	ToolCount        *int       `json:"tool_count,omitempty"`
	Error            string     `json:"error,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

// GenerationRefusal is a generation rsync.ai declined before starting it. Code
// is stable and machine-readable; Message is the sentence the caller shows.
type GenerationRefusal struct {
	Code       string
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (r *GenerationRefusal) Error() string { return r.Message }

func refuse(code string, status int, format string, a ...any) *GenerationRefusal {
	return &GenerationRefusal{Code: code, Status: status, Message: fmt.Sprintf(format, a...)}
}

// generationCaller is who asked, as the run row records it.
type generationCaller struct {
	actor   services.Actor
	tokenID string
	// userText is the user's own words, for Ask rsync.ai's URL check.
	userText string
}

// launchGeneration runs a generation in the background. Tests replace it to
// run the generation inline.
var launchGeneration = func(run func()) { go run() }

// StartConnectorGeneration checks a background generation request and, when
// it passes, records a running row, starts the run and returns the row. A
// refusal is a *GenerationRefusal; any other error means rsync.ai could not
// record the run.
func StartConnectorGeneration(c *gin.Context, actor services.Actor, in GenerationInput) (*GenerationRun, error) {
	caller := generationCaller{
		actor:    actor,
		tokenID:  c.GetString(ctxTokenID),
		userText: c.GetString(services.CtxUserText),
	}
	if actor.Channel != services.ChannelAgent {
		caller.tokenID = ""
	}
	return startConnectorGeneration(withLLMWorkspace(c.Request.Context(), actor.WorkspaceID), caller, in)
}

func startConnectorGeneration(ctx context.Context, caller generationCaller, in GenerationInput) (*GenerationRun, error) {
	actor := caller.actor
	if actor.Channel == services.ChannelREST {
		// The Generate page waits for its answer; it goes through GenerateConnector.
		return nil, refuse("forbidden", http.StatusForbidden, "Use the Generate page to generate a connector from the UI.")
	}
	if err := services.Authorize(actor, services.ActionConnectorGenerate); err != nil {
		return nil, generationForbidden(actor)
	}

	raw := strings.TrimSpace(in.Name)
	if !isKebabCaseConnectorID(raw) {
		r := refuse("invalid_name", http.StatusBadRequest, "The connector name must be lowercase kebab-case, for example acme-crm.")
		if n := normalizeConnectorName(raw); n != "" && n != raw {
			r.Message += fmt.Sprintf(" Try %q.", n)
		}
		return nil, r
	}
	name := normalizeConnectorName(raw)
	if len(name) > 100 {
		return nil, refuse("invalid_name", http.StatusBadRequest, "The connector name is longer than 100 characters.")
	}
	if isProtectedCoreConnector(name) {
		return nil, refuse("protected_connector", http.StatusForbidden,
			"%s is a built-in connector rsync.ai maintains; it cannot be generated or overwritten.", name)
	}
	if isDevOnlyConnector(name) {
		return nil, refuse("hidden_name", http.StatusBadRequest, "%s", hiddenNameRefusal(name))
	}
	if mcpConnectorIsVersioned(name) {
		return nil, refuse("already_exists", http.StatusConflict,
			"A connector named %s already exists. Use it in a pipeline, or pick another name.", name)
	}

	spec, docs := strings.TrimSpace(in.SpecURL), strings.TrimSpace(in.DocsURL)
	if spec != "" && docs != "" {
		return nil, refuse("invalid_url", http.StatusBadRequest, "Give spec_url or docs_url, not both.")
	}
	source := spec
	if source == "" {
		source = docs
	}
	if source == "" && actor.Channel != services.ChannelChat {
		return nil, refuse("url_required", http.StatusBadRequest,
			"Give the URL of the API's OpenAPI or Swagger document (spec_url), or of its documentation (docs_url).")
	}
	if source != "" {
		if r := checkGenerationURL(source); r != nil {
			return nil, r
		}
	}
	if actor.Channel == services.ChannelAssistant && !userTextHasURL(caller.userText, source) {
		return nil, refuse("url_not_from_user", http.StatusBadRequest,
			"Ask rsync.ai only generates from a URL you typed yourself. Paste the API's OpenAPI or docs URL into your message.")
	}

	caps, err := probeGenerator(ctx)
	if err != nil {
		log.WithError(err).Warn("connector generation: the generator did not answer its health check")
		return nil, refuse("generator_unavailable", http.StatusServiceUnavailable,
			"The connector generator is not answering. Try again in a few minutes.")
	}
	if !caps.GenerateFromURL {
		return nil, refuse("upload_spec_required", http.StatusConflict,
			"This edition generates connectors from an uploaded spec only. Upload it at /connectors/generate.")
	}
	if !caps.CatalogShared {
		return nil, refuse("private_catalog", http.StatusConflict,
			"This install cannot keep a generated connector: every service reads its own copy of the connector "+
				"catalog from the release image. Generate connectors on a Docker install.")
	}

	if ok, wait := connectorGenLimiter.Allow(actor.WorkspaceID, time.Now()); !ok {
		r := refuse("rate_limited", http.StatusTooManyRequests,
			"This workspace has generated 10 connectors in the last hour. Try again in %s.", roundWait(wait))
		r.RetryAfter = wait
		return nil, r
	}

	run, err := admitGenerationRun(ctx, caller, actorKindFor(actor.Channel), name, source)
	if err != nil {
		return nil, err
	}

	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		desc = "User-requested connector for " + name
	}
	if r := []rune(desc); len(r) > maxGenerationDescLen {
		desc = string(r[:maxGenerationDescLen])
	}
	req := map[string]any{
		"api_name":         name,
		"description":      desc,
		"force_regenerate": false,
		"enable_chaos":     true,
		"save_artifacts":   true,
	}
	if spec != "" {
		req["openapi_spec_url"] = spec
	}
	if docs != "" {
		req["docs_url"] = docs
	}
	runID, ws := run.ID, actor.WorkspaceID
	launchGeneration(func() { executeGenerationRun(runID, ws, req) })
	return run, nil
}

// generationForbidden says why the caller may not generate, in its own terms.
func generationForbidden(actor services.Actor) *GenerationRefusal {
	switch {
	case !actor.Role.Meets(security.WSAdmin) && actor.Channel == services.ChannelAgent:
		return refuse("forbidden", http.StatusForbidden,
			"This token's user is not an admin or owner of the workspace, and only they can generate a connector.")
	case !actor.Role.Meets(security.WSAdmin):
		return refuse("forbidden", http.StatusForbidden,
			"Only a workspace admin or owner can generate a connector. Ask one of them to do it.")
	default:
		return refuse("forbidden", http.StatusForbidden,
			"This token lacks the %s scope. An owner or admin can create a token that has it.", services.ScopeConnectorsGenerate)
	}
}

func actorKindFor(ch services.Channel) string {
	switch ch {
	case services.ChannelAgent:
		return actorKindAgent
	case services.ChannelAssistant:
		return actorKindAssistant
	case services.ChannelChat:
		return actorKindChat
	default:
		return actorKindUI
	}
}

func roundWait(d time.Duration) string {
	if d < time.Minute {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Round(time.Minute)/time.Minute))
}

// checkGenerationURL is the gateway's first, cheap check of a URL a
// generation will fetch: https, a public-looking host name, no credentials.
// The generator guards every fetch and redirect against private addresses
// itself (discoverer.py _ssrf_guarded_get); this refuses the obvious cases
// before a run is recorded.
func checkGenerationURL(raw string) *GenerationRefusal {
	if len(raw) > maxGenerationURLLen {
		return refuse("invalid_url", http.StatusBadRequest, "The URL is longer than %d characters.", maxGenerationURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return refuse("invalid_url", http.StatusBadRequest, "The URL must start with https:// and name a host.")
	}
	if u.User != nil {
		return refuse("invalid_url", http.StatusBadRequest, "The URL must not carry a user name or password.")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return refuse("invalid_url", http.StatusBadRequest, "The URL must start with https:// and name a host.")
	}
	if net.ParseIP(host) != nil {
		return refuse("invalid_url", http.StatusBadRequest, "The URL must name a host, not an IP address.")
	}
	if !strings.Contains(host, ".") || host == "localhost" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return refuse("invalid_url", http.StatusBadRequest, "The URL must name a public host.")
	}
	return nil
}

// userTextHasURL reports whether the user typed the URL themselves. A
// trailing slash is ignored, so "…/openapi.json/" matches what was typed.
func userTextHasURL(userText, raw string) bool {
	want := strings.TrimRight(raw, "/")
	return want != "" && strings.Contains(userText, want)
}

func toolGeneratorBaseURL() string {
	u := strings.TrimRight(strings.TrimSpace(os.Getenv("TOOL_GENERATOR_URL")), "/")
	if u == "" {
		u = "http://tool-generator:5010"
	}
	return u
}

// generatorCaps is what the generator installed here can do, from its /health.
// A field the generator does not report keeps the full-flow default, so an
// older generator behaves as before.
type generatorCaps struct {
	GenerateFromURL bool
	CatalogShared   bool
}

func probeGenerator(ctx context.Context) (generatorCaps, error) {
	ctx, cancel := context.WithTimeout(ctx, generatorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, toolGeneratorBaseURL()+"/health", nil)
	if err != nil {
		return generatorCaps{}, err
	}
	stampLLMWorkspace(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return generatorCaps{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return generatorCaps{}, fmt.Errorf("health answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		GenerateFromURL *bool `json:"generate_from_url"`
		CatalogShared   *bool `json:"catalog_shared"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return generatorCaps{}, fmt.Errorf("health body: %w", err)
	}
	caps := generatorCaps{GenerateFromURL: true, CatalogShared: true}
	if body.GenerateFromURL != nil {
		caps.GenerateFromURL = *body.GenerateFromURL
	}
	if body.CatalogShared != nil {
		caps.CatalogShared = *body.CatalogShared
	}
	return caps, nil
}

// staleGenerationSQL fails every run still 'running' after generationStaleAfter:
// its process stopped before it could record the end. A run's own limit is
// generationRunLimit, so nothing live is touched.
const staleGenerationSQL = `UPDATE connector_generation_runs
	SET status = 'failed', error = 'Interrupted: rsync.ai stopped before the run finished.', finished_at = NOW()
	WHERE status = 'running' AND started_at < NOW() - INTERVAL '20 minutes'`

// admitGenerationRun records a running row, or refuses when the workspace
// already has maxRunningGenerations in flight or the same connector name is
// being generated anywhere. The advisory lock makes count-then-insert atomic
// per workspace; the partial unique index guards the name across workspaces.
func admitGenerationRun(ctx context.Context, caller generationCaller, kind, name, source string) (*GenerationRun, error) {
	database := db.GetDB()
	if database == nil {
		return nil, errors.New("database unavailable")
	}
	ws := caller.actor.WorkspaceID
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "connector_generation:"+ws); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, staleGenerationSQL); err != nil {
		return nil, err
	}
	var running int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connector_generation_runs WHERE workspace_id = $1 AND status = 'running'`, ws,
	).Scan(&running); err != nil {
		return nil, err
	}
	if running >= maxRunningGenerations {
		return nil, refuse("too_many_running", http.StatusTooManyRequests,
			"This workspace already has %d connector generations running. Wait for one to finish.", running)
	}
	run := &GenerationRun{ConnectorName: name, Status: "running", SourceURL: source, ActorKind: kind}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO connector_generation_runs (workspace_id, user_id, actor_kind, token_id, connector_name, source_url)
		 VALUES ($1, NULLIF($2, '')::uuid, $3, NULLIF($4, '')::uuid, $5, NULLIF($6, ''))
		 RETURNING id, started_at`,
		ws, caller.actor.UserID, kind, caller.tokenID, name, source,
	).Scan(&run.ID, &run.StartedAt)
	if pgdriver.SQLState(err) == "23505" {
		return nil, refuse("already_running", http.StatusConflict,
			"A connector named %s is being generated right now. Check back in a few minutes.", name)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

// executeGenerationRun posts a background run to the generator and records
// how it ended. It is detached from the request that started it, keeping its
// workspace for the generator's egress log.
func executeGenerationRun(runID, workspaceID string, req map[string]any) {
	ctx, cancel := context.WithTimeout(withLLMWorkspace(context.Background(), workspaceID), generationRunLimit)
	defer cancel()
	res := postToolGenerator(ctx, req)
	if res.failure != "" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.failure = fmt.Sprintf("The generator did not finish within %d minutes.", int(generationRunLimit/time.Minute))
	}
	finishGenerationRun(runID, res)
}

// generatorResult is one /v1/generate call. failure is set when the gateway
// got no readable answer; otherwise payload is the generator's JSON.
type generatorResult struct {
	status  int
	body    []byte
	payload map[string]any
	failure string
}

// postToolGenerator sends one /v1/generate request; ctx bounds it.
func postToolGenerator(ctx context.Context, toolGenReq map[string]any) generatorResult {
	reqBody, _ := json.Marshal(toolGenReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, toolGeneratorBaseURL()+"/v1/generate", bytes.NewReader(reqBody))
	if err != nil {
		log.Errorf("❌ Failed to build tool-generator request: %v", err)
		return generatorResult{failure: "Failed to build connector generator request"}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	stampLLMWorkspace(httpReq)
	// SEC-H-03: authenticate this GUARDED outbound generate call with the shared
	// internal-service secret when configured. tool-generator gates /v1/generate on
	// X-Internal-Secret (env INTERNAL_SERVICE_SECRET; compose sets it on api-gateway
	// in prod). No-op when unset. The raw ToolGeneratorProxy deliberately does NOT
	// send this header, so a proxied /v1/generate|/v1/deploy stays unauthenticated →
	// rejected upstream (defense in depth). Mirrors backend-orchestrator
	// server_manager.go tryDeployConnectorContainer.
	if secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET")); secret != "" {
		httpReq.Header.Set("X-Internal-Secret", secret)
	}
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		log.Errorf("❌ Failed to call tool-generator v2: %v", err)
		return generatorResult{failure: "Failed to reach connector generator service"}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Failed to read tool-generator response: %v", err)
		return generatorResult{status: resp.StatusCode, failure: "Failed to read generator response"}
	}
	// Parse as a generic map so no V2 field (error_stage, workflow_stages, ...) is dropped.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		log.Errorf("Failed to parse tool-generator response: %v", err)
		return generatorResult{status: resp.StatusCode, body: body, failure: "Failed to parse generator response"}
	}
	return generatorResult{status: resp.StatusCode, body: body, payload: payload}
}

// succeeded reports whether the generator said the connector was generated.
func (r generatorResult) succeeded() bool {
	if r.failure != "" || r.status < 200 || r.status > 299 {
		return false
	}
	ok, _ := r.payload["success"].(bool)
	return ok
}

// normalizeFailure gives a failed answer the error_message the UI reads.
func (r generatorResult) normalizeFailure() {
	if h, ok := llmNotConfiguredBody(r.status, r.body); ok {
		// The agentic generator needs a model; say what to set up, not the code.
		r.payload["error"] = h["error"]
		r.payload["error_message"] = h["message"]
		r.payload["error_stage"] = llmNotConfiguredCode
	}
	if _, ok := r.payload["error_message"]; ok {
		return
	}
	// Prefer legacy 'error' then 'message'
	if e, ok := r.payload["error"].(string); ok && e != "" {
		r.payload["error_message"] = e
	} else if m, ok := r.payload["message"].(string); ok && m != "" {
		r.payload["error_message"] = m
	} else if d := fastAPIDetail(r.payload["detail"]); d != "" {
		// llm-service signals failure by raising HTTPException, whose body
		// is {"detail": ...} and carries NEITHER "error" NOR "message"
		// (tool_generator/deployment/routes.go:62, service.py, and every
		// discovery route). The chain above therefore never matched a
		// single one of them, and they all reached the wizard as the flat
		// string "Generation failed" -- so an operator whose
		// INTERNAL_API_SECRET was unset was told their connector had
		// failed to generate, rather than that the service was not
		// configured to generate it.
		r.payload["error_message"] = d
	} else {
		r.payload["error_message"] = "Generation failed"
	}
}

// generationOutcome is how a run ended, as its row records it.
type generationOutcome struct {
	status           string
	version          string
	validationPassed sql.NullBool
	toolCount        sql.NullInt64
	err              string
}

func outcomeOf(res generatorResult) generationOutcome {
	if !res.succeeded() {
		msg := res.failure
		if msg == "" {
			res.normalizeFailure()
			msg, _ = res.payload["error_message"].(string)
		}
		// The error is shown to Ask rsync.ai's model and to agents, so it is
		// scrubbed of literals and cut short, as a run's error is.
		msg = llmscrub.Scrub(msg)
		if r := []rune(msg); len(r) > maxGenerationErrRunes {
			msg = string(r[:maxGenerationErrRunes])
		}
		if strings.TrimSpace(msg) == "" {
			msg = "Generation failed"
		}
		return generationOutcome{status: "failed", err: msg}
	}
	out := generationOutcome{status: "succeeded"}
	meta, _ := res.payload["metadata"].(map[string]any)
	if v, ok := res.payload["version"].(string); ok && v != "" {
		out.version = v
	} else if v, ok := meta["version_dir"].(string); ok {
		out.version = v
	}
	if len(out.version) > 32 {
		out.version = out.version[:32]
	}
	out.validationPassed = reportedValidation(res.payload)
	if n, ok := res.payload["operation_count"].(float64); ok && n >= 0 {
		out.toolCount = sql.NullInt64{Int64: int64(n), Valid: true}
	} else if ops, ok := meta["operations"].([]any); ok {
		out.toolCount = sql.NullInt64{Int64: int64(len(ops)), Valid: true}
	}
	return out
}

// reportedValidation reads the check the generator made, from the fields its
// /v1/generate contract has (GenerateResponseV1 has no validation_passed): the
// fast and spec-first paths report a "preflight" stage, the agentic path a
// "Validation" stage, and an agentic draft sets is_draft. No such stage is no
// answer, not a pass.
func reportedValidation(payload map[string]any) sql.NullBool {
	if draft, _ := payload["is_draft"].(bool); draft {
		return sql.NullBool{Bool: false, Valid: true}
	}
	stages, _ := payload["workflow_stages"].([]any)
	for _, s := range stages {
		stage, _ := s.(map[string]any)
		name, _ := stage["name"].(string)
		if n := strings.ToLower(name); n != "preflight" && n != "validation" {
			continue
		}
		status, _ := stage["status"].(string)
		switch status {
		case "completed":
			return sql.NullBool{Bool: true, Valid: true}
		case "failed":
			return sql.NullBool{Bool: false, Valid: true}
		}
	}
	return sql.NullBool{}
}

// finishGenerationRun records how a run ended. It uses its own short context,
// so a run that hit its limit is still recorded.
func finishGenerationRun(runID string, res generatorResult) {
	database := db.GetDB()
	if database == nil || runID == "" {
		return
	}
	out := outcomeOf(res)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx,
		`UPDATE connector_generation_runs
		 SET status = $2, version = NULLIF($3, ''), validation_passed = $4, tool_count = $5,
		     error = NULLIF($6, ''), finished_at = NOW()
		 WHERE id = $1 AND status = 'running'`,
		runID, out.status, out.version, out.validationPassed, out.toolCount, out.err,
	); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("connector generation: could not record how the run ended")
		return
	}
	log.WithFields(log.Fields{"run_id": runID, "status": out.status}).Info("connector generation run finished")
}

// SweepStaleConnectorGenerations fails runs a stopped process left running.
func SweepStaleConnectorGenerations(ctx context.Context) (int64, error) {
	database := db.GetDB()
	if database == nil {
		return 0, errors.New("database unavailable")
	}
	res, err := database.ExecContext(ctx, staleGenerationSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunConnectorGenerationSweeper sweeps at startup and then every
// generationSweepEvery until ctx ends, so a run interrupted by a restart reads
// failed within minutes of its limit, whether or not another run is started.
func RunConnectorGenerationSweeper(ctx context.Context) {
	t := time.NewTicker(generationSweepEvery)
	defer t.Stop()
	for {
		if n, err := SweepStaleConnectorGenerations(ctx); err != nil {
			log.WithError(err).Warn("connector generation sweep failed")
		} else if n > 0 {
			log.WithField("runs", n).Warn("connector generation sweep: marked interrupted runs failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// GetConnectorGeneration returns one generation run of the active workspace.
// GET /api/v1/connectors/generations/:id
func GetConnectorGeneration(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSViewer); !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database_unavailable"})
		return
	}
	var (
		run      GenerationRun
		valid    sql.NullBool
		tools    sql.NullInt64
		finished sql.NullTime
	)
	err := database.QueryRowContext(c.Request.Context(),
		`SELECT id, connector_name, status, COALESCE(source_url, ''), actor_kind, COALESCE(version, ''),
		        validation_passed, tool_count, COALESCE(error, ''), started_at, finished_at
		 FROM connector_generation_runs WHERE id = $1 AND workspace_id = $2`, id, ws,
	).Scan(&run.ID, &run.ConnectorName, &run.Status, &run.SourceURL, &run.ActorKind, &run.Version,
		&valid, &tools, &run.Error, &run.StartedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("connector generation: read run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error"})
		return
	}
	if valid.Valid {
		v := valid.Bool
		run.ValidationPassed = &v
	}
	if tools.Valid {
		n := int(tools.Int64)
		run.ToolCount = &n
	}
	if finished.Valid {
		t := finished.Time
		run.FinishedAt = &t
	}
	c.JSON(http.StatusOK, run)
}

// ConnectorProvenance says that an AI agent generated a connector, and when.
// By names the agent token, or "Ask rsync.ai", and only to the workspace whose
// run it was.
type ConnectorProvenance struct {
	GeneratedByAgent bool      `json:"generated_by_agent"`
	GeneratedAt      time.Time `json:"generated_at"`
	By               string    `json:"by,omitempty"`
}

// connectorProvenance reads the latest succeeded run of a connector. It
// returns nil when the connector was not generated by an agent or Ask rsync.ai
// (a person regenerating it on the Generate page clears the line), or on any
// error: the line is a courtesy, never a reason to fail the page.
func connectorProvenance(ctx context.Context, name, workspaceID string) *ConnectorProvenance {
	database := db.GetDB()
	if database == nil || name == "" {
		return nil
	}
	var (
		kind, ws, tokenName string
		at                  time.Time
	)
	err := database.QueryRowContext(ctx,
		`SELECT r.actor_kind, r.workspace_id::text, COALESCE(t.name, ''), r.finished_at
		 FROM connector_generation_runs r LEFT JOIN agent_tokens t ON t.id = r.token_id
		 WHERE r.connector_name = $1 AND r.status = 'succeeded' AND r.finished_at IS NOT NULL
		 ORDER BY r.finished_at DESC LIMIT 1`, name,
	).Scan(&kind, &ws, &tokenName, &at)
	if err != nil || (kind != actorKindAgent && kind != actorKindAssistant) {
		return nil
	}
	p := &ConnectorProvenance{GeneratedByAgent: true, GeneratedAt: at}
	if workspaceID != "" && ws == workspaceID {
		if kind == actorKindAssistant {
			p.By = "Ask rsync.ai"
		} else {
			p.By = tokenName
		}
	}
	return p
}
