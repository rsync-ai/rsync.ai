package validators

import (
	"errors"
	"testing"

	"api-gateway/internal/security"
)

// Bug class: a statement the read gate passes as a read, which writes or acts on
// the server. Reads are role-independent (ValidateExplorerStatement sends every
// SELECT/WITH through ValidateExplorerSQL), so a read that writes is a write any
// viewer can run, and model, data-check and workflow SQL inherit the same gate.
//
// Two shapes of the class, each probed in the spellings that slipped through:
//   - SELECT … INTO <target>: Postgres and SQL Server create a table, MySQL assigns
//     variables or writes a file. No form of INTO belongs in a read.
//   - A function call with a side effect or server access (kill a backend, read a
//     server file, open a second connection, change a setting, consume a CDC slot,
//     run a query string). The read-only transaction the executors now open stops
//     writes, but not these, so the gate must name them.
//
// KI-SQL-READ-GATE-LETS-WRITES-THROUGH: the first five probes are the KI's repro.
func TestViewerReadsThatWriteAreRefused(t *testing.T) {
	probes := []string{
		// The KI repro.
		"SELECT * INTO newtbl FROM t",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT pg_terminate_backend(123)",
		"SELECT dblink_exec('dbname=x', 'DROP TABLE y')",
		"SELECT set_config('statement_timeout','0',false)",

		// SELECT … INTO, every form.
		"select * into newtbl from t",
		"SELECT * INTO UNLOGGED newtbl FROM t",
		"SELECT * INTO TEMP newtbl FROM t",
		"SELECT * INTO TEMPORARY TABLE newtbl FROM t",
		"SELECT * INTO TEMP TABLE x FROM t",
		"SELECT * INTO public.newtbl FROM t",
		`SELECT * INTO "NewTbl" FROM t`,
		"SELECT * INTO dbo.newtbl FROM t",
		"SELECT id INTO @v FROM t",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"WITH s AS (SELECT 1 AS a) SELECT * INTO newtbl FROM s",
		"SELECT *\nINTO\tnewtbl\nFROM t",

		// Postgres: server files and processes.
		"SELECT pg_read_binary_file('/etc/passwd')",
		"SELECT pg_ls_dir('.')",
		"SELECT pg_stat_file('postgresql.conf')",
		"SELECT pg_cancel_backend(123)",
		"SELECT pg_reload_conf()",
		"SELECT pg_rotate_logfile()",
		"SELECT pg_switch_wal()",
		"SELECT pg_promote()",
		"SELECT lo_import('/etc/passwd')",
		"SELECT lo_export(16384, '/tmp/x')",
		"SELECT lo_unlink(16384)",
		// Postgres: a second connection or a query string run as SQL.
		"SELECT dblink('dbname=x', 'SELECT 1')",
		"SELECT dblink_connect('c', 'dbname=x')",
		"SELECT * FROM dblink('dbname=x', 'DELETE FROM y RETURNING 1') AS r(a int)",
		"SELECT query_to_xml('DELETE FROM y RETURNING 1', true, false, '')",
		"SELECT ts_stat('SELECT 1')",
		"SELECT * FROM crosstab('SELECT 1')",
		// Postgres: settings, locks, notifications, sequences.
		"SELECT set_config('default_transaction_read_only','off',false)",
		"SELECT pg_advisory_lock(1)",
		"SELECT pg_notify('c', 'x')",
		"SELECT nextval('s')",
		"SELECT setval('s', 1)",
		// Postgres: replication slots. A consumed CDC slot is lost data.
		"SELECT * FROM pg_logical_slot_get_changes('rsync_slot', NULL, NULL)",
		"SELECT pg_drop_replication_slot('rsync_slot')",
		"SELECT pg_create_logical_replication_slot('s', 'pgoutput')",
		"SELECT pg_replication_slot_advance('rsync_slot', '0/0')",
		// MySQL and SQL Server.
		"SELECT LOAD_FILE('/etc/passwd')",
		"SELECT * FROM OPENROWSET('SQLNCLI', 'Server=x;', 'SELECT 1')",
		"SELECT * FROM OPENQUERY(linked, 'DELETE FROM y')",
		"SELECT * FROM OPENDATASOURCE('SQLNCLI', 'Data Source=x').db.dbo.t",

		// Spellings of a name.
		"SELECT PG_TERMINATE_BACKEND(123)",
		"SELECT pg_catalog.pg_terminate_backend(123)",
		`SELECT "pg_terminate_backend"(123)`,
		`SELECT pg_catalog."pg_terminate_backend"(123)`,
		"SELECT pg_terminate_backend (123)",
		"WITH k AS (SELECT pg_terminate_backend(123)) SELECT * FROM k",
		"SELECT pid FROM pg_stat_activity WHERE pg_terminate_backend(pid)",
		// A Unicode-escaped identifier spells any name without its letters.
		`SELECT U&"pg_terminate\005fbackend"(123)`,
		// A string the engine ends somewhere other than where the masker does.
		`SELECT E'\'', pg_terminate_backend(123), ''`,
		"SELECT E'a\\' -- ', pg_terminate_backend(123)",
	}
	roles := []security.WorkspaceRole{security.WSViewer, security.WSMember, security.WSAdmin, security.WSOwner}
	for _, q := range probes {
		for _, role := range roles {
			if r := ValidateExplorerStatement(q, role); r == nil || r.Valid {
				t.Errorf("%s may run %q as a read", role, q)
			}
		}
	}
}

