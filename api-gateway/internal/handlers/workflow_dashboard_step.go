package handlers

// A workflow's dashboard step (docs/internal/dashboards-design.md § 8, dashboards D3 PR 3).
// It draws every tile of a saved dashboard as the run's actor, as the board stands
// saved (no filters picked), and holds what it drew for the sends after it: each chart
// tile the way the chart step writes a chart (a plotted chart's picture, a scorecard's
// number, a table's first rows), each text tile's words, and the whole board as the
// PDF its Export PDF button makes. A tile that cannot be drawn is "not drawn" in the
// message and does not fail the step; only a board that is gone, or that the run-as
// user cannot see, does.
//
// The rows go to the frontend inside the deployment (POST /api/internal/charts/png and
// /api/internal/dashboards/pdf, X-Internal-Secret) and, as pictures, words and the PDF,
// to the delivery channels. Not to a log line, not to the run's rows, not to a model.
// What the step drew waits in Redis, encrypted, and is deleted when the run ends.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/charts"

	"github.com/redis/go-redis/v9"
	"github.com/rsync-ai/shared/crypto"
	workflow "github.com/rsync-ai/shared/workflowdef"
	log "github.com/sirupsen/logrus"
)

const (
	// dashboardDrawConcurrency is how many tiles are read at once, as on the
	// dashboard's page (DashboardPage DRAW_CONCURRENCY).
	dashboardDrawConcurrency = 4
	dashboardPDFTimeout      = 90 * time.Second
	// dashboardPDFMax is the largest PDF the frontend may hand back: with the
	// pictures it stays under the 25 MB most mail servers take.
	dashboardPDFMax = 10 << 20
	// dashboardStepWriteDeadline lifts the gateway's 300 s write timeout for this
	// step's answer.
	dashboardStepWriteDeadline = 10 * time.Minute

	// The dashboard page's own words (dashboard-pdf.ts pdfTileOf).
	dashboardHiddenChart = "This chart is private to its author."
	dashboardHiddenName  = "A chart you cannot see"
	dashboardTooLong     = "This tile could not be drawn: it took too long to read"

	// dashboardNotDrawn stands in for a dashboard whose step drew nothing.
	dashboardNotDrawn = "A dashboard could not be drawn for this message; the run shows why."
)

// dashboardDrawBudget is how long the tiles may take together; a tile not read by
// then is not drawn. With the PDF's time it stays under the adapter's call timeout
// for this step (workflow_graph_run.go, 570 s). A var for tests.
var dashboardDrawBudget = 7 * time.Minute

// Seams for tests: the board, where a dashboard step's drawing waits, and the
// frontend's PDF.
var (
	loadWorkflowDashboard                           = loadVisibleDashboard
	workflowDashboards       workflowDashboardStore = redisWorkflowDashboards{}
	drawWorkflowDashboardPDF                        = frontendDashboardPDF
)

// workflowDashboard is what a dashboard step drew.
type workflowDashboard struct {
	StepID      string                  `json:"step_id"`
	DashboardID string                  `json:"dashboard_id"`
	Name        string                  `json:"name"`
	Link        string                  `json:"link"`
	ReadAt      time.Time               `json:"read_at"`
	Tiles       []workflowDashboardTile `json:"tiles"`
	PDF         []byte                  `json:"pdf,omitempty"`
	// PDFError says why the board's PDF could not be made.
	PDFError string `json:"pdf_error,omitempty"`
}

// workflowDashboardTile is one tile, in the board's reading order: a chart as the
// chart step draws it, a text tile's words, or why the tile is not drawn.
type workflowDashboardTile struct {
	Kind     string         `json:"kind"`
	Name     string         `json:"name,omitempty"`
	Body     string         `json:"body,omitempty"`
	Chart    *workflowChart `json:"chart,omitempty"`
	NotDrawn string         `json:"not_drawn,omitempty"`
}

// ---------------------------------------------------------------------------
// The step
// ---------------------------------------------------------------------------

