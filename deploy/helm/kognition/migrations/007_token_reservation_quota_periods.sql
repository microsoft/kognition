-- Copyright (c) Microsoft Corporation.
-- Licensed under the MIT License.

-- 007_token_reservation_quota_periods: bind reservations to quota reset periods.

ALTER TABLE token_reservations
    ADD COLUMN IF NOT EXISTS quota_periods JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Existing reservations are associated only with quotas that have never reset.
-- A non-empty reset marker makes the original accounting period ambiguous, so
-- settlement handles it conservatively as uncharged in the current period.
UPDATE token_reservations AS reservation
SET quota_periods = COALESCE((
    SELECT jsonb_object_agg(resource.name, jsonb_build_object(
        'resourceId', resource.id::text,
        'resetTime', COALESCE(resource.data #>> '{status,lastResetTime}', '')
    ))
    FROM resources AS resource
    WHERE resource.kind = 'CognitiveQuota'
      AND resource.namespace = reservation.namespace
      AND resource.deleted_at IS NULL
      AND resource.data #> '{spec,tokenBudget}' IS NOT NULL
      AND COALESCE(resource.data #>> '{status,lastResetTime}', '') = ''
), '{}'::jsonb)
WHERE quota_periods = '{}'::jsonb;