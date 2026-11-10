-- Migration 136: a model whose trigger could not be delivered says so.
--
-- A completion's fan-out is handed to Temporal, and since workflows-design.md § 4.2
-- there is no in-process path behind it: the gateway retries the start for about 30
-- seconds, and when it still cannot get through, every model below the completion gets
-- a 'skipped' row with skip_reason 'trigger_lost' instead of nothing. Without the row a
-- model whose trigger was lost had a history indistinguishable from "nothing happened".
--
-- The only change is the list the 104 CHECK accepts. Added NOT VALID and validated after
-- COMMIT, as in 104: added plainly, it scans saved_query_runs under the ACCESS EXCLUSIVE
-- lock the ALTER took, so every run write waits for the scan; VALIDATE CONSTRAINT scans
-- under SHARE UPDATE EXCLUSIVE, which blocks neither reads nor writes. The ALTER still
-- takes ACCESS EXCLUSIVE for an instant, so lock_timeout bounds the wait behind a running
-- model write: a blocked migration fails fast and the next api-gateway start re-runs it
-- (same timeout as 132, 133 and 135). Every statement is re-runnable.

BEGIN;

SET LOCAL lock_timeout = '3s';

ALTER TABLE saved_query_runs DROP CONSTRAINT IF EXISTS saved_query_runs_skip_reason_check;
ALTER TABLE saved_query_runs ADD CONSTRAINT saved_query_runs_skip_reason_check
    CHECK (skip_reason IS NULL OR skip_reason IN
        ('upstream_failed', 'upstream_skipped', 'chain_depth_exceeded', 'waiting_on_upstreams', 'trigger_lost')) NOT VALID;

COMMIT;

ALTER TABLE saved_query_runs VALIDATE CONSTRAINT saved_query_runs_skip_reason_check;