// runDashboard draws the step's board as the run's actor and holds it for the sends
// after it.
func (s *graphStepCall) runDashboard() {
	if s.actorRefused() {
		return
	}
	// Reading every tile can outlast the server's write timeout.
	if s.c != nil {
		rc := http.NewResponseController(s.c.Writer)
		if err := rc.SetWriteDeadline(time.Now().Add(dashboardStepWriteDeadline)); err != nil {
			log.WithError(err).WithFields(s.fields()).Warn("[workflows] dashboard: could not extend the HTTP write deadline")
		}
	}
	actor := workflowRunActor(s.run)
	rec, err := loadWorkflowDashboard(s.ctx, s.db, s.run.WorkspaceID, actor, s.step.DashboardID)
	if errors.Is(err, sql.ErrNoRows) {
		s.refuse("The dashboard was deleted, or the run-as user cannot see it.", true)
		return
	}
	if err != nil {
		s.unavail(err, "could not load the dashboard")
		return
	}
	now := time.Now()
	held, draws := drawWorkflowDashboardTiles(s.ctx, s.db, s.run.WorkspaceID, actor, rec, now)
	held.StepID = s.step.ID

	var notes []string
	notDrawn, first := 0, ""
	for _, t := range held.Tiles {
		if t.NotDrawn != "" {
			if notDrawn == 0 {
				first = t.NotDrawn
			}
			notDrawn++
		}
	}
	if notDrawn > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d tiles were not drawn; the first: %s", notDrawn, len(held.Tiles), first))
	}
	pdf, err := drawWorkflowDashboardPDF(s.ctx, rec, draws, now)
	if err != nil {
		held.PDFError = workflow.CapRunes(err.Error(), workflowErrorMax)
		notes = append(notes, "The dashboard's PDF could not be made: "+held.PDFError)
	} else {
		held.PDF = pdf
	}
	if err := workflowDashboards.Put(s.ctx, workflowResultKey(s.run.RunID, s.step.ID), held,
		workflowResultTTL(s.run.Definition)); err != nil {
		s.unavail(err, "could not hold the dashboard")
		return
	}
	s.finish(workflow.StatusSucceeded, "", workflow.CapRunes(strings.Join(notes, " "), workflowErrorMax), graphStepAnswer{})
}

// dashboardTileDraw is one chart tile's answer, as the dashboard's page gets it
// from POST /api/v1/dashboards/:id/tiles/:tileId/draw: the board's PDF is laid out
// from it. Failure is set instead when the tile was not read in time.
type dashboardTileDraw struct {
	Draw    *chartDraw
	Failure string
}

// drawWorkflowDashboardTiles reads each chart tile the actor may see, a few at a time,
// and writes every tile for a message, in the board's reading order (y, then x).
func drawWorkflowDashboardTiles(ctx context.Context, database *sql.DB, ws, actor string, rec *dashboardRecord,
	now time.Time) (workflowDashboard, map[string]dashboardTileDraw) {
	tiles := append([]*dashboardTile(nil), rec.Tiles...)
	sort.SliceStable(tiles, func(i, j int) bool {
		if tiles[i].Y != tiles[j].Y {
			return tiles[i].Y < tiles[j].Y
		}
		return tiles[i].X < tiles[j].X
	})
	out := workflowDashboard{DashboardID: rec.ID, Name: rec.Name, Link: DashboardURL(rec.ID), ReadAt: now.UTC(),
		Tiles: make([]workflowDashboardTile, len(tiles))}

	draws := make([]dashboardTileDraw, len(tiles))
	var queue []int
	for i, t := range tiles {
		switch {
		case t.Kind == charts.TileText:
			out.Tiles[i] = workflowDashboardTile{Kind: charts.TileText, Body: t.Body}
		case t.Chart == nil:
			out.Tiles[i] = workflowDashboardTile{Kind: charts.TileChart, Name: dashboardHiddenName, NotDrawn: dashboardHiddenChart}
		default:
			queue = append(queue, i)
		}
	}

	dctx, cancel := context.WithTimeout(ctx, dashboardDrawBudget)
	defer cancel()
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(dashboardDrawConcurrency, len(queue)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out.Tiles[i], draws[i] = drawWorkflowDashboardTile(dctx, database, ws, actor, tiles[i].Chart, now)
				// A read the budget cut short says so, not the driver's words for it.
				if out.Tiles[i].NotDrawn != "" && dctx.Err() != nil {
					out.Tiles[i].NotDrawn = dashboardTooLong
					draws[i] = dashboardTileDraw{Failure: "it took too long to read"}
				}
			}
		}()
	}
	for _, i := range queue {
		if dctx.Err() != nil {
			out.Tiles[i] = workflowDashboardTile{Kind: charts.TileChart, Name: tiles[i].Chart.Name, NotDrawn: dashboardTooLong}
			draws[i] = dashboardTileDraw{Failure: "it took too long to read"}
			continue
		}
		select {
		case work <- i:
		case <-dctx.Done():
			out.Tiles[i] = workflowDashboardTile{Kind: charts.TileChart, Name: tiles[i].Chart.Name, NotDrawn: dashboardTooLong}
			draws[i] = dashboardTileDraw{Failure: "it took too long to read"}
		}
	}
	close(work)
	wg.Wait()

	byID := map[string]dashboardTileDraw{}
	for _, i := range queue {
		byID[tiles[i].ID] = draws[i]
	}
	return out, byID
}

