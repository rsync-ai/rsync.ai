package handlers

// Running a workflow (migration 130): run its one read-only query on its
// connection, decide its condition, fill its message and send it to each of its
// delivery channels. A run is a WorkflowRunWorkflow on temporal-adapter's
// `workflow-runs` queue (workflow_runs_temporal.go, design § 4.1); its steps
// call api-gateway, which holds the explorer's query executors and the
// notifier's transports, so the query and the sends still run here.
//
// Every run re-checks, at run time and against the database, what was true
// when the workflow was turned on: the run-as user is still an owner or admin
// of the workspace, the connection still exists, and the SQL is still one read
// statement. A refusal that will hold next time pauses the workflow with the
// reason, the way a model schedule auto-pauses (authorizeModelRun).
//
// A run records outcomes only (row count, whether the condition held, each
// delivery's status). The rows go to the delivery channels and nowhere else:
// not to the run row, not to a log line, not to a model. The rendered message
// waits for its sends in Redis, encrypted, and is deleted when the run ends.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"api-gateway/internal/email"
	"api-gateway/internal/notifier"
	"api-gateway/internal/security"
	"api-gateway/internal/validators"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	"github.com/rsync-ai/shared/sqlexec"
	log "github.com/sirupsen/logrus"
)

const (
	// workflowMinRole is the role a run-as user needs at run time, the same
	// floor as a model run (modelRunMinRole): an unattended, repeating query
	// whose result leaves rsync.ai is an administrative act.
	workflowMinRole = security.WSAdmin
	// workflowQueryRowLimit is the explorer's own cap.
	workflowQueryRowLimit = 500
	workflowQueryTimeout  = 90 * time.Second
	workflowErrorMax      = 1000

	deliveryStatusSent    = "sent"
	deliveryStatusFailed  = "failed"
	deliveryStatusSkipped = "skipped"
)

// Seams for tests: the Slack transport and the query executor.
var (
	postSlack     = notifier.PostSlackText
	workflowQuery = runWorkflowQuery
)

// DeliveryOutcome is one channel's result in a run.
type DeliveryOutcome struct {
	// StepID is the send step that sent it.
	StepID    string `json:"step_id,omitempty"`
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	// Sent is how many emails were accepted, for an email channel.
	Sent  int    `json:"sent,omitempty"`
	Error string `json:"error,omitempty"`
}

// renderedMessage is one run's message in every format a channel needs.
type renderedMessage struct {
	Subject string
	Slack   string
	Text    string
	HTML    string
	// Images are the charts the message shows (workflow_chart_step.go).
	Images []messageImage `json:",omitempty"`
	// Files are attached after an email's text: a dashboard's PDF
	// (workflow_dashboard_step.go).
	Files []messageFile `json:",omitempty"`
}

func htmlEscape(s string) string { return html.EscapeString(s) }

func workflowURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/")
	if base == "" {
		base = "http://localhost:3000"
	}
	return base + "/workflows/" + id
}

// renderWorkflowMessage fills a branch's message for each format. The notes
// name any {{placeholder}} the result could not fill.
func renderWorkflowMessage(name string, m workflow.Message, res workflow.Result, out workflow.Outcome,
	link string, now time.Time) (renderedMessage, []string) {
	return renderWorkflowMessageWithDrawings(name, m, res, out, link, nil, now)
}

