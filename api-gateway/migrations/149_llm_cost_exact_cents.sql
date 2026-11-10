-- 149_llm_cost_exact_cents.sql
-- An LLM call's cost is kept exactly; only a total is rounded.
--
-- 057 stored each call in whole cents, rounded up, so a 10-token gpt-4o-mini
-- call that cost 0.00045¢ was charged 1¢, and a month of small calls ran the
-- $50 quota out at a fraction of that spend. Prices are whole cents per
-- million tokens (llm-service src/utils/llm_cost.py), so six decimal places
-- hold every call's cost exactly. cost_quota_usd_cents stays whole cents, and
-- the quota pre-check reads the month's exact total rounded down to a cent,
-- which against a whole-cent quota is exact (internal/handlers/llm_quota.go).

ALTER TABLE llm_usage_events ALTER COLUMN cost_usd_cents TYPE NUMERIC(14,6);
ALTER TABLE users ALTER COLUMN monthly_cost_usd_cents TYPE NUMERIC(14,6);

-- One charge function: the whole-cent signature goes. An integer charge, from
-- an llm-service that predates this, resolves to the NUMERIC one.
DROP FUNCTION IF EXISTS check_and_charge_user(UUID, INTEGER);

CREATE OR REPLACE FUNCTION check_and_charge_user(p_user_id UUID, p_cents NUMERIC)
RETURNS BOOLEAN AS $$
DECLARE
    v_period TEXT := to_char(now(), 'YYYY-MM');
    v_quota  INTEGER;
    v_cur    NUMERIC;
BEGIN
    -- Roll the period if a new month started
    UPDATE users
       SET monthly_cost_usd_cents = 0,
           monthly_cost_period    = v_period
     WHERE id = p_user_id
       AND monthly_cost_period <> v_period;

    -- Read current state
    SELECT cost_quota_usd_cents, monthly_cost_usd_cents
      INTO v_quota, v_cur
      FROM users WHERE id = p_user_id;

    IF v_quota IS NULL THEN
        RETURN FALSE;  -- unknown user
    END IF;

    IF v_cur + p_cents > v_quota THEN
        RETURN FALSE;  -- over quota
    END IF;

    UPDATE users
       SET monthly_cost_usd_cents = monthly_cost_usd_cents + p_cents
     WHERE id = p_user_id;
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;