// drawWorkflowDashboardTile draws one chart tile, saying in the page's words why it
// is not drawn when it is not.
func drawWorkflowDashboardTile(ctx context.Context, database *sql.DB, ws, actor string, ch *chartRecord,
	now time.Time) (workflowDashboardTile, dashboardTileDraw) {
	t := workflowDashboardTile{Kind: charts.TileChart, Name: ch.Name}
	d := drawWorkflowChart(ctx, database, ws, actor, ch, nil, now)
	answer := dashboardTileDraw{Draw: &d}
	switch {
	case d.Break != nil:
		t.NotDrawn = "This chart cannot be drawn: " + d.Break.Message
	case d.Error != "":
		t.NotDrawn = workflow.CapRunes("The source did not run: "+d.Error, workflowErrorMax)
	case d.Result == nil:
		t.NotDrawn = "This tile has no rows."
	default:
		c, err := messageChart(ctx, ch, d.Result)
		if err != nil {
			t.NotDrawn = workflow.CapRunes(err.Error(), workflowErrorMax)
			break
		}
		t.Chart = &c
	}
	return t, answer
}

// ---------------------------------------------------------------------------
// Where a dashboard waits for its sends
// ---------------------------------------------------------------------------

// workflowDashboardStore holds a dashboard step's drawing for the sends after it.
type workflowDashboardStore interface {
	Put(ctx context.Context, key string, d workflowDashboard, ttl time.Duration) error
	Get(ctx context.Context, key string) (workflowDashboard, bool, error)
	Delete(ctx context.Context, key string) error
}

// redisWorkflowDashboards keeps each drawing encrypted, under run/step.
type redisWorkflowDashboards struct{}

func workflowDashboardRedisKey(key string) string { return "workflow-run-dashboard:" + key }

func (redisWorkflowDashboards) client() (*redis.Client, error) {
	if schemaCache == nil {
		return nil, errors.New("Redis is not available")
	}
	return schemaCache.GetClient(), nil
}

func (s redisWorkflowDashboards) Put(ctx context.Context, key string, d workflowDashboard, ttl time.Duration) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(raw)
	if err != nil {
		return err
	}
	return rc.Set(ctx, workflowDashboardRedisKey(key), enc, ttl).Err()
}

func (s redisWorkflowDashboards) Get(ctx context.Context, key string) (workflowDashboard, bool, error) {
	rc, err := s.client()
	if err != nil {
		return workflowDashboard{}, false, err
	}
	enc, err := rc.Get(ctx, workflowDashboardRedisKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return workflowDashboard{}, false, nil
	}
	if err != nil {
		return workflowDashboard{}, false, err
	}
	raw, err := crypto.Decrypt(enc)
	if err != nil {
		return workflowDashboard{}, false, err
	}
	var d workflowDashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return workflowDashboard{}, false, err
	}
	return d, true, nil
}

func (s redisWorkflowDashboards) Delete(ctx context.Context, key string) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	return rc.Del(ctx, workflowDashboardRedisKey(key)).Err()
}

// ---------------------------------------------------------------------------
// The PDF: the frontend lays it out
// ---------------------------------------------------------------------------

// dashboardPDFRequest is what the frontend's internal route lays the PDF out from:
// the board's tiles, as GET /api/v1/dashboards/:id hands them to the page, and each
// chart tile's answer, as the page's draws get it.
type dashboardPDFRequest struct {
	Name        string                          `json:"name"`
	GeneratedAt time.Time                       `json:"generated_at"`
	Tiles       []*dashboardTile                `json:"tiles"`
	Draws       map[string]dashboardPDFTileDraw `json:"draws"`
}

type dashboardPDFTileDraw struct {
	Draw    *dashboardPDFDraw `json:"draw"`
	Failure *string           `json:"failure"`
}

