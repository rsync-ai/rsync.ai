package handlers

// The OpenLineage export (design § 5.2): the pull, the push target and its auth header,
// and the push's delivery: retries, redirects, private addresses and what it records.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
)

const olAuthCanary = "Bearer unit-test-openlineage-canary-91c2"

func olCryptoEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", "unit-test-openlineage-key-0123456789abcdef")
}

// olTimeArg matches a time.Time bound to a query by instant.
type olTimeArg time.Time

func (a olTimeArg) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	return ok && got.Equal(time.Time(a))
}

// olRouter serves the OpenLineage routes as a caller with this role in wsScopeWS.
func olRouter(role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/lineage/openlineage", PullOpenLineageEvents)
	r.GET("/lineage/openlineage/target", GetOpenLineageTarget)
	r.PUT("/lineage/openlineage/target", PutOpenLineageTarget)
	r.DELETE("/lineage/openlineage/target", DeleteOpenLineageTarget)
	return r
}

func olServe(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

// olTestPusher posts without the address guard (httptest listens on loopback), keeping
// the pusher's own redirect rule.
func olTestPusher(database func() *sql.DB) *openLineagePusher {
	p := newOpenLineagePusher(database)
	p.client.Transport = http.DefaultTransport
	p.backoff = time.Millisecond
	return p
}

func TestValidateOpenLineageTargetURL(t *testing.T) {
	for _, ok := range []string{
		"https://lineage.example.com/api/v1/lineage",
		"http://marquez.internal:5000/api/v1/lineage",
	} {
		if err := validateOpenLineageTargetURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "ftp://lineage.example.com/x", "https://user:pw@lineage.example.com/x", "/api/v1/lineage",
		"https://", "javascript:alert(1)", "lineage.example.com/x",
		"https://lineage.example.com/" + strings.Repeat("a", openLineageTargetURLMax),
	} {
		if err := validateOpenLineageTargetURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := validateOpenLineageTargetURL("https://u:p@h.example.com/"); err == nil || !strings.Contains(err.Error(), "auth header") {
		t.Errorf("user info: %v", err)
	}
}

func TestValidateOpenLineageAuthHeader(t *testing.T) {
	for _, ok := range []string{"", "Bearer abc", "Basic dXNlcjpwYXNz", strings.Repeat("a", openLineageAuthHeaderMax)} {
		if err := validateOpenLineageAuthHeader(ok); err != nil {
			t.Errorf("%.20q: %v", ok, err)
		}
	}
	for _, bad := range []string{"Bearer a\r\nX-Evil: 1", "Bearer a\nb", "Bearer \x00", strings.Repeat("a", openLineageAuthHeaderMax+1)} {
		if err := validateOpenLineageAuthHeader(bad); err == nil {
			t.Errorf("%.20q accepted", bad)
		}
	}
}

func TestOpenLineageCursorAndTimes(t *testing.T) {
	ended := time.Date(2026, 10, 14, 9, 0, 0, 123456789, time.FixedZone("x", 3600))
	id := "e0000000-0000-4000-8000-0000000000e1"
	gotT, gotID, ok := decodeOpenLineageCursor(encodeOpenLineageCursor(ended, id))
	if !ok || !gotT.Equal(ended) || gotID != id {
		t.Errorf("round trip = %v %q %v", gotT, gotID, ok)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{"%%%", enc("no separator"), enc("2026-10-14T09:00:00Z|not-a-uuid"), enc("yesterday|" + id), ""} {
		if _, _, ok := decodeOpenLineageCursor(bad); ok {
			t.Errorf("cursor %q accepted", bad)
		}
	}

	if v, ok := parseOpenLineageTime(""); !ok || v.Valid {
		t.Errorf(`"" = %v %v`, v, ok)
	}
	if v, ok := parseOpenLineageTime("2026-10-14T09:00:00+02:00"); !ok || !v.Valid || !v.Time.Equal(leT0.Add(-2*time.Hour)) {
		t.Errorf("RFC 3339 = %v %v", v, ok)
	}
	for _, bad := range []string{"2026-10-14", "yesterday", "1760432400"} {
		if _, ok := parseOpenLineageTime(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPGStringListScan(t *testing.T) {
	for _, tc := range []struct {
		src  any
		want []string
	}{
		{[]byte("{a,b}"), []string{"a", "b"}},
		{`{"a","b"}`, []string{"a", "b"}},
		{"{}", []string{}},
		{nil, nil},
	} {
		var l pgStringList
		if err := l.Scan(tc.src); err != nil || (l == nil) != (tc.want == nil) || strings.Join(l, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Scan(%v) = %#v %v, want %#v", tc.src, l, err, tc.want)
		}
	}
	var l pgStringList
	if err := l.Scan(42); err == nil {
		t.Error("an int scanned")
	}
}

// post: what it sends, what it retries and what it records.
func TestOpenLineagePost_DeliveryOutcomes(t *testing.T) {
	type hit struct {
		auth, contentType, body string
	}
	serve := func(codes ...int) (*httptest.Server, *[]hit) {
		var mu sync.Mutex
		hits := &[]hit{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			n := len(*hits)
			*hits = append(*hits, hit{r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)})
			mu.Unlock()
			code := codes[min(n, len(codes)-1)]
			if code == http.StatusFound {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte("a body that is never kept"))
		}))
		t.Cleanup(srv.Close)
		return srv, hits
	}
	p := olTestPusher(nil)
	body := []byte(`{"eventType":"COMPLETE"}`)

	srv, hits := serve(http.StatusServiceUnavailable, http.StatusOK)
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL, AuthHeader: olAuthCanary}, body); status != openLineageStatusDelivered || code != 200 {
		t.Errorf("503 then 200 = %s %d", status, code)
	}
	if len(*hits) != 2 || (*hits)[1] != (hit{olAuthCanary, "application/json", string(body)}) {
		t.Errorf("hits = %+v", *hits)
	}

	srv, hits = serve(http.StatusBadRequest)
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL}, body); status != openLineageStatusFailed || code != 400 || len(*hits) != 1 {
		t.Errorf("400 = %s %d after %d attempts", status, code, len(*hits))
	}
	if (*hits)[0].auth != "" {
		t.Errorf("an Authorization header with none set: %q", (*hits)[0].auth)
	}

	srv, hits = serve(http.StatusTooManyRequests)
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL}, body); status != openLineageStatusFailed || code != 429 || len(*hits) != openLineagePushAttempts {
		t.Errorf("429 = %s %d after %d attempts", status, code, len(*hits))
	}

	// A redirect is answered as it is: the auth header goes nowhere else.
	srv, hits = serve(http.StatusFound)
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL + "/lineage", AuthHeader: olAuthCanary}, body); status != openLineageStatusFailed || code != http.StatusFound || len(*hits) != 1 {
		t.Errorf("302 = %s %d after %d requests", status, code, len(*hits))
	}

	srv, _ = serve(http.StatusOK)
	srv.Close()
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL}, body); status != openLineageStatusFailed || code != 0 {
		t.Errorf("no server = %s %d", status, code)
	}
}

