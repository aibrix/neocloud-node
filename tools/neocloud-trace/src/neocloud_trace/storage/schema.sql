PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS experiment_runs (
    id TEXT PRIMARY KEY,
    protocol_version TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    config_json TEXT NOT NULL,
    host_id TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    collector_revision TEXT,
    python_version TEXT,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    termination_reason TEXT,
    total_budget_usd REAL NOT NULL,
    max_active_gpus_per_provider INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS host_observations (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    observed_at TEXT NOT NULL,
    expected_monotonic REAL,
    actual_monotonic REAL,
    scheduler_drift_ms INTEGER,
    healthy INTEGER NOT NULL,
    network_status TEXT,
    network_latency_ms INTEGER,
    db_write_ok INTEGER NOT NULL,
    details_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_api_observations_v2 (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    observed_at TEXT NOT NULL,
    provider TEXT NOT NULL,
    operation TEXT NOT NULL,
    status TEXT NOT NULL,
    latency_ms INTEGER,
    failure_class TEXT,
    host_observation_id TEXT REFERENCES host_observations(id),
    response_hash TEXT,
    raw_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS catalog_quotes_v2 (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    observed_at TEXT NOT NULL,
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    gpu_count INTEGER NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    market TEXT NOT NULL,
    stock_status TEXT,
    available_gpu_counts_json TEXT NOT NULL,
    available INTEGER,
    quoted_price_usd_hr REAL,
    spot_price_usd_hr REAL,
    vcpus INTEGER,
    memory_gib REAL,
    storage_gib REAL,
    query_json TEXT NOT NULL,
    raw_hash TEXT NOT NULL,
    raw_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS capacity_trials (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    gpu_count INTEGER NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    market TEXT NOT NULL,
    image TEXT,
    target_depth INTEGER NOT NULL,
    requested_depth INTEGER NOT NULL DEFAULT 0,
    accepted_depth INTEGER NOT NULL DEFAULT 0,
    provider_ready_depth INTEGER NOT NULL DEFAULT 0,
    application_ready_depth INTEGER NOT NULL DEFAULT 0,
    cleaned_depth INTEGER NOT NULL DEFAULT 0,
    first_failed_stage INTEGER,
    failure_class TEXT,
    catalog_quote_id TEXT REFERENCES catalog_quotes_v2(id),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    details_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS capacity_replicas (
    id TEXT PRIMARY KEY,
    trial_id TEXT NOT NULL REFERENCES capacity_trials(id),
    stage_target INTEGER NOT NULL,
    ordinal INTEGER NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 0,
    requested_at TEXT NOT NULL,
    accepted_at TEXT,
    provider_ready_at TEXT,
    application_ready_at TEXT,
    failed_at TEXT,
    cleaned_at TEXT,
    provider_resource_id TEXT,
    provider_location TEXT,
    machine_id TEXT,
    failure_class TEXT,
    failure_message TEXT,
    realized_price_usd_hr REAL,
    status TEXT NOT NULL,
    retry_of TEXT REFERENCES capacity_replicas(id),
    details_json TEXT NOT NULL,
    UNIQUE(trial_id, stage_target, ordinal, attempt)
);

CREATE TABLE IF NOT EXISTS capacity_epochs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    cohort_name TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    target_depth INTEGER NOT NULL,
    scheduled_offset_seconds REAL NOT NULL,
    scheduled_at TEXT NOT NULL,
    selection_probability REAL NOT NULL,
    selection_seed INTEGER NOT NULL,
    started_at TEXT,
    started_lag_seconds REAL,
    finished_at TEXT,
    trial_id TEXT,
    status TEXT NOT NULL,
    stop_reason TEXT,
    details_json TEXT NOT NULL,
    UNIQUE(run_id, cohort_name, sequence)
);

CREATE TABLE IF NOT EXISTS capacity_campaigns (
    campaign_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL UNIQUE REFERENCES experiment_runs(id),
    cohort_name TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS completion_window_epochs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    cohort_name TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    market TEXT NOT NULL,
    target_depth INTEGER NOT NULL,
    arm_name TEXT NOT NULL,
    launch_fraction REAL NOT NULL,
    assignment_probability REAL NOT NULL,
    assignment_seed INTEGER NOT NULL,
    release_offset_seconds REAL NOT NULL,
    release_at TEXT NOT NULL,
    allocation_deadline_at TEXT NOT NULL,
    planned_launch_at TEXT NOT NULL,
    released_at TEXT,
    actual_decision_at TEXT,
    decision_lag_seconds REAL,
    release_quote_id TEXT REFERENCES catalog_quotes_v2(id),
    decision_quote_id TEXT REFERENCES catalog_quotes_v2(id),
    capacity_epoch_id TEXT,
    provider_sla_met INTEGER,
    application_sla_met INTEGER,
    finished_at TEXT,
    status TEXT NOT NULL,
    stop_reason TEXT,
    details_json TEXT NOT NULL,
    UNIQUE(run_id, cohort_name, sequence)
);

CREATE TABLE IF NOT EXISTS canary_leases (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    market TEXT NOT NULL,
    interruptible INTEGER NOT NULL,
    pair_id TEXT,
    provider_resource_id TEXT,
    machine_id TEXT,
    planned_end_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    provider_ready_at TEXT,
    application_ready_at TEXT,
    last_heartbeat_at TEXT,
    event_at TEXT,
    censor_reason TEXT,
    operator_cleanup_at TEXT,
    cleaned_at TEXT,
    status TEXT NOT NULL,
    details_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS canary_epochs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    cohort_name TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    provider TEXT NOT NULL,
    sku TEXT NOT NULL,
    gpu_type TEXT NOT NULL,
    region TEXT,
    datacenter TEXT,
    resource_class TEXT NOT NULL,
    planned_duration_seconds REAL NOT NULL,
    interval_seconds REAL NOT NULL,
    selection_probability REAL NOT NULL,
    selection_seed INTEGER,
    selected_at TEXT NOT NULL,
    started_at TEXT,
    planned_end_at TEXT,
    finished_at TEXT,
    status TEXT NOT NULL,
    stop_reason TEXT,
    details_json TEXT NOT NULL,
    UNIQUE(run_id, cohort_name, sequence)
);

CREATE TABLE IF NOT EXISTS canary_observations (
    id TEXT PRIMARY KEY,
    lease_id TEXT NOT NULL REFERENCES canary_leases(id),
    observed_at TEXT NOT NULL,
    provider_state TEXT,
    machine_id TEXT,
    runtime_uptime_seconds REAL,
    realized_price_usd_hr REAL,
    provider_location TEXT,
    heartbeat_ok INTEGER,
    heartbeat_latency_ms INTEGER,
    heartbeat_version TEXT,
    host_observation_id TEXT REFERENCES host_observations(id),
    raw_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS cleanup_events (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    owner_type TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    provider_resource_id TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    action TEXT NOT NULL,
    status TEXT NOT NULL,
    failure_class TEXT,
    details_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS budget_ledger_v2 (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    observed_at TEXT NOT NULL,
    provider TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    kind TEXT NOT NULL,
    reference TEXT,
    details_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_billing_records (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    provider TEXT NOT NULL,
    provider_resource_id TEXT,
    period_start TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    billed_duration_ms INTEGER,
    currency TEXT NOT NULL,
    source TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    gpu_type TEXT,
    disk_space_billed_gb REAL,
    imported_at TEXT NOT NULL,
    raw_json TEXT NOT NULL,
    UNIQUE(run_id, provider, source, source_record_id)
);

CREATE TABLE IF NOT EXISTS cost_reconciliations (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES experiment_runs(id),
    provider TEXT NOT NULL,
    provider_resource_id TEXT NOT NULL,
    owner_type TEXT,
    owner_id TEXT,
    estimated_amount_usd REAL,
    billed_amount_usd REAL,
    variance_usd REAL,
    billing_record_count INTEGER NOT NULL,
    status TEXT NOT NULL,
    method TEXT NOT NULL,
    reconciled_at TEXT NOT NULL,
    details_json TEXT NOT NULL,
    UNIQUE(run_id, provider, provider_resource_id)
);

CREATE INDEX IF NOT EXISTS idx_quotes_run_cell_time
    ON catalog_quotes_v2(run_id, provider, sku, resource_class, observed_at);
CREATE INDEX IF NOT EXISTS idx_replicas_provider_resource
    ON capacity_replicas(provider_resource_id, cleaned_at);
CREATE INDEX IF NOT EXISTS idx_capacity_epochs_run_cohort
    ON capacity_epochs(run_id, cohort_name, sequence);
CREATE INDEX IF NOT EXISTS idx_completion_window_epochs_run_cohort
    ON completion_window_epochs(run_id, cohort_name, sequence);
CREATE INDEX IF NOT EXISTS idx_canaries_provider_resource
    ON canary_leases(provider_resource_id, cleaned_at);
CREATE INDEX IF NOT EXISTS idx_canaries_pair
    ON canary_leases(pair_id);
CREATE INDEX IF NOT EXISTS idx_canary_epochs_run_cohort
    ON canary_epochs(run_id, cohort_name, sequence);
CREATE INDEX IF NOT EXISTS idx_budget_run_provider
    ON budget_ledger_v2(run_id, provider);
CREATE INDEX IF NOT EXISTS idx_billing_run_resource
    ON provider_billing_records(run_id, provider, provider_resource_id);
CREATE INDEX IF NOT EXISTS idx_reconciliation_run_status
    ON cost_reconciliations(run_id, status);
