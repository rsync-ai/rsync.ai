-- Migration 134: charge the bytes a sink commits after it restarts
--
-- kafka-mcp-sink reports counts.bytes_committed and counts.dlq_rows as running
-- totals it keeps in memory, so a restarted sink starts both at zero. The
-- projector kept the larger of the stored and reported value and charged only
-- the amount above the stored one, so after a restart nothing was charged, and
-- the stored total did not move, until the new process passed the old total.
-- On the CDC lane the stats row lives as long as the pipeline (execution_id is
-- forced to pipeline_id, migration 090), so the old total includes the initial
-- snapshot and a restarted CDC pipeline could go months uncharged.
--
-- The sink now stamps each TABLE_STATS event with counter_epoch_ms, the time its
-- process started those totals at zero. These columns hold, per row, the epoch
-- of the last event and the raw totals it reported. A newer epoch means the
-- totals restarted: the projector adds the reported total to the stored one.
-- The same epoch adds only the increase since the last event, so a replayed
-- event adds nothing. An older epoch is a replay from before the restart and
-- adds nothing.
--
-- NULL on every row from before this change and on rows only another producer
-- wrote (the batch lane before its sink restarts, the cdcstats agent). The first
-- event with an epoch then counts as a restart, which it is: the sink that sends
-- it was started after this change and began its totals at zero.
--
-- Note: our migration runner already wraps each migration in a transaction, so this
-- file must not include BEGIN/COMMIT statements.

ALTER TABLE pipeline_run_table_stats
ADD COLUMN IF NOT EXISTS counter_epoch_ms BIGINT NULL,
ADD COLUMN IF NOT EXISTS epoch_bytes_committed BIGINT NULL,
ADD COLUMN IF NOT EXISTS epoch_dlq_rows BIGINT NULL;

COMMENT ON COLUMN pipeline_run_table_stats.counter_epoch_ms IS
'When the sink process that sent the last counted TABLE_STATS event started its in-memory totals (bytes_committed, dlq_rows) at zero, in Unix milliseconds. NULL before migration 134 or from producers that send none.';
COMMENT ON COLUMN pipeline_run_table_stats.epoch_bytes_committed IS
'The raw counts.bytes_committed of the last event in counter_epoch_ms. bytes_committed is the sum across epochs.';
COMMENT ON COLUMN pipeline_run_table_stats.epoch_dlq_rows IS
'The raw counts.dlq_rows of the last event in counter_epoch_ms. dlq_rows is the sum across epochs.';