// The pusher's own client refuses a private address before it connects.
func TestOpenLineagePost_RefusesAPrivateAddress(t *testing.T) {
	if os.Getenv("SAFEHTTP_ALLOW_HOSTS") != "" {
		t.Skip("SAFEHTTP_ALLOW_HOSTS lets hosts past the guard")
	}
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer srv.Close()
	p := newOpenLineagePusher(nil)
	p.backoff = time.Millisecond
	if status, code := p.post(context.Background(), openLineageTarget{URL: srv.URL, AuthHeader: olAuthCanary}, []byte("{}")); status != openLineageStatusRefused || code != 0 {
		t.Errorf("loopback = %s %d", status, code)
	}
	if reached {
		t.Error("the request reached a loopback server")
	}
}

// push: from the event ids to a POST of each event to its workspace's target, and the
// outcome recorded on the target.
func TestOpenLineagePush_SendsTheEventAndRecordsDelivery(t *testing.T) {
	olCryptoEnv(t)
	var mu sync.Mutex
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	authEnc, _ := crypto.EncryptString(olAuthCanary)
	cfgPlain, _ := json.Marshal(map[string]any{"host": "db.example.com", "database": "shop", "password": "SECRET-PW", "connection_string": "postgres://u:SECRET-PW@db.example.com/shop"})
	cfgEnc, _ := crypto.Encrypt(cfgPlain)
	result, _ := json.Marshal(savedColumnLineage{Columns: []string{"id"}, Edges: []columnLineageEdge{
		{Table: []string{"analytics", "orders"}, Column: "id", ToColumn: "id", Type: "DIRECT", Subtype: "IDENTITY"}}})
	const eventID = "e0000000-0000-4000-8000-0000000000e1"

	mock.ExpectQuery(`array_agg\(e\.id::text`).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"ws", "ids", "url", "enc"}).AddRow(leWS, "{"+eventID+"}", srv.URL, authEnc))
	mock.ExpectQuery(`LEFT JOIN saved_queries sq`).
		WithArgs(leWS, sqlmock.AnyArg(), nil, nil, nil, "00000000-0000-0000-0000-000000000000", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "ws", "kind", "job", "name", "run", "type", "started", "ended", "in", "out", "hash", "conn"}).
			AddRow(eventID, leWS, "model", leModel, "orders_enriched", "run-1", "COMPLETE", leT0, leT0.Add(time.Minute),
				[]byte(`[{"connection_id":"`+leWH+`","name":"analytics.orders"}]`),
				[]byte(`[{"connection_id":"`+leWH+`","name":"marts.orders_enriched"}]`), "h1", leWH))
	mock.ExpectQuery(`FROM model_column_lineage`).WithArgs(leWS, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "hash", "status", "result"}).AddRow(leModel, "h1", "ok", result))
	mock.ExpectQuery(`FROM connections`).WithArgs(leWS, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "config"}).AddRow(leWH, "postgresql", cfgEnc))
	mock.ExpectExec(`UPDATE openlineage_targets`).WithArgs(leWS, srv.URL, openLineageStatusDelivered, 201).
		WillReturnResult(sqlmock.NewResult(0, 1))

	olTestPusher(nil).push(context.Background(), mockDB, []string{eventID})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != olAuthCanary {
		t.Errorf("Authorization = %q", gotAuth)
	}
	for _, want := range []string{`"runId":"` + eventID + `"`, `"namespace":"postgres://db.example.com:5432"`,
		`"name":"shop.analytics.orders"`, `"name":"shop.marts.orders_enriched"`, `"columnLineage"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("the event lacks %s: %s", want, gotBody)
		}
	}
	if strings.Contains(gotBody, "SECRET-PW") || strings.Contains(gotBody, olAuthCanary) {
		t.Errorf("a credential is in the event: %s", gotBody)
	}
}

// PUT stores the auth header encrypted and never shows it: not in the response, not in
// the audit row.
func TestPutOpenLineageTarget_StoresTheAuthHeaderEncrypted(t *testing.T) {
	olCryptoEnv(t)
	const targetURL = "https://lineage.example.com/api/v1/lineage"
	readBack := func(mock sqlmock.Sqlmock, hasAuth bool) {
		mock.ExpectQuery(`SELECT url, auth_header_enc <> ''`).WithArgs(wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"url", "has", "enabled", "status", "code", "delivered", "updated"}).
				AddRow(targetURL, hasAuth, true, "", nil, nil, leT0))
	}
	for _, tc := range []struct {
		label, auth, change string
		stored              bool // a ciphertext is stored
	}{
		{"set", `,"auth_header":" ` + olAuthCanary + ` "`, "set", true},
		{"kept", ``, "kept", false},
		{"cleared", `,"auth_header":""`, "cleared", false},
	} {
		mock, cleanup := withMockDB(t)
		var enc, details string
		mock.ExpectExec(`INSERT INTO openlineage_targets`).
			WithArgs(wsScopeWS, targetURL, captureJSONArg(&enc), true, wsScopeUser, tc.change == "kept").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`INSERT INTO audit_logs`).
			WithArgs(wsScopeUser, "openlineage_target.update", "workspace", sqlmock.AnyArg(), captureJSONArg(&details), sqlmock.AnyArg(), wsScopeWS).
			WillReturnResult(sqlmock.NewResult(0, 1))
		readBack(mock, tc.stored)

		w := olServe(olRouter("admin"), http.MethodPut, "/lineage/openlineage/target", `{"url":" `+targetURL+` "`+tc.auth+`}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.label, w.Code, w.Body)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if tc.stored {
			if enc == "" || strings.Contains(enc, "canary") {
				t.Errorf("%s: stored %q", tc.label, enc)
			}
			if plain, err := crypto.DecryptString(enc); err != nil || plain != olAuthCanary {
				t.Errorf("%s: the stored header decrypts to %q (%v)", tc.label, plain, err)
			}
		} else if enc != "" {
			t.Errorf("%s: stored %q", tc.label, enc)
		}
		if !strings.Contains(details, `"auth_header":"`+tc.change+`"`) || strings.Contains(details, "canary") {
			t.Errorf("%s: audit details %s", tc.label, details)
		}
		if strings.Contains(w.Body.String(), "canary") || (enc != "" && strings.Contains(w.Body.String(), enc)) {
			t.Errorf("%s: the response shows the header: %s", tc.label, w.Body)
		}
		var v openLineageTargetView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || !v.Configured || v.HasAuthHeader != tc.stored || v.URL != targetURL {
			t.Errorf("%s: view %+v (%v)", tc.label, v, err)
		}
		cleanup()
	}
}

