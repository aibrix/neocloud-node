-- NeoCloud research trace V3 analysis queries.
-- Replace :run_id with the experiment_runs.id under analysis.

-- 1. Run metadata and conservative cost envelope.
SELECT
  r.id,
  r.protocol_version,
  r.host_id,
  r.vantage_id,
  r.status,
  r.started_at,
  r.finished_at,
  r.termination_reason,
  r.total_budget_usd,
  COALESCE(SUM(CASE WHEN b.kind = 'committed_upper_bound' THEN b.amount_usd ELSE 0 END), 0)
    AS cumulative_committed_upper_bound_usd,
  -COALESCE(SUM(CASE WHEN b.kind = 'released_unused_commitment' THEN b.amount_usd ELSE 0 END), 0)
    AS released_unused_commitment_usd,
  COALESCE(SUM(b.amount_usd), 0) AS current_budget_exposure_usd,
  SUM(b.kind = 'settled_runtime_estimate') AS settled_references
FROM experiment_runs r
LEFT JOIN budget_ledger_v2 b ON b.run_id = r.id
WHERE r.id = :run_id
GROUP BY r.id;

-- 2. Collector continuity. Large scheduler drift is a censoring control.
WITH ordered AS (
  SELECT
    observed_at,
    healthy,
    scheduler_drift_ms,
    network_status,
    (julianday(observed_at) -
      julianday(LAG(observed_at) OVER (ORDER BY observed_at))) * 86400 AS gap_s
  FROM host_observations
  WHERE run_id = :run_id
)
SELECT
  COUNT(*) AS samples,
  SUM(healthy = 0) AS unhealthy_samples,
  ROUND(MAX(gap_s), 1) AS maximum_wall_clock_gap_s,
  MAX(scheduler_drift_ms) AS maximum_scheduler_drift_ms,
  SUM(network_status IS NOT NULL AND network_status NOT LIKE '2%') AS network_failures
FROM ordered;

-- 3. Provider API reliability. A retry remains a separate failed attempt.
SELECT
  provider,
  operation,
  status,
  COALESCE(failure_class, 'none') AS failure_class,
  COUNT(*) AS calls,
  ROUND(AVG(latency_ms), 1) AS mean_latency_ms,
  MAX(latency_ms) AS maximum_latency_ms
FROM provider_api_observations_v2
WHERE run_id = :run_id
GROUP BY provider, operation, status, COALESCE(failure_class, 'none')
ORDER BY provider, operation, calls DESC;

-- 4. Exact catalog signal variation by query cell.
SELECT
  provider,
  sku,
  gpu_count,
  COALESCE(region, datacenter, 'provider-selected') AS location,
  resource_class,
  market,
  COUNT(*) AS samples,
  SUM(available = 1) AS advertised_available_samples,
  COUNT(DISTINCT stock_status) AS distinct_stock_states,
  MIN(quoted_price_usd_hr) AS minimum_on_demand_price,
  MAX(quoted_price_usd_hr) AS maximum_on_demand_price,
  MIN(spot_price_usd_hr) AS minimum_spot_price,
  MAX(spot_price_usd_hr) AS maximum_spot_price
FROM catalog_quotes_v2
WHERE run_id = :run_id
GROUP BY provider, sku, gpu_count, location, resource_class, market
ORDER BY provider, sku, gpu_count, location, resource_class;

-- 5. Scheduled capacity epochs. Keep non-attempted epochs in the denominator so
-- deadline, budget, cleanup, and collector-loss stopping remain observable.
SELECT
  e.cohort_name,
  e.sequence,
  e.provider,
  e.sku,
  COALESCE(e.region, e.datacenter, 'provider-selected') AS location,
  e.resource_class,
  COALESCE(
    t.gpu_count,
    CAST(json_extract(e.details_json, '$.gpu_count_per_instance') AS INTEGER),
    1
  ) AS gpu_count_per_instance,
  e.target_depth AS target_gpu_depth,
  e.target_depth / COALESCE(
    t.gpu_count,
    CAST(json_extract(e.details_json, '$.gpu_count_per_instance') AS INTEGER),
    1
  ) AS target_instance_count,
  e.scheduled_at,
  e.scheduled_offset_seconds,
  e.started_at,
  e.started_lag_seconds,
  e.selection_probability,
  e.selection_seed,
  e.status AS epoch_status,
  e.stop_reason,
  t.requested_depth AS requested_instance_count,
  t.accepted_depth AS accepted_instance_count,
  t.provider_ready_depth AS provider_ready_instance_count,
  t.application_ready_depth AS application_ready_instance_count,
  t.cleaned_depth AS cleaned_instance_count,
  t.requested_depth * t.gpu_count AS requested_gpu_depth,
  t.accepted_depth * t.gpu_count AS accepted_gpu_depth,
  t.provider_ready_depth * t.gpu_count AS provider_ready_gpu_depth,
  t.application_ready_depth * t.gpu_count AS application_ready_gpu_depth,
  t.cleaned_depth * t.gpu_count AS cleaned_gpu_depth,
  t.first_failed_stage AS first_failed_gpu_stage,
  t.failure_class,
  q.observed_at AS quote_observed_at,
  q.stock_status,
  q.available AS advertised_available,
  q.quoted_price_usd_hr