// dashboardPDFDraw is a tile's draw without the chart's SQL, which the PDF does not show.
type dashboardPDFDraw struct {
	Chart   *chartRecord    `json:"chart"`
	Result  *chartPNGResult `json:"result"`
	Break   *chartBreak     `json:"break"`
	Badges  *chartBadges    `json:"badges"`
	Error   string          `json:"error,omitempty"`
	Filters struct {
		Applied []string `json:"applied"`
		Ignored []string `json:"ignored"`
	} `json:"filters"`
}

var dashboardPDFClient = &http.Client{Timeout: dashboardPDFTimeout}

var pdfSignature = []byte("%PDF-")

// frontendDashboardPDF asks the frontend to lay the board out with the code behind
// its Export PDF button, so the attachment is what the app exports.
func frontendDashboardPDF(ctx context.Context, rec *dashboardRecord, draws map[string]dashboardTileDraw, now time.Time) ([]byte, error) {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET"))
	if secret == "" {
		return nil, errors.New("INTERNAL_SERVICE_SECRET is not set, so the frontend will not make it")
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("FRONTEND_INTERNAL_URL")), "/")
	if base == "" {
		base = "http://frontend:3000"
	}
	in := dashboardPDFRequest{Name: rec.Name, GeneratedAt: now.UTC(), Tiles: rec.Tiles,
		Draws: map[string]dashboardPDFTileDraw{}}
	if in.Tiles == nil {
		in.Tiles = []*dashboardTile{}
	}
	for _, t := range rec.Tiles {
		got, ok := draws[t.ID]
		if !ok {
			continue
		}
		if got.Draw == nil {
			failure := got.Failure
			in.Draws[t.ID] = dashboardPDFTileDraw{Failure: &failure}
			continue
		}
		d := &dashboardPDFDraw{Chart: t.Chart, Break: got.Draw.Break, Badges: got.Draw.Badges, Error: got.Draw.Error}
		d.Filters.Applied, d.Filters.Ignored = []string{}, []string{}
		if r := got.Draw.Result; r != nil {
			masked, unfiltered := r.Masked, r.Unfiltered
			if masked == nil {
				masked = []string{}
			}
			if unfiltered == nil {
				unfiltered = []string{}
			}
			d.Result = &chartPNGResult{Columns: r.Columns, Rows: r.Rows, Truncated: r.Truncated, Masked: masked,
				Unfiltered: unfiltered}
		}
		in.Draws[t.ID] = dashboardPDFTileDraw{Draw: d}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardPDFTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/internal/dashboards/pdf", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)
	resp, err := dashboardPDFClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the frontend could not be reached: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("the frontend answered %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("the frontend answered %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/pdf") {
		return nil, fmt.Errorf("the frontend answered %q, not a PDF", ct)
	}
	pdf, err := io.ReadAll(io.LimitReader(resp.Body, dashboardPDFMax+1))
	if err != nil {
		return nil, fmt.Errorf("the PDF could not be read: %w", err)
	}
	if len(pdf) > dashboardPDFMax {
		return nil, fmt.Errorf("the PDF is over %d MB, too large to email", dashboardPDFMax>>20)
	}
	if !bytes.HasPrefix(pdf, pdfSignature) {
		return nil, errors.New("the frontend's answer is not a PDF")
	}
	return pdf, nil
}

// ---------------------------------------------------------------------------
// In the message
// ---------------------------------------------------------------------------

// messageFile is a file an email carries as an attachment, after its text.
type messageFile struct {
	Filename    string
	ContentType string
	Content     []byte
}

