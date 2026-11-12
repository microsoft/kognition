-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 012_usage_event_immutability: enforce immutable cost facts and usage receipts.

CREATE OR REPLACE FUNCTION reject_cost_record_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'cost records are immutable';
END;
$$;

DROP TRIGGER IF EXISTS trg_cost_records_immutable ON cost_records;
CREATE TRIGGER trg_cost_records_immutable
    BEFORE UPDATE ON cost_records
    FOR EACH ROW EXECUTE FUNCTION reject_cost_record_update();

CREATE OR REPLACE FUNCTION reject_usage_receipt_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage event receipts are immutable';
END;
$$;

DROP TRIGGER IF EXISTS trg_usage_event_receipts_immutable ON usage_event_receipts;
CREATE TRIGGER trg_usage_event_receipts_immutable
    BEFORE UPDATE OR DELETE ON usage_event_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_usage_receipt_mutation();