// renderWorkflowMessageWithDrawings is renderWorkflowMessage with the charts and
// dashboards the send runs after below the body, before the footer; an empty
// drawing for a step that drew nothing.
func renderWorkflowMessageWithDrawings(name string, m workflow.Message, res workflow.Result, out workflow.Outcome,
	link string, drawings []workflowDrawing, now time.Time) (renderedMessage, []string) {
	vars := workflow.Vars{WorkflowName: name, RunTime: now, Link: link, Result: res, Value: out.Value}
	subject, unknownSubj := workflow.RenderSubject(m.Subject, vars)
	if subject == "" {
		subject = "[rsync.ai] " + workflow.CapRunes(name, workflow.EmailSubjectMax-11)
	}
	slackBody, unknown := workflow.Render(m.Body, vars, workflow.FormatSlack)
	textBody, _ := workflow.Render(m.Body, vars, workflow.FormatText)
	htmlBody, _ := workflow.Render(m.Body, vars, workflow.FormatHTML)

	footerText := fmt.Sprintf("Sent by the rsync.ai workflow %q: %s", name, link)
	cs := renderWorkflowDrawings(drawings)
	slackCharts := ""
	if cs.Slack != "" {
		slackCharts = "\n\n" + cs.Slack
	}
	// Slack ends with the workflow's link, unless the message already shows it.
	slackLink := "\n" + link
	if workflow.UsesSlot(m.Subject, "link") || workflow.UsesSlot(m.Body, "link") {
		slackLink = ""
	}
	msg := renderedMessage{
		Subject: subject,
		Slack: workflow.CapRunes("*"+slackEscapeText(subject)+"*\n"+slackBody,
			max(workflow.SlackMessageMax-utf8Len(slackLink)-3-utf8Len(slackCharts), 1)) + slackCharts + slackLink,
		Text: workflow.CapRunes(textBody, workflow.EmailBodyCharsMax) + cs.Text + "\n\n--\n" + footerText,
		HTML: `<div style="font-family:sans-serif;font-size:14px;line-height:1.5">` +
			workflow.CapRunes(htmlBody, workflow.EmailBodyCharsMax) + cs.HTML +
			`<p style="color:#71717a;font-size:12px;margin-top:24px">Sent by the rsync.ai workflow <a href="` +
			html.EscapeString(link) + `">` + html.EscapeString(name) + `</a></p></div>`,
		Images: cs.Images,
		Files:  cs.Files,
	}
	var notes []string
	if workflow.UsesSlot(m.Subject, "row_count") || workflow.UsesSlot(m.Body, "row_count") {
		notes = append(notes, fmt.Sprintf("{{row_count}} is %d here: how many rows the query returned, not a count "+
			"or total in the data.", res.RowCount))
	}
	seen := map[string]bool{}
	for _, u := range append(unknownSubj, unknown...) {
		if !seen[u] {
			seen[u] = true
			notes = append(notes, fmt.Sprintf("%s is not a column of the result, so it was left empty.", u))
		}
	}
	return msg, notes
}

func slackEscapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func utf8Len(s string) int { return len([]rune(s)) }

// deliverToChannel sends msg through one channel.
func deliverToChannel(ctx context.Context, database *sql.DB, ch *sendChannel, msg renderedMessage) DeliveryOutcome {
	out := DeliveryOutcome{ChannelID: ch.ID, Name: ch.Name, Kind: ch.Kind}
	fail := func(err error) DeliveryOutcome {
		out.Status = deliveryStatusFailed
		out.Error = workflow.CapRunes(err.Error(), 500)
		return out
	}
	switch ch.Kind {
	case deliveryKindSlack:
		if ch.SecretErr != nil {
			return fail(ch.SecretErr)
		}
		if err := postSlack(ctx, ch.WebhookURL, msg.Slack); err != nil {
			return fail(err)
		}
		out.Status = deliveryStatusSent
		return out
	case deliveryKindEmail:
		sent, err := sendWorkflowEmail(ctx, database, ch.Recipients, msg)
		out.Sent = sent
		if err != nil {
			if sent > 0 {
				err = fmt.Errorf("%d of %d addresses received it: %w", sent, len(ch.Recipients), err)
			}
			return fail(err)
		}
		out.Status = deliveryStatusSent
		return out
	default:
		return fail(fmt.Errorf("unknown channel kind %q", ch.Kind))
	}
}

// errEmailNotSetUp is what an email channel reports on an rsync.ai with no
// mail transport.
var errEmailNotSetUp = errors.New("email is not set up on this rsync.ai: set RESEND_API_KEY and RESEND_FROM_ADDR, " +
	"or configure SMTP in Admin > Notifications")

