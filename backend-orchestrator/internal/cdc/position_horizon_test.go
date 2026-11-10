package cdc

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A source that purges its change log on a clock takes a stopped pipeline's position with
// it. These pin how that clock is read, so the WAL watchdog starts the pipeline before the
// purge — and never for a position that cannot be purged or is already gone.

func binlogIndex(files ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"Log_name", "File_size", "Encrypted"})
	for _, f := range files {
		rows.AddRow(f, int64(1000), "No")
	}
	return rows
}

func variable(name, val string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow(name, val)
}

func TestMySQLPositionHorizon(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	older := BinlogPosition{File: "mysql-bin.000002", Pos: 4}
	week := 7 * 24 * time.Hour

	cases := []struct {
		name    string
		pos     BinlogPosition
		evt     time.Time
		mock    func(sqlmock.Sqlmock)
		want    PositionHorizon
		wantErr bool
	}{
		{
			name: "rotated file: purge estimated from the event time plus retention",
			pos:  older, evt: now.Add(-6 * 24 * time.Hour),
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnRows(variable("binlog_expire_logs_seconds", "604800"))
			},
			want: PositionHorizon{Retention: week, Remaining: 24 * time.Hour},
		},
		{
			name: "file is the one being written: never purged",
			pos:  BinlogPosition{File: "mysql-bin.000003"}, evt: now.Add(-30 * 24 * time.Hour),
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
			},
			want: PositionHorizon{Safe: true},
		},
		{
			name: "file no longer in the index: already purged",
			pos:  BinlogPosition{File: "mysql-bin.000001"}, evt: now,
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
			},
			want: PositionHorizon{Purged: true},
		},
		{
			name: "no automatic expiry: safe",
			pos:  older, evt: now.Add(-90 * 24 * time.Hour),
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnRows(variable("binlog_expire_logs_seconds", "0"))
				m.ExpectQuery("expire_logs_days").WillReturnRows(variable("expire_logs_days", "0"))
			},
			want: PositionHorizon{Safe: true},
		},
		{
			name: "MariaDB fractional expire_logs_days",
			pos:  older, evt: now.Add(-6 * time.Hour),
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
				m.ExpectQuery("expire_logs_days").WillReturnRows(variable("expire_logs_days", "0.500000"))
			},
			want: PositionHorizon{Retention: 12 * time.Hour, Remaining: 6 * time.Hour},
		},
		{
			name: "past the horizon but still listed: negative remaining, not purged",
			pos:  older, evt: now.Add(-8 * 24 * time.Hour),
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnRows(variable("binlog_expire_logs_seconds", "604800"))
			},
			want: PositionHorizon{Retention: week, Remaining: -24 * time.Hour},
		},
		{
			name: "unreadable retention is an error, not unlimited",
			pos:  older, evt: now,
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnError(sql.ErrConnDone)
			},
			wantErr: true,
		},
		{
			name: "no event time on a rotated file is an error",
			pos:  older,
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("SHOW BINARY LOGS").WillReturnRows(binlogIndex("mysql-bin.000002", "mysql-bin.000003"))
				m.ExpectQuery("binlog_expire_logs_seconds").WillReturnRows(variable("binlog_expire_logs_seconds", "604800"))
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			c.mock(m)
			got, err := mysqlPositionHorizonDB(context.Background(), db, c.pos, c.evt, now)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("horizon = %+v, want %+v", got, c.want)
			}
			if err := m.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSQLServerPositionHorizon(t *testing.T) {
	const lsn = "0000002b:00000768:0005"
	ageQuery := `DATEDIFF\(SECOND, MAX\(tran_end_time\), GETDATE\(\)\)`
	cases := []struct {
		name    string
		mock    func(sqlmock.Sqlmock)
		want    PositionHorizon
		wantErr bool
	}{
		{
			name: "age taken on the server against the cleanup job's retention",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(ageQuery).WithArgs("0x0000002B000007680005").
					WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(20 * 3600)))
				m.ExpectQuery(`msdb.dbo.cdc_jobs`).WillReturnRows(sqlmock.NewRows([]string{"retention"}).AddRow(int64(1440)))
			},
			want: PositionHorizon{Retention: 24 * time.Hour, Remaining: 4 * time.Hour},
		},
		{
			name: "msdb unreadable: the documented 3-day default, marked assumed",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(ageQuery).WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(int64(3600)))
				m.ExpectQuery(`msdb.dbo.cdc_jobs`).WillReturnError(sql.ErrConnDone)
			},
			want: PositionHorizon{Retention: SQLServerDefaultCleanupRetention, RetentionAssumed: true,
				Remaining: SQLServerDefaultCleanupRetention - time.Hour},
		},
		{
			name: "no mapping at or below the position: the cleanup already passed it",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(ageQuery).WillReturnRows(sqlmock.NewRows([]string{"age"}).AddRow(nil))
			},
			want: PositionHorizon{Purged: true},
		},
		{
			name: "unreadable LSN map is an error",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(ageQuery).WillReturnError(sql.ErrConnDone)
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, m, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			c.mock(m)
			got, err := sqlserverPositionHorizonDB(context.Background(), db, lsn)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("horizon = %+v, want %+v", got, c.want)
			}
			if err := m.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestDebeziumLSNHex(t *testing.T) {
	if got, err := debeziumLSNHex("0000002b:00000768:0005"); err != nil || got != "0x0000002B000007680005" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "NULL", "0000002b:00000768", "0000002b:0000076g:0005", "2b:768:5"} {
		if _, err := debeziumLSNHex(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