func TestOpenLineageTargetRoutes_RefuseBeforeTheDatabase(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	for _, tc := range []struct {
		role, method, path, body string
		want                     int
	}{
		{"editor", http.MethodGet, "/lineage/openlineage", "", http.StatusForbidden},
		{"viewer", http.MethodGet, "/lineage/openlineage/target", "", http.StatusForbidden},
		{"editor", http.MethodPut, "/lineage/openlineage/target", `{"url":"https://h.example.com/"}`, http.StatusForbidden},
		{"viewer", http.MethodDelete, "/lineage/openlineage/target", "", http.StatusForbidden},
		{"admin", http.MethodPut, "/lineage/openlineage/target", `{"url":"file:///etc/passwd"}`, http.StatusBadRequest},
		{"admin", http.MethodPut, "/lineage/openlineage/target", `{"url":"https://u:p@h.example.com/"}`, http.StatusBadRequest},
		{"admin", http.MethodPut, "/lineage/openlineage/target", `{"url":"https://h.example.com/","auth_header":"Bearer a\r\nX: b"}`, http.StatusBadRequest},
		{"admin", http.MethodGet, "/lineage/openlineage?since=yesterday", "", http.StatusBadRequest},
		{"admin", http.MethodGet, "/lineage/openlineage?cursor=bm9wZQ", "", http.StatusBadRequest},
	} {
		if w := olServe(olRouter(tc.role), tc.method, tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s %s = %d %s, want %d", tc.role, tc.method, tc.path, w.Code, w.Body, tc.want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteOpenLineageTarget_Audited(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectExec(`DELETE FROM openlineage_targets`).WithArgs(wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(wsScopeUser, "openlineage_target.delete", "workspace", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := olServe(olRouter("admin"), http.MethodDelete, "/lineage/openlineage/target", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The pull pages oldest first: a full page names the next one, which starts after its
// last event; a short page is the last.
func TestPullOpenLineageEvents_PagesByCursor(t *testing.T) {
	olCryptoEnv(t)
	mock, cleanup := withMockDB(t)
	defer cleanup()
	cols := []string{"id", "ws", "kind", "job", "name", "run", "type", "started", "ended", "in", "out", "hash", "conn"}
	in := []byte(`[{"connection_id":"` + leSource + `","name":"orders"}]`)
	out := []byte(`[{"connection_id":"` + leWH + `","name":"orders"}]`)
	ids := []string{"e0000000-0000-4000-8000-0000000000e1", "e0000000-0000-4000-8000-0000000000e2", "e0000000-0000-4000-8000-0000000000e3"}
	ends := []time.Time{leT0, leT0.Add(time.Minute), leT0.Add(2 * time.Minute)}
	connRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "type", "config"}).AddRow(leSource, "mysql", "").AddRow(leWH, "postgresql", "")
	}

	mock.ExpectQuery(`LEFT JOIN saved_queries sq`).
		WithArgs(wsScopeWS, sqlmock.AnyArg(), olTimeArg(leT0.Add(-time.Hour)), nil, nil, "00000000-0000-0000-0000-000000000000", 2).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(ids[0], wsScopeWS, "pipeline", lePipeline, "orders sync", ids[0], "COMPLETE", nil, ends[0], in, out, "", "").
			AddRow(ids[1], wsScopeWS, "pipeline", lePipeline, "orders sync", ids[1], "FAIL", ends[0], ends[1], in, out, "", ""))
	mock.ExpectQuery(`FROM connections`).WillReturnRows(connRows())

	r := olRouter("admin")
	w := olServe(r, http.MethodGet, "/lineage/openlineage?limit=2&since=2026-10-14T08:00:00Z", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("page 1 = %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("page 1 has %d lines: %s", len(lines), w.Body)
	}
	var second openLineageRunEvent
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil || second.EventType != "FAIL" || second.Run.RunID != ids[1] ||
		second.Inputs[0].Namespace != openLineageRsyncNamespace+leSource {
		t.Errorf("line 2 = %s (%v)", lines[1], err)
	}
	cursor := w.Header().Get(OpenLineageNextCursorHeader)
	if cursor == "" {
		t.Fatal("a full page has no next cursor")
	}

	mock.ExpectQuery(`LEFT JOIN saved_queries sq`).
		WithArgs(wsScopeWS, sqlmock.AnyArg(), nil, nil, olTimeArg(ends[1]), ids[1], 2).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(ids[2], wsScopeWS, "pipeline", lePipeline, "orders sync", ids[2], "COMPLETE", ends[1], ends[2], in, out, "", ""))
	mock.ExpectQuery(`FROM connections`).WillReturnRows(connRows())
	w = olServe(r, http.MethodGet, "/lineage/openlineage?limit=2&cursor="+cursor, "")
	if w.Code != http.StatusOK || w.Header().Get(OpenLineageNextCursorHeader) != "" || strings.Count(w.Body.String(), "\n") != 1 {
		t.Fatalf("page 2 = %d cursor %q %s", w.Code, w.Header().Get(OpenLineageNextCursorHeader), w.Body)
	}

	// A limit over the maximum is the maximum.
	mock.ExpectQuery(`LEFT JOIN saved_queries sq`).
		WithArgs(wsScopeWS, sqlmock.AnyArg(), nil, nil, nil, sqlmock.AnyArg(), openLineagePullMaxLimit).
		WillReturnRows(sqlmock.NewRows(cols))
	if w = olServe(r, http.MethodGet, "/lineage/openlineage?limit=50000", ""); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("empty = %d %s", w.Code, w.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A model run that was skipped read and wrote nothing, so what leaves is the run, with no
// dataset. The events recorded before the recorder knew it carry the tables of the SQL as
// it stood; they are not sent as the run's reads and writes, and their column lineage is
// not looked up. A cancelled pipeline run keeps its tables.
func TestPullOpenLineageEvents_ASkippedModelRunLeavesWithoutDatasets(t *testing.T) {
	olCryptoEnv(t)
	mock, cleanup := withMockDB(t)
	defer cleanup()
	cols := []string{"id", "ws", "kind", "job", "name", "run", "type", "started", "ended", "in", "out", "hash", "conn"}
	in := []byte(`[{"connection_id":"` + leSource + `","name":"orders"}]`)
	out := []byte(`[{"connection_id":"` + leWH + `","name":"orders"}]`)
	mock.ExpectQuery(`LEFT JOIN saved_queries sq`).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("e0000000-0000-4000-8000-0000000000e1", wsScopeWS, "model", leModel, "orders_enriched", "run-1", "ABORT", leT0, leT0.Add(time.Minute), in, out, "abc123", leWH).
			AddRow("e0000000-0000-4000-8000-0000000000e2", wsScopeWS, "pipeline", lePipeline, "orders sync", "exec-1", "ABORT", leT0, leT0.Add(2*time.Minute), in, out, "", ""))
	// No model_column_lineage query: the skipped run has no output to carry columns.
	mock.ExpectQuery(`FROM connections`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "config"}).AddRow(leSource, "mysql", "").AddRow(leWH, "postgresql", ""))

	w := olServe(olRouter("admin"), http.MethodGet, "/lineage/openlineage", "")
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if w.Code != http.StatusOK || len(lines) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var skipped, cancelled openLineageRunEvent
	if err := json.Unmarshal([]byte(lines[0]), &skipped); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &cancelled); err != nil {
		t.Fatal(err)
	}
	if skipped.EventType != "ABORT" || len(skipped.Inputs) != 0 || len(skipped.Outputs) != 0 {
		t.Errorf("a skipped model run leaves as %s with inputs %+v, outputs %+v", skipped.EventType, skipped.Inputs, skipped.Outputs)
	}
	if cancelled.EventType != "ABORT" || len(cancelled.Inputs) != 1 || len(cancelled.Outputs) != 1 {
		t.Errorf("a cancelled pipeline run leaves with inputs %+v, outputs %+v", cancelled.Inputs, cancelled.Outputs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// enqueue is called from the recorder, so a full queue drops the ids rather than wait.
func TestOpenLineagePusherEnqueue_NeverBlocks(t *testing.T) {
	p := &openLineagePusher{queue: make(chan []string, 1)}
	p.enqueue([]string{"a"})
	done := make(chan struct{})
	go func() { p.enqueue([]string{"b"}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	if got := <-p.queue; len(got) != 1 || got[0] != "a" || len(p.queue) != 0 {
		t.Errorf("queue = %v, %d left", got, len(p.queue))
	}
}

func TestRegisterOpenLineage_RoutesAndPushHook(t *testing.T) {
	prev := lineagePushHook
	t.Cleanup(func() { lineagePushHook = prev })
	lineagePushHook = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterOpenLineage(ctx, r.Group("/api/v1"))
	got := map[string]bool{}
	for _, rt := range r.Routes() {
		got[rt.Method+" "+rt.Path] = true
	}
	for _, want := range []string{"GET /api/v1/lineage/openlineage", "GET /api/v1/lineage/openlineage/target",
		"PUT /api/v1/lineage/openlineage/target", "DELETE /api/v1/lineage/openlineage/target"} {
		if !got[want] {
			t.Errorf("no route %s", want)
		}
	}
	if len(got) != 4 {
		t.Errorf("routes = %v", got)
	}
	if lineagePushHook == nil {
		t.Error("the push hook is not set")
	}
}