// sendWorkflowEmail emails each recipient: through Resend (HTML, its charts in
// place) when the server has a Resend key, else through the admin's SMTP (plain
// text, its charts attached). A dashboard's PDF is attached either way.
func sendWorkflowEmail(ctx context.Context, database *sql.DB, to []string, msg renderedMessage) (int, error) {
	resend := email.New()
	useResend := resend.IsConfigured()
	if !useResend && !notifier.SMTPConfigured(ctx, database) {
		return 0, errEmailNotSetUp
	}
	sent := 0
	var firstErr error
	for _, addr := range to {
		var err error
		if useResend {
			err = resend.SendWithAttachments(ctx, addr, msg.Subject, msg.HTML,
				append(resendImages(msg.Images), resendFiles(msg.Files)...))
		} else {
			err = notifier.SendSMTPText(ctx, database, addr, msg.Subject, msg.Text,
				append(smtpImages(msg.Images), smtpFiles(msg.Files)...)...)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent++
	}
	return sent, firstErr
}

// resendImages are a message's charts as Resend attachments, each shown in
// place through its content id.
func resendImages(imgs []messageImage) []email.Attachment {
	var out []email.Attachment
	for _, im := range imgs {
		out = append(out, email.Attachment{Filename: im.Filename, Content: im.PNG, ContentType: "image/png",
			ContentID: im.ContentID})
	}
	return out
}

// smtpImages are a message's charts as files after a plain-text email.
func smtpImages(imgs []messageImage) []notifier.Attachment {
	var out []notifier.Attachment
	for _, im := range imgs {
		out = append(out, notifier.Attachment{Filename: im.Filename, ContentType: "image/png", Content: im.PNG})
	}
	return out
}

// resendFiles are a message's files as Resend attachments, after its text (no
// content id).
func resendFiles(files []messageFile) []email.Attachment {
	var out []email.Attachment
	for _, f := range files {
		out = append(out, email.Attachment{Filename: f.Filename, Content: f.Content, ContentType: f.ContentType})
	}
	return out
}

// smtpFiles are a message's files as attachments after a plain-text email.
func smtpFiles(files []messageFile) []notifier.Attachment {
	var out []notifier.Attachment
	for _, f := range files {
		out = append(out, notifier.Attachment{Filename: f.Filename, ContentType: f.ContentType, Content: f.Content})
	}
	return out
}

// runWorkflowQuery runs one read statement on a workspace connection through
// the explorer's executors, with the explorer's per-column masking of personal
// columns. The caller has already checked the role; this re-checks that the
// SQL is a single read for that role and refuses anything else.
func runWorkflowQuery(ctx context.Context, database *sql.DB, workspaceID, connectionID, sqlText string,
	role security.WorkspaceRole) (workflow.Result, int64, error) {
	if reason := workflowSQLRefusal(sqlText, role); reason != "" {
		return workflow.Result{}, 0, errors.New(reason)
	}
	var connectorType, configEncrypted string
	err := database.QueryRowContext(ctx, `
		SELECT connector_type, config FROM connections WHERE id = $1 AND workspace_id = $2
	`, connectionID, workspaceID).Scan(&connectorType, &configEncrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return workflow.Result{}, 0, errors.New("the workflow's connection no longer exists in this workspace")
	}
	if err != nil {
		return workflow.Result{}, 0, fmt.Errorf("load the connection: %w", err)
	}
	if reason := workflowConnectionRefusal(connectorType); reason != "" {
		return workflow.Result{}, 0, errors.New(reason)
	}
	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		return workflow.Result{}, 0, errors.New("the connection's saved credentials could not be decrypted")
	}
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return workflow.Result{}, 0, errors.New("the connection's saved settings could not be read")
	}

	qctx, cancel := context.WithTimeout(ctx, workflowQueryTimeout)
	defer cancel()
	start := time.Now()
	var res *ExplorerQueryResponse
	eng, err := explorerEngine(connectorType, config)
	if err == nil {
		res, err = eng.Query(qctx, sqlText, nil, sqlexec.Limits{Rows: workflowQueryRowLimit, Mask: true})
	}
	ms := time.Since(start).Milliseconds()
	if err != nil {
		errorType, _ := validators.ClassifyExecutionError(err)
		return workflow.Result{}, ms, fmt.Errorf("the query failed (%s): %s", errorType,
			workflow.CapRunes(err.Error(), workflowErrorMax))
	}
	if res == nil {
		return workflow.Result{}, ms, errors.New("the query returned no result")
	}
	return workflow.Result{Columns: res.Columns, Rows: res.Rows, RowCount: res.RowCount, Truncated: res.Truncated}, ms, nil
}

// workflowSQLRefusal says why sqlText is not a workflow query for role: it
// must be exactly one statement, and a read.
func workflowSQLRefusal(sqlText string, role security.WorkspaceRole) string {
	if v := validators.ValidateExplorerStatement(sqlText, role); v == nil || !v.Valid {
		msg := "it is not a single valid statement"
		if v != nil && v.ErrorMessage != "" {
			msg = v.ErrorMessage
		}
		return "A workflow's query must be one read-only SELECT: " + msg + "."
	}
	if class := validators.ClassifyStatementSQL(sqlText); class != validators.ClassRead {
		return fmt.Sprintf("A workflow's query must be one read-only SELECT; this one is a %s statement. "+
			"A workflow never changes data.", strings.ReplaceAll(string(class), "_", " "))
	}
	return ""
}