FROM capacity_epochs e
LEFT JOIN capacity_trials t ON t.id = e.trial_id
LEFT JOIN catalog_quotes_v2 q ON q.id = t.catalog_quote_id
WHERE e.run_id = :run_id
ORDER BY e.cohort_name, e.sequence;

-- 6. Advertised-versus-delivered capacity, including the linked pre-create quote.
SELECT
  t.id AS trial_id,
  t.provider,
  t.sku,
  t.region,
  t.resource_class,
  t.started_at,
  q.observed_at AS quote_observed_at,
  q.stock_status,
  q.available AS advertised_available,
  q.quoted_price_usd_hr,
  t.gpu_count AS gpu_count_per_instance,
  t.target_depth AS target_gpu_depth,
  t.target_depth / t.gpu_count AS target_instance_count,
  t.requested_depth AS requested_instance_count,
  t.accepted_depth AS accepted_instance_count,
  t.provider_ready_depth AS provider_ready_instance_count,
  t.application_ready_depth AS application_ready_instance_count,
  t.cleaned_depth AS cleaned_instance_count,
  t.requested_depth * t.gpu_count AS requested_gpu_depth,
  t.accepted_depth * t.gpu_count AS accepted_gpu_depth,
  t.provider_ready_depth * t.gpu_count AS provider_ready_gpu_depth,
  t.application_ready_depth * t.gpu_count AS application_ready_gpu_depth,
  t.cleaned_depth * t.gpu_count AS cleaned_gpu_depth,
  t.first_failed_stage AS first_failed_gpu_stage,
  t.failure_class,
  t.status,
  (SELECT COUNT(*) FROM capacity_replicas r
    WHERE r.trial_id = t.id AND r.retry_of IS NOT NULL) AS retry_attempts
FROM capacity_trials t
LEFT JOIN catalog_quotes_v2 q ON q.id = t.catalog_quote_id
WHERE t.run_id = :run_id
ORDER BY t.started_at;

-- 7. Per-stage latency and realized price. Retries that eventually created a
-- resource are included; failed attempts remain visible in query 8.
SELECT
  t.provider,
  t.sku,
  t.gpu_count AS gpu_count_per_instance,
  r.stage_target AS target_gpu_depth,
  COUNT(*) AS delivered_instances,
  COUNT(*) * t.gpu_count AS delivered_gpu_depth,
  ROUND(AVG((julianday(r.provider_ready_at) - julianday(r.requested_at)) * 86400), 1)
    AS mean_provider_ready_s,
  ROUND(MAX((julianday(r.provider_ready_at) - julianday(r.requested_at)) * 86400), 1)
    AS maximum_provider_ready_s,
  ROUND(AVG((julianday(r.application_ready_at) - julianday(r.requested_at)) * 86400), 1)
    AS mean_application_ready_s,
  ROUND(MAX((julianday(r.application_ready_at) - julianday(r.requested_at)) * 86400), 1)
    AS maximum_application_ready_s,
  MIN(r.realized_price_usd_hr) AS minimum_realized_price,
  MAX(r.realized_price_usd_hr) AS maximum_realized_price
FROM capacity_replicas r
JOIN capacity_trials t ON t.id = r.trial_id
WHERE t.run_id = :run_id AND r.provider_resource_id IS NOT NULL
GROUP BY t.provider, t.sku, t.gpu_count, r.stage_target
ORDER BY t.provider, t.sku, t.gpu_count, r.stage_target;

-- 8. Capacity failure and retry distribution.
SELECT
  t.provider,
  t.sku,
  t.gpu_count AS gpu_count_per_instance,
  r.stage_target AS target_gpu_depth,
  r.attempt,
  r.failure_class,
  COUNT(*) AS attempts
FROM capacity_replicas r
JOIN capacity_trials t ON t.id = r.trial_id
WHERE t.run_id = :run_id AND r.failure_class IS NOT NULL
GROUP BY t.provider, t.sku, t.gpu_count, r.stage_target, r.attempt, r.failure_class
ORDER BY t.provider, t.sku, t.gpu_count, r.stage_target, r.attempt, r.failure_class;


-- 9. Continuous cohort epochs. Budget exhaustion is an observed stopping
-- mechanism and must remain visible when estimating survival.
SELECT
  e.cohort_name,
  e.sequence,
  e.provider,
  e.sku,
  e.resource_class,
  e.selection_probability,
  e.selection_seed,
  e.selected_at,
  e.started_at,
  e.planned_end_at,
  e.finished_at,
  e.status,
  e.stop_reason,
  COUNT(l.id) AS leases
FROM canary_epochs e
LEFT JOIN canary_leases l ON l.pair_id = e.id
WHERE e.run_id = :run_id
GROUP BY e.id
ORDER BY e.cohort_name, e.sequence;

