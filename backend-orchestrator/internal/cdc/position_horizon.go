package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PositionHorizon is how long a CDC connector's stored source position stays readable on
// a source that purges its change log on a retention clock whether or not anyone has read
// it: the MySQL/MariaDB binlog and SQL Server's change tables. It is the opposite risk to a
// PostgreSQL slot, which makes the source KEEP its WAL: here the source's disk is safe and
// it is the pipeline's position that runs out. Once the log behind the position is purged,
// Start cannot resume and every change since is lost to a Reload.
//
// Measuring it reads the source's catalogue only (log index, retention settings, the LSN
// time map) and never a row value.
type PositionHorizon struct {
	// Safe: nothing will purge the position as things stand — retention is off, or the
	// position is in the binlog file the source is still writing.
	Safe bool
	// Purged: the log behind the position is already gone, so starting cannot bring it back.
	Purged bool
	// Remaining is the time until the source may purge the position, the earliest estimate
	// (negative once past it). Meaningful only when neither Safe nor Purged.
	Remaining time.Duration
	// Retention is the source's retention setting.
	Retention time.Duration
	// RetentionAssumed marks a retention the source would not report, so its default is used.
	RetentionAssumed bool
}

// PositionHorizon measures a MySQL/MariaDB binlog position. eventTime is the time of the
// last event the connector read (Debezium's ts_sec offset key).
func (m *MySQLManager) PositionHorizon(ctx context.Context, connectionID string, pos BinlogPosition, eventTime time.Time) (PositionHorizon, error) {
	cfg, err := m.getDecryptedConnectionConfig(ctx, connectionID)
	if err != nil {
		return PositionHorizon{}, fmt.Errorf("failed to get connection config: %w", err)
	}
	db, err := connectToMySQL(cfg)
	if err != nil {
		return PositionHorizon{}, fmt.Errorf("failed to connect to source MySQL: %w", err)
	}
	defer db.Close()
	return mysqlPositionHorizonDB(ctx, db, pos, eventTime, time.Now())
}

// mysqlPositionHorizonDB: MySQL expires a binlog file by its age once it is no longer the
// one being written, and never purges the active file. A file's age is not readable over
// SQL, so the purge is estimated from the position's own event time — at the earliest,
// since the file was last written at or after that event.
func mysqlPositionHorizonDB(ctx context.Context, db *sql.DB, pos BinlogPosition, eventTime, now time.Time) (PositionHorizon, error) {
	if pos.IsZero() {
		return PositionHorizon{}, fmt.Errorf("stored position has no binlog file")
	}
	_, order, err := readBinlogIndex(ctx, db)
	if err != nil {
		return PositionHorizon{}, err
	}
	idx := -1
	for i, f := range order {
		if f == strings.TrimSpace(pos.File) {
			idx = i
			break
		}
	}
	switch {
	case idx < 0:
		return PositionHorizon{Purged: true}, nil
	case idx == len(order)-1:
		return PositionHorizon{Safe: true}, nil
	}

	secs, err := binlogRetentionSeconds(ctx, db)
	if err != nil {
		return PositionHorizon{}, err
	}
	if secs <= 0 {
		return PositionHorizon{Safe: true}, nil
	}
	if eventTime.IsZero() {
		return PositionHorizon{}, fmt.Errorf("stored position has no event time")
	}
	retention := time.Duration(secs) * time.Second
	return PositionHorizon{Retention: retention, Remaining: eventTime.Add(retention).Sub(now)}, nil
}

// binlogRetentionSeconds reads the source's automatic binlog expiry, 0 when there is none:
// binlog_expire_logs_seconds (MySQL 8.0+, MariaDB 10.6+), else expire_logs_days.
func binlogRetentionSeconds(ctx context.Context, db *sql.DB) (int, error) {
	var name, val string
	if err := db.QueryRowContext(ctx, "SHOW VARIABLES LIKE 'binlog_expire_logs_seconds'").Scan(&name, &val); err == nil {
		if secs, ok := parseRetentionNumber(val); ok && secs > 0 {
			return int(secs), nil
		}
	} else if err != sql.ErrNoRows {
		return 0, fmt.Errorf("cannot read binlog_expire_logs_seconds: %w", err)
	}
	if err := db.QueryRowContext(ctx, "SHOW VARIABLES LIKE 'expire_logs_days'").Scan(&name, &val); err == nil {
		// MariaDB takes a fractional number of days.
		if days, ok := parseRetentionNumber(val); ok && days > 0 {
			return int(days * 86400), nil
		}
	} else if err != sql.ErrNoRows {
		return 0, fmt.Errorf("cannot read expire_logs_days: %w", err)
	}
	return 0, nil
}