// workflowConnectionRefusal says why a connection cannot be queried by a
// workflow: it must be one the SQL explorer can query.
func workflowConnectionRefusal(connectorType string) string {
	ecap := ResolveExplorerCapability(connectorType)
	if !ecap.Supported || ecap.QueryLanguage != langSQL {
		return fmt.Sprintf("Workflows run SQL, and %s is not a SQL connection the explorer can query.",
			getFriendlyName(connectorType))
	}
	return ""
}

// workflowRunRow is what a run needs from its workflow.
type workflowRunRow struct {
	RunID     string
	Trigger   string
	StartedBy string
	// RunStatus is the run's own status; Status is the workflow's.
	RunStatus    string
	WorkflowID   string
	WorkspaceID  string
	Name         string
	Status       string
	Definition   workflow.Definition
	ConnectionID string
	RunAsUserID  string
	// StopRequested is a person's Stop (migration 140): the run ends at its next step.
	StopRequested bool
}

// loadWorkflowRun reads a run and the definition it started with
// (workflow_runs.definition, migration 131): an edit saved since does not
// change a run already queued. Its connection is the one its query step
// names, still in the workspace; a run from before migration 131 has no copy
// and runs the workflow's own definition.
func loadWorkflowRun(ctx context.Context, database *sql.DB, runID string) (*workflowRunRow, error) {
	var (
		r                  workflowRunRow
		raw                []byte
		startedBy, runAsID sql.NullString
	)
	err := database.QueryRowContext(ctx, `
		SELECT r.id, r.trigger, r.started_by, r.status, w.id, w.workspace_id, w.name, w.status,
		       COALESCE(r.definition, w.definition), w.run_as_user_id, r.stop_requested_at IS NOT NULL
		FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id
		WHERE r.id = $1
	`, runID).Scan(&r.RunID, &r.Trigger, &startedBy, &r.RunStatus, &r.WorkflowID, &r.WorkspaceID, &r.Name, &r.Status, &raw,
		&runAsID, &r.StopRequested)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &r.Definition); err != nil {
		return nil, fmt.Errorf("read the workflow definition: %w", err)
	}
	r.StartedBy, r.RunAsUserID = startedBy.String, runAsID.String
	if id := r.Definition.Query().ConnectionID; id != "" {
		err := database.QueryRowContext(ctx, `
			SELECT id FROM connections WHERE workspace_id = $1 AND id::text = $2
		`, r.WorkspaceID, id).Scan(&r.ConnectionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return &r, nil
}

// runOutcome is what a run records: at evaluate when it ends there, else at
// finish (workflow_runs_temporal.go).
type runOutcome struct {
	status       string
	conditionMet *bool
	// branch is the 0-based branch that sends; nil when none held.
	branch     *int
	rowCount   *int
	deliveries []DeliveryOutcome
	notes      []string
	err        string
	// pause is a reason that will still hold next time: the workflow pauses.
	pause string
	// failedSteps are the steps whose failure failed a graph run (failingSteps),
	// none when no step did.
	failedSteps []string
}

// sendPlan is what a run whose condition held sends: one message, to each
// channel of the send step of the branch that held. The message carries
// values from the rows, so it is kept only in Redis, encrypted, until the
// sends are done.
type sendPlan struct {
	stepID   string
	channels []*sendChannel
	msg      renderedMessage
}

// workflowNeedsNewAdapter refuses a run of a step-5 definition that reached
// the endpoints of a temporal-adapter from before step 5.
const workflowNeedsNewAdapter = "This workflow has steps that only the updated temporal-adapter runs; " +
	"rsync.ai's scheduler needs updating before it can run."

// evaluateWorkflowRun is a run up to its sends: the checks, the query, the
// condition and the message. A nil plan means the run ends here with the
// outcome; a plan means it sends, and its outcome is recorded at finish. It
// serves a run of a temporal-adapter from before step 5, so only slice 1's
// shape (workflow.Definition.IsLegacyShape).
func evaluateWorkflowRun(ctx context.Context, database *sql.DB, run *workflowRunRow) (runOutcome, *sendPlan) {
	failed := func(msg string) runOutcome { return runOutcome{status: "failed", err: msg} }
	if !run.Definition.IsLegacyShape() {
		return failed(workflowNeedsNewAdapter), nil
	}

	actorID := workflowRunActor(run)
	if actorID == "" {
		o := failed("The workflow has no run-as user. An owner or admin should turn it on again.")
		o.pause = o.err
		return o, nil
	}
	role, err := resolveWorkspaceRoleForUser(ctx, database, run.WorkspaceID, actorID)
	if err != nil {
		return failed("rsync.ai could not check the run-as user's role; the next run tries again."), nil
	}
	if refusal := workflowRoleRefusal(role); refusal != "" {
		o := failed(refusal)
		o.pause = o.err
		return o, nil
	}
	if run.ConnectionID == "" {
		o := failed("The workflow's connection was deleted.")
		o.pause = o.err
		return o, nil
	}
	sqlText := run.Definition.Query().SQL
	if reason := workflowSQLRefusal(sqlText, role); reason != "" {
		o := failed(reason)
		o.pause = reason
		return o, nil
	}

	res, _, err := workflowQuery(ctx, database, run.WorkspaceID, run.ConnectionID, sqlText, role)
	if err != nil {
		return failed(err.Error()), nil
	}
	rowCount := res.RowCount
	branches := run.Definition.Branches()
	dec, err := workflow.Decide(branches, res)
	if err != nil {
		o := failed("The condition could not be decided: " + err.Error() + ".")
		o.rowCount = &rowCount
		return o, nil
	}
	met := dec.Branch >= 0
	o := runOutcome{status: "succeeded", conditionMet: &met, rowCount: &rowCount, deliveries: []DeliveryOutcome{}}
	o.notes = append(o.notes, dec.Notes...)
	if !met {
		return o, nil
	}
	branch := dec.Branch
	o.branch = &branch
	several := len(branches) > 1
	stepID := run.Definition.CaseSend(branch).ID
	// Quiet minutes hold per send step: a lower branch firing does not
	// silence a higher one.
	if q := run.Definition.QuietMinutes; q > 0 {
		if recent, err := workflowSentRecently(ctx, database, run.WorkflowID, run.RunID, stepID, q); err == nil && recent {
			held := "The condition was true"
			if several {
				held = fmt.Sprintf("Condition %d was true", branch+1)
			}
			o.notes = append(o.notes, fmt.Sprintf(
				"%s, but it sent a message less than %d minutes ago, so this one was not.", held, q))
			return o, nil
		}
	}

	channels, err := loadWorkflowSendChannels(ctx, database, run.WorkspaceID, run.WorkflowID, stepID)
	if err != nil {
		o.status, o.err = "failed", "The workflow's delivery channels could not be loaded."
		return o, nil
	}
	if len(channels) == 0 {
		o.status, o.err = "failed", "The workflow has no delivery channel to send to."
		if several {
			o.err = fmt.Sprintf("Condition %d has no delivery channel to send to.", branch+1)
		}
		return o, nil
	}
	msg, notes := renderWorkflowMessage(run.Name, branches[branch].Message, res, dec.Outcome,
		workflowURL(run.WorkflowID), time.Now())
	o.notes = append(o.notes, notes...)
	return o, &sendPlan{stepID: stepID, channels: channels, msg: msg}
}

// workflowSentRecently says whether another run of the workflow sent from
// send step stepID in the last minutes (workflow_runs.deliveries; migration
// 140 gave the rows from before step 5 their step ids).
func workflowSentRecently(ctx context.Context, database *sql.DB, workflowID, runID, stepID string,
	minutes int) (bool, error) {
	var recent bool
	err := database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workflow_runs
			WHERE workflow_id = $1 AND id <> $2
			  AND deliveries @> jsonb_build_array(jsonb_build_object('status', 'sent', 'step_id', $4::text))
			  AND started_at > NOW() - make_interval(mins => $3)
		)`, workflowID, runID, minutes, stepID).Scan(&recent)
	return recent, err
}

// workflowRunActor is whose role a run is checked against: whoever clicked
// Run now, else the workflow's run-as user.
func workflowRunActor(run *workflowRunRow) string {
	if run.Trigger == workflow.TriggerManual && run.StartedBy != "" {
		return run.StartedBy
	}
	return run.RunAsUserID
}

// workflowRoleRefusal says why role may not run a workflow; "" means it may.
func workflowRoleRefusal(role security.WorkspaceRole) string {
	if role == "" || !role.Meets(workflowMinRole) {
		return "The person this workflow runs as is no longer an owner or admin of the workspace. " +
			"An owner or admin should turn it on again."
	}
	return ""
}

// loadWorkflowSendChannels loads one send step's channels with secrets
// decrypted.
func loadWorkflowSendChannels(ctx context.Context, database *sql.DB, ws, workflowID string,
	stepID string) ([]*sendChannel, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT dc.id FROM workflow_channels wc
		JOIN delivery_channels dc ON dc.id = wc.channel_id AND dc.workspace_id = $1
		WHERE wc.workflow_id = $2 AND wc.node_id = $3
		ORDER BY wc.position, lower(dc.name)
	`, ws, workflowID, stepID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*sendChannel, 0, len(ids))
	for _, id := range ids {
		ch, err := loadChannelForSend(ctx, database, ws, id)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// recordWorkflowRun records a run that ends at evaluate, once: only a run
// still running with no sends planned is recorded (a run that sends ends at
// finish). It reports whether this call recorded it.
func recordWorkflowRun(ctx context.Context, database *sql.DB, runID string, o runOutcome) (bool, error) {
	deliveries, _ := json.Marshal(o.deliveries)
	if o.deliveries == nil {
		deliveries = []byte("[]")
	}
	var errText *string
	if o.err != "" {
		e := workflow.CapRunes(o.err, workflowErrorMax)
		errText = &e
	}
	notes := o.notes
	if notes == nil {
		notes = []string{}
	}
	res, err := database.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status = $2, condition_met = $3, row_count = $4, deliveries = $5, notes = $6, error = $7,
		    branch = $8, finished_at = NOW()
		WHERE id = $1 AND status = 'running'
		  AND NOT EXISTS (SELECT 1 FROM workflow_sends s WHERE s.run_id = workflow_runs.id)
	`, runID, o.status, o.conditionMet, o.rowCount, string(deliveries), pgdriver.StringArray(notes), errText,
		o.branch)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// pauseWorkflowForRun pauses a workflow for a reason that will hold next time,
// the way a model schedule auto-pauses. A run someone clicked never pauses it:
// they can see the answer.
func pauseWorkflowForRun(ctx context.Context, database *sql.DB, run *workflowRunRow, reason string) {
	if reason == "" || run.Trigger == workflow.TriggerManual {
		return
	}
	fail := func(err error) {
		log.WithError(err).WithField("workflow_id", run.WorkflowID).Error("[workflows] could not pause")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE workflows SET status = 'paused', paused_reason = $2, next_run_at = NULL, updated_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, run.WorkflowID, reason)
	if err != nil {
		fail(err)
		return
	}
	// Under the row's lock, as every schedule change (beginScheduledWorkflowRun).
	if n, _ := res.RowsAffected(); n > 0 {
		dropWorkflowSchedule(ctx, run.WorkflowID)
	}
	if err := tx.Commit(); err != nil {
		fail(err)
	}
}

// auditWorkflowRun writes one audit_logs row for a run that sent anything:
// which channels, and whether each send went out. Never the message.
func auditWorkflowRun(ctx context.Context, database *sql.DB, run *workflowRunRow, o runOutcome) {
	if len(o.deliveries) == 0 {
		return
	}
	type sent struct {
		Channel string `json:"channel"`
		Kind    string `json:"kind"`
		Status  string `json:"status"`
	}
	var list []sent
	for _, d := range o.deliveries {
		list = append(list, sent{Channel: d.Name, Kind: d.Kind, Status: d.Status})
	}
	details, _ := json.Marshal(map[string]interface{}{
		"run_id": run.RunID, "trigger": run.Trigger, "deliveries": list, "row_count": o.rowCount,
	})
	actor := workflowRunActor(run)
	var actorParam interface{} = sql.NullString{}
	if actor != "" {
		actorParam = actor
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details, workspace_id, actor_kind)
		VALUES ($1, 'workflow_send', 'workflow', $2, $3, $4, 'workflow')
	`, actorParam, run.WorkflowID, string(details), run.WorkspaceID); err != nil {
		log.WithError(err).WithField("run_id", run.RunID).Warn("[workflows] could not audit run")
	}
}
