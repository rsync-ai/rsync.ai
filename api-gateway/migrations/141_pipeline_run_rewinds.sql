-- Migration 141: the tables a run rewound (generator-trust plan, phase 2).
--
-- A batch run whose sink lost batches puts its checkpoints back to where the run
-- started (orchestrator cdc.RewindCheckpointsOfExecution), so a Resume re-reads the
-- lost rows. The table stats called such a table "degraded", the same word as any
-- table whose written count fell short of its read count or that dead-lettered a row,
-- so the UI could not tell "rewound, Resume re-reads it" from "rows not written".
--
-- One row per table a run rewound, written in the rewind's own transaction, so a row
-- exists exactly when a rewind committed. execution_id is TEXT: it is the checkpoint
-- position's execution_id string. The table-stats API reads it at query time, joined
-- on pipeline, execution and table name, rather than copying a flag onto
-- pipeline_run_table_stats: the stats row may not be projected yet when the rewind
-- runs, and an UPDATE then would mark nothing.
--
-- Note: the migration runner wraps each migration in a transaction; no BEGIN/COMMIT.

CREATE TABLE IF NOT EXISTS pipeline_run_rewinds (
    pipeline_id UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    execution_id TEXT NOT NULL,
    source_table TEXT NOT NULL,
    rewound_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, execution_id, source_table)
);