// dashboardFilename is a board's PDF's name, as its Export PDF names it
// (export/filename.ts): its name, then the day it was read.
func dashboardFilename(name string, at time.Time, seen map[string]bool) string {
	slug := strings.Trim(chartFileUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	if slug == "" {
		slug = "dashboard"
	}
	base := slug + "-" + at.UTC().Format("2006-01-02")
	file := base + ".pdf"
	for n := 2; seen[file]; n++ {
		file = fmt.Sprintf("%s-%d.pdf", base, n)
	}
	seen[file] = true
	return file
}

func dashboardReadText(d *workflowDashboard) string {
	return "Data read " + d.ReadAt.UTC().Format("2006-01-02 15:04") + " UTC"
}

// dashboardSlack is a board in a Slack message, within budget runes: its name
// linked, when it was read, and its scorecards' numbers; the rest is on the board.
func dashboardSlack(d *workflowDashboard, budget int) string {
	head := chartSlackHead(&workflowChart{Name: d.Name, Link: d.Link}, budget)
	lines := []string{head, "_" + dashboardReadText(d) + "_"}
	used := utf8Len(head) + 1 + utf8Len(lines[1])
	rest := 0
	for _, t := range d.Tiles {
		if t.Chart == nil || t.Chart.Text.Number == "" {
			if t.Kind == charts.TileChart {
				rest++
			}
			continue
		}
		line := "• " + slackEscapeText(t.Chart.Name) + ": *" + slackEscapeText(t.Chart.Text.Number) + "*"
		if t.Chart.Text.Change != "" {
			line += " (" + slackEscapeText(t.Chart.Text.Change) + ")"
		}
		// Room for this line and the count of the tiles after it.
		if used+1+utf8Len(line)+60 > budget {
			rest++
			continue
		}
		lines = append(lines, line)
		used += 1 + utf8Len(line)
	}
	if rest > 0 {
		lines = append(lines, fmt.Sprintf("_%d more tile(s): open the dashboard to see them._", rest))
	}
	return workflow.CapRunes(strings.Join(lines, "\n"), budget)
}

// dashboardPlainText is a board in a plain-text email.
func dashboardPlainText(d *workflowDashboard, imgs map[int]*messageImage, file *messageFile) string {
	lines := []string{d.Name, dashboardReadText(d), "Open the dashboard: " + d.Link}
	parts := []string{strings.Join(lines, "\n")}
	if len(d.Tiles) == 0 {
		parts = append(parts, "This dashboard has no tiles yet.")
	}
	for i, t := range d.Tiles {
		switch {
		case t.Chart != nil:
			parts = append(parts, chartPlainText(t.Chart, imgs[i]))
		case t.Kind == charts.TileText:
			parts = append(parts, t.Body)
		default:
			parts = append(parts, t.Name+"\n"+t.NotDrawn)
		}
	}
	parts = append(parts, dashboardPDFLine(d, file))
	return strings.Join(parts, "\n\n")
}

func dashboardPDFLine(d *workflowDashboard, file *messageFile) string {
	if file != nil {
		return "The whole dashboard is attached as " + file.Filename + "."
	}
	if d.PDFError != "" {
		return "The dashboard's PDF could not be made: " + d.PDFError
	}
	return "The dashboard's PDF could not be made."
}

// dashboardHTML is a board in an HTML email: its name, when it was read and a
// button to it, then each tile in reading order.
func dashboardHTML(d *workflowDashboard, imgs map[int]*messageImage, file *messageFile) string {
	esc := html.EscapeString
	link := esc(d.Link)
	var b strings.Builder
	b.WriteString(`<div style="margin-top:24px;padding:16px;border:1px solid #e4e4e7;border-radius:8px">`)
	b.WriteString(`<p style="margin:0;font-size:18px;font-weight:600"><a href="` + link +
		`" style="color:#18181b;text-decoration:none">` + esc(d.Name) + `</a></p>`)
	b.WriteString(`<p style="margin:4px 0 0;color:#71717a;font-size:12px">` + esc(dashboardReadText(d)) + `</p>`)
	b.WriteString(`<p style="margin:12px 0 0"><a href="` + link + `" style="display:inline-block;padding:6px 12px;` +
		`border-radius:6px;background:#18181b;color:#ffffff;text-decoration:none;font-size:13px">Open dashboard</a></p>`)
	if len(d.Tiles) == 0 {
		b.WriteString(`<p style="margin:24px 0 0;color:#71717a">This dashboard has no tiles yet.</p>`)
	}
	for i, t := range d.Tiles {
		switch {
		case t.Chart != nil:
			b.WriteString(chartHTML(t.Chart, imgs[i]))
		case t.Kind == charts.TileText:
			b.WriteString(`<p style="margin:24px 0 0">` + strings.ReplaceAll(esc(t.Body), "\n", "<br>") + `</p>`)
		default:
			b.WriteString(`<div style="margin-top:24px"><p style="margin:0 0 8px;font-weight:600">` + esc(t.Name) +
				`</p><p style="margin:0;color:#71717a">` + esc(t.NotDrawn) + `</p></div>`)
		}
	}
	b.WriteString(`<p style="margin:24px 0 0;color:#71717a;font-size:12px">` + esc(dashboardPDFLine(d, file)) + `</p></div>`)
	return b.String()
}