// The refusals above must not cost ordinary reads: the names are matched as
// whole words, and INTO only outside literals and quoted identifiers.
func TestViewerReadsThatLookLikeWritesStayValid(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t",
		"SELECT 'please log into the app' AS hint",
		`SELECT "into" FROM t`,
		"SELECT intoxication_level, into_count FROM t",
		"SELECT set_name, config_value FROM settings",
		"SELECT lo_price, hi_price FROM quotes",
		"SELECT pid, state FROM pg_stat_activity",
		"SELECT slot_name, active FROM pg_replication_slots",
		"SELECT name, setting FROM pg_settings WHERE name = 'work_mem'",
		"SELECT nextval_count FROM stats",
		"WITH s AS (SELECT 1 AS a) SELECT a FROM s",
	} {
		if r := ValidateExplorerStatement(q, security.WSViewer); r == nil || !r.Valid {
			t.Errorf("viewer refused an ordinary read %q: %+v", q, r)
		}
	}
}

// What the read-only transaction and the server's timeout answer with reaches the user
// as a refusal or a timeout, not as an unexpected 500.
func TestReadOnlyRefusalsAndServerTimeoutsAreClassified(t *testing.T) {
	for msg, want := range map[string]string{
		// Postgres 25006 and MySQL 1792.
		"query failed: ERROR: cannot execute CREATE TABLE AS in a read-only transaction (SQLSTATE 25006)": "permission_denied",
		"query failed: Error 1792 (25006): Cannot execute statement in a READ ONLY transaction.":          "permission_denied",
		// The BigQuery connector's refusal and its cancelled read
		// (warehouse_adapters.py BigQueryWarehouseAdapter._read_query).
		"A BigQuery read runs read-only: only a single SELECT may run, and BigQuery parses this as DELETE": "permission_denied",
		"The BigQuery read did not finish within 25s; the job was cancelled":                               "timeout",
		// Postgres 57014, MySQL 3024, MariaDB 1969.
		"query failed: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)":                           "timeout",
		"query failed: Error 3024 (HY000): Query execution was interrupted, maximum statement execution time exceeded": "timeout",
		"query failed: Error 1969 (70100): Query execution was interrupted (max_statement_time exceeded)":              "timeout",
	} {
		if got, _ := ClassifyExecutionError(errors.New(msg)); got != want {
			t.Errorf("%q classified %q, want %q", msg, got, want)
		}
	}
}