-- 10. Matched canary outcomes and conditional observation quality.
SELECT
  l.pair_id,
  e.cohort_name,
  json_extract(e.details_json, '$.design') AS cohort_design,
  e.sequence AS epoch_sequence,
  e.selection_probability,
  l.interruptible,
  l.machine_id,
  l.datacenter,
  l.censor_reason,
  l.censor_reason AS analysis_outcome,
  (SELECT COUNT(*) FROM canary_leases p WHERE p.pair_id=l.pair_id) AS group_size,
  ROUND((julianday(COALESCE(l.event_at, l.cleaned_at)) - julianday(l.created_at)) * 86400, 1)
    AS observed_lifetime_s,
  ROUND((julianday(l.provider_ready_at) - julianday(l.created_at)) * 86400, 1)
    AS provider_ready_s,
  ROUND((julianday(l.application_ready_at) - julianday(l.created_at)) * 86400, 1)
    AS application_ready_s,
  ROUND((julianday(COALESCE(l.event_at, l.cleaned_at)) -
    julianday(l.provider_ready_at)) * 86400, 1) AS post_provider_ready_exposure_s,
  ROUND((julianday(COALESCE(l.event_at, l.cleaned_at)) -
    julianday(l.application_ready_at)) * 86400, 1) AS post_application_ready_exposure_s,
  COUNT(o.id) AS observations,
  SUM(o.heartbeat_ok = 1) AS heartbeat_successes,
  ROUND(AVG(o.heartbeat_latency_ms), 1) AS mean_heartbeat_latency_ms,
  MIN(o.realized_price_usd_hr) AS minimum_realized_price,
  MAX(o.realized_price_usd_hr) AS maximum_realized_price
FROM canary_leases l
JOIN canary_epochs e ON e.id = l.pair_id
LEFT JOIN canary_observations o ON o.lease_id = l.id
WHERE l.run_id = :run_id
GROUP BY l.id
ORDER BY l.pair_id, l.interruptible DESC;

-- 11. Provider billing and resource-level reconciliation coverage. Missing
-- billing is not zero cost; aggregate invoice rows remain unattributed.
SELECT
  provider,
  source,
  COUNT(*) AS billing_records,
  SUM(provider_resource_id IS NOT NULL) AS resource_attributable_records,
  ROUND(SUM(amount_usd), 6) AS billed_amount_usd,
  SUM(billed_duration_ms) AS billed_duration_ms
FROM provider_billing_records
WHERE run_id = :run_id
GROUP BY provider, source
ORDER BY provider, source;

SELECT
  provider,
  status,
  COUNT(*) AS resources_or_aggregate_rows,
  ROUND(SUM(COALESCE(estimated_amount_usd, 0)), 6) AS estimated_amount_usd,
  ROUND(SUM(COALESCE(billed_amount_usd, 0)), 6) AS billed_amount_usd,
  ROUND(SUM(COALESCE(variance_usd, 0)), 6) AS variance_usd
FROM cost_reconciliations
WHERE run_id = :run_id
GROUP BY provider, status
ORDER BY provider, status;

-- 12. Cleanup correctness and unresolved billable ownership.
SELECT action, status, COALESCE(failure_class, 'none') AS failure_class, COUNT(*) AS events
FROM cleanup_events
WHERE run_id = :run_id
GROUP BY action, status, COALESCE(failure_class, 'none')
ORDER BY action, status;

SELECT 'capacity_replica' AS owner_type, r.id AS owner_id, r.provider_resource_id
FROM capacity_replicas r
JOIN capacity_trials t ON t.id = r.trial_id
WHERE t.run_id = :run_id
  AND r.provider_resource_id IS NOT NULL
  AND r.cleaned_at IS NULL
UNION ALL
SELECT 'canary_lease', l.id, l.provider_resource_id
FROM canary_leases l
WHERE l.run_id = :run_id
  AND l.provider_resource_id IS NOT NULL
  AND l.cleaned_at IS NULL;

-- 13. Durable Stability-window coverage. Keep missed windows with zero linked
-- epochs or leases in the denominator.
SELECT
  c.campaign_id,
  w.cohort_name,
  w.sequence AS window_sequence,
  w.scheduled_at,
  w.scheduled_end_at,
  w.claimed_at,
  w.started_at,
  w.finished_at,
  w.status AS window_status,
  w.stop_reason AS window_stop_reason,
  ROUND((julianday(w.started_at) - julianday(w.scheduled_at)) * 86400, 1)
    AS started_lag_seconds,
  COUNT(DISTINCT link.epoch_id) AS canary_epochs,
  COUNT(l.id) AS leases,
  SUM(l.interruptible = 1) AS interruptible_leases,
  SUM(l.interruptible = 0) AS matched_control_leases,
  SUM(l.application_ready_at IS NOT NULL) AS application_ready_leases
FROM stability_windows w
JOIN stability_campaigns c ON c.run_id = w.run_id
LEFT JOIN stability_window_epochs link ON link.window_id = w.id
LEFT JOIN canary_leases l ON l.pair_id = link.epoch_id
WHERE w.run_id = :run_id
GROUP BY w.id
ORDER BY w.sequence;
