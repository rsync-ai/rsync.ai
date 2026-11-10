package retention

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Audit-log retention. audit_logs (who did what: admin actions, Explorer writes,
// workflow runs that sent anything) and connection_access_logs (every credential
// read) only ever grew; nothing deleted a row. They are now kept
// RSYNC_AUDIT_LOG_RETENTION_DAYS (default 365) and trimmed once a day.
//
// Always on, in cloud and OSS alike; RSYNC_AUDIT_LOG_RETENTION_DAYS=0 keeps every
// row. Safe with several api-gateway replicas: every chunk takes its rows FOR UPDATE
// SKIP LOCKED. Both tables are read newest first (admin_audit.go, admin_users.go),
// and nothing derives state from an old row, so an aged-out row is simply gone from
// the admin Audit page. Deleting rows does not shrink the table's file; it makes the
// space reusable, so the table stops growing.

const (
	auditLogRetentionInterval = 24 * time.Hour
	auditLogRetentionChunk    = 5000
	// auditLogRetentionBudget bounds one tick, so a first run over years of rows is
	// worked off over several days instead of one long burst of writes.
	auditLogRetentionBudget = 5 * time.Minute
	auditLogRetentionPause  = 100 * time.Millisecond
	// defaultAuditLogRetentionDays keeps an audit row this long.
	defaultAuditLogRetentionDays = 365
)

type AuditLogRetentionConfig struct {
	// RetentionDays keeps a row this many days after it was written; 0 keeps them all.
	RetentionDays int
	Interval      time.Duration
	Chunk         int
	Budget        time.Duration
	Pause         time.Duration
}

func LoadAuditLogRetentionConfig() AuditLogRetentionConfig {
	cfg := AuditLogRetentionConfig{
		RetentionDays: defaultAuditLogRetentionDays,
		Interval:      auditLogRetentionInterval,
		Chunk:         auditLogRetentionChunk,
		Budget:        auditLogRetentionBudget,
		Pause:         auditLogRetentionPause,
	}
	if v, ok := os.LookupEnv("RSYNC_AUDIT_LOG_RETENTION_DAYS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.RetentionDays = n
		} else {
			log.Warnf("audit-log retention: RSYNC_AUDIT_LOG_RETENTION_DAYS=%q is not a whole number of days; keeping %d", v, cfg.RetentionDays)
		}
	}
	return cfg
}

func StartAuditLogRetention(ctx context.Context, database *sql.DB) {
	if database == nil {
		log.Warn("audit-log retention: DB is nil; skipping")
		return
	}
	cfg := LoadAuditLogRetentionConfig()
	if cfg.RetentionDays == 0 {
		log.Info("audit-log retention: RSYNC_AUDIT_LOG_RETENTION_DAYS=0, keeping every audit row")
		return
	}
	go func() {
		RunAuditLogRetentionOnce(ctx, database, cfg)

		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunAuditLogRetentionOnce(ctx, database, cfg)
			}
		}
	}()
}

// AuditLogRetentionResult is what one tick did.
type AuditLogRetentionResult struct {
	AuditRows       int64
	AccessRows      int64
	BudgetExhausted bool
	Err             error
}

// The created_at indexes (migration 021 idx_audit_logs_created_at, migration 001
// idx_connection_access_logs_created_at) serve the age scan. A row with no
// created_at is never aged out.
const (
	pruneAuditLogsQuery = `
WITH del AS (
	SELECT id FROM audit_logs
	WHERE created_at < NOW() - make_interval(days => $1)
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
DELETE FROM audit_logs WHERE id IN (SELECT id FROM del)`

	pruneConnectionAccessLogsQuery = `
WITH del AS (
	SELECT id FROM connection_access_logs
	WHERE created_at < NOW() - make_interval(days => $1)
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
DELETE FROM connection_access_logs WHERE id IN (SELECT id FROM del)`
)

// RunAuditLogRetentionOnce trims both tables within cfg.Budget. Best-effort: an
// error ends the tick, and the next tick starts over.
func RunAuditLogRetentionOnce(ctx context.Context, database *sql.DB, cfg AuditLogRetentionConfig) AuditLogRetentionResult {
	var res AuditLogRetentionResult
	if cfg.RetentionDays <= 0 {
		return res
	}

	deadline := time.Now().Add(cfg.Budget)
	var done bool
	res.AuditRows, done, res.Err = pruneByAge(ctx, database, pruneAuditLogsQuery, cfg, deadline)
	if res.Err == nil && done {
		res.AccessRows, _, res.Err = pruneByAge(ctx, database, pruneConnectionAccessLogsQuery, cfg, deadline)
	}
	res.BudgetExhausted = !time.Now().Before(deadline)
	if res.Err != nil {
		log.WithError(res.Err).Warn("audit-log retention: prune stopped")
	}

	if res.AuditRows > 0 || res.AccessRows > 0 || res.BudgetExhausted {
		log.WithFields(log.Fields{
			"deleted_audit_logs":             res.AuditRows,
			"deleted_connection_access_logs": res.AccessRows,
			"retention_days":                 cfg.RetentionDays,
			"budget_exhausted":               res.BudgetExhausted,
		}).Info("audit-log retention tick complete")
	}
	return res
}

// pruneByAge runs query in chunks until a short chunk (done) or the budget or
// context runs out.
func pruneByAge(ctx context.Context, database *sql.DB, query string, cfg AuditLogRetentionConfig, deadline time.Time) (int64, bool, error) {
	var deleted int64
	for {
		r, err := database.ExecContext(ctx, query, cfg.RetentionDays, cfg.Chunk)
		if err != nil {
			return deleted, false, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return deleted, false, err
		}
		deleted += n
		if n < int64(cfg.Chunk) {
			return deleted, true, nil
		}
		if !pauseUntil(ctx, cfg.Pause, deadline) {
			return deleted, false, nil
		}
	}
}