func parseRetentionNumber(v string) (float64, bool) {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err != nil {
		return 0, false
	}
	return f, true
}

// SQLServerDefaultCleanupRetention is the CDC cleanup job's retention when it was never
// changed: 4320 minutes (3 days).
const SQLServerDefaultCleanupRetention = 4320 * time.Minute

// PositionHorizon measures a SQL Server position. commitLSN is Debezium's commit_lsn
// offset key ("0000002b:00000768:0005").
func (m *SQLServerManager) PositionHorizon(ctx context.Context, connectionID, commitLSN string) (PositionHorizon, error) {
	cfg, err := m.getDecryptedConnectionConfig(ctx, connectionID)
	if err != nil {
		return PositionHorizon{}, fmt.Errorf("failed to get connection config: %w", err)
	}
	db, err := connectToSQLServer(cfg)
	if err != nil {
		return PositionHorizon{}, fmt.Errorf("failed to connect to source SQL Server: %w", err)
	}
	defer db.Close()
	return sqlserverPositionHorizonDB(ctx, db, commitLSN)
}

// sqlserverPositionHorizonDB: the cleanup job deletes change rows, and their entries in
// cdc.lsn_time_mapping, once they are older than its retention. The position's age is taken
// on the server, whose clock and time zone the map is written in; no mapping entry at or
// below the position means the cleanup has already passed it.
func sqlserverPositionHorizonDB(ctx context.Context, db *sql.DB, commitLSN string) (PositionHorizon, error) {
	lsn, err := debeziumLSNHex(commitLSN)
	if err != nil {
		return PositionHorizon{}, err
	}
	var ageSecs sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT DATEDIFF(SECOND, MAX(tran_end_time), GETDATE())
		FROM cdc.lsn_time_mapping
		WHERE start_lsn <= CONVERT(binary(10), @p1, 1)`, lsn).Scan(&ageSecs); err != nil {
		return PositionHorizon{}, fmt.Errorf("cannot read cdc.lsn_time_mapping: %w", err)
	}
	if !ageSecs.Valid {
		return PositionHorizon{Purged: true}, nil
	}

	retention, assumed := SQLServerDefaultCleanupRetention, true
	var minutes sql.NullInt64
	// msdb is not readable for every CDC user, nor on Azure SQL Database; the default then.
	if err := db.QueryRowContext(ctx, `
		SELECT retention FROM msdb.dbo.cdc_jobs
		WHERE database_id = DB_ID() AND job_type = N'cleanup'`).Scan(&minutes); err == nil && minutes.Valid && minutes.Int64 > 0 {
		retention, assumed = time.Duration(minutes.Int64)*time.Minute, false
	}
	age := time.Duration(ageSecs.Int64) * time.Second
	return PositionHorizon{Retention: retention, RetentionAssumed: assumed, Remaining: retention - age}, nil
}

// debeziumLSNHex turns Debezium's "0000002b:00000768:0005" into the style-1 literal
// CONVERT(binary(10), …, 1) takes: "0x0000002B000007680005".
func debeziumLSNHex(lsn string) (string, error) {
	parts := strings.Split(strings.TrimSpace(lsn), ":")
	if len(parts) != 3 || len(parts[0]) != 8 || len(parts[1]) != 8 || len(parts[2]) != 4 {
		return "", fmt.Errorf("not a SQL Server LSN: %q", lsn)
	}
	hex := strings.ToUpper(strings.Join(parts, ""))
	for _, c := range hex {
		if !strings.ContainsRune("0123456789ABCDEF", c) {
			return "", fmt.Errorf("not a SQL Server LSN: %q", lsn)
		}
	}
	return "0x" + hex, nil
}
