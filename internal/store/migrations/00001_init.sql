-- +goose Up

CREATE TABLE scopes (
    kind            TEXT        NOT NULL CHECK (kind IN ('org', 'enterprise')),
    name            TEXT        NOT NULL,
    installation_id BIGINT,
    active          BOOLEAN     NOT NULL DEFAULT true,
    copilot_enabled BOOLEAN     NOT NULL DEFAULT true,
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, name)
);

CREATE TABLE collector_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    collector   TEXT        NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    status      TEXT        NOT NULL CHECK (status IN ('running', 'ok', 'partial', 'error')),
    items       INTEGER     NOT NULL DEFAULT 0,
    error       TEXT
);
CREATE INDEX collector_runs_collector_started ON collector_runs (collector, started_at DESC);
CREATE INDEX collector_runs_ok ON collector_runs (collector, finished_at DESC) WHERE status IN ('ok', 'partial');

CREATE TABLE github_users (
    user_id       BIGINT      PRIMARY KEY,
    login         TEXT        NOT NULL,
    name          TEXT,
    primary_email TEXT,
    orgs          TEXT[]      NOT NULL DEFAULT '{}',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX github_users_login ON github_users (lower(login));

CREATE TABLE github_user_emails (
    org              TEXT   NOT NULL,
    user_id          BIGINT NOT NULL,
    email_normalized TEXT   NOT NULL,
    source           TEXT   NOT NULL CHECK (source IN ('verified', 'profile')),
    email            TEXT   NOT NULL,
    PRIMARY KEY (org, user_id, email_normalized, source)
);
CREATE INDEX github_user_emails_email ON github_user_emails (email_normalized);
CREATE INDEX github_user_emails_user ON github_user_emails (user_id);

CREATE TABLE copilot_report_raw (
    scope_kind  TEXT        NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name  TEXT        NOT NULL,
    report_type TEXT        NOT NULL,
    day         DATE        NOT NULL,
    status      TEXT        NOT NULL CHECK (status IN ('complete', 'no_data')),
    row_count   INTEGER     NOT NULL DEFAULT 0,
    -- JSON array of the NDJSON rows; NULL when status = 'no_data'.
    payload     JSONB,
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_name, report_type, day)
);
CREATE INDEX copilot_report_raw_fetched ON copilot_report_raw (fetched_at);

CREATE TABLE copilot_daily (
    scope_kind  TEXT NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name  TEXT NOT NULL,
    day         DATE NOT NULL,

    daily_active_users                        INTEGER,
    weekly_active_users                       INTEGER,
    monthly_active_users                      INTEGER,
    monthly_active_chat_users                 INTEGER,
    monthly_active_agent_users                INTEGER,
    daily_active_cli_users                    INTEGER,
    weekly_active_cli_users                   INTEGER,
    monthly_active_cli_users                  INTEGER,
    daily_active_copilot_app_users            INTEGER,
    weekly_active_copilot_app_users           INTEGER,
    monthly_active_copilot_app_users          INTEGER,
    daily_active_copilot_cloud_agent_users    INTEGER,
    weekly_active_copilot_cloud_agent_users   INTEGER,
    monthly_active_copilot_cloud_agent_users  INTEGER,
    daily_active_copilot_code_review_users    INTEGER,
    weekly_active_copilot_code_review_users   INTEGER,
    monthly_active_copilot_code_review_users  INTEGER,
    daily_passive_copilot_code_review_users   INTEGER,
    weekly_passive_copilot_code_review_users  INTEGER,
    monthly_passive_copilot_code_review_users INTEGER,
    daily_active_vscode_agent_users           INTEGER,
    weekly_active_vscode_agent_users          INTEGER,
    monthly_active_vscode_agent_users         INTEGER,

    user_initiated_interaction_count BIGINT,
    code_generation_activity_count   BIGINT,
    code_acceptance_activity_count   BIGINT,
    loc_suggested_to_add_sum         BIGINT,
    loc_suggested_to_delete_sum      BIGINT,
    loc_added_sum                    BIGINT,
    loc_deleted_sum                  BIGINT,

    pr_total_created                          BIGINT,
    pr_total_reviewed                         BIGINT,
    pr_total_merged                           BIGINT,
    pr_total_created_by_copilot               BIGINT,
    pr_total_reviewed_by_copilot              BIGINT,
    pr_total_merged_created_by_copilot        BIGINT,
    pr_total_merged_reviewed_by_copilot       BIGINT,
    pr_total_suggestions                      BIGINT,
    pr_total_applied_suggestions              BIGINT,
    pr_total_copilot_suggestions              BIGINT,
    pr_total_copilot_applied_suggestions      BIGINT,
    pr_median_minutes_to_merge                DOUBLE PRECISION,
    pr_median_minutes_to_merge_copilot_authored DOUBLE PRECISION,
    pr_median_minutes_to_merge_copilot_reviewed DOUBLE PRECISION,

    raw        JSONB       NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_name, day)
);

CREATE TABLE copilot_daily_facts (
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name TEXT NOT NULL,
    day        DATE NOT NULL,
    breakdown  TEXT NOT NULL,
    key1       TEXT NOT NULL DEFAULT '',
    key2       TEXT NOT NULL DEFAULT '',
    metric     TEXT NOT NULL,
    value      DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (scope_kind, scope_name, day, breakdown, key1, key2, metric)
);

CREATE TABLE copilot_user_daily (
    scope_kind TEXT   NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name TEXT   NOT NULL,
    day        DATE   NOT NULL,
    user_id    BIGINT NOT NULL,
    login      TEXT   NOT NULL,

    used_agent                       BOOLEAN,
    used_chat                        BOOLEAN,
    used_cli                         BOOLEAN,
    used_copilot_app                 BOOLEAN,
    used_copilot_cloud_agent         BOOLEAN,
    used_copilot_coding_agent        BOOLEAN,
    used_copilot_code_review_active  BOOLEAN,
    used_copilot_code_review_passive BOOLEAN,
    used_vscode_agent                BOOLEAN,

    ai_credits_used       DOUBLE PRECISION,
    ai_adoption_phase     TEXT,
    ai_adoption_phase_number INTEGER,

    user_initiated_interaction_count BIGINT,
    code_generation_activity_count   BIGINT,
    code_acceptance_activity_count   BIGINT,
    loc_suggested_to_add_sum         BIGINT,
    loc_suggested_to_delete_sum      BIGINT,
    loc_added_sum                    BIGINT,
    loc_deleted_sum                  BIGINT,

    ides      TEXT[] NOT NULL DEFAULT '{}',
    languages TEXT[] NOT NULL DEFAULT '{}',
    models    TEXT[] NOT NULL DEFAULT '{}',

    raw        JSONB       NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_name, day, user_id)
);
CREATE INDEX copilot_user_daily_user_day ON copilot_user_daily (user_id, day DESC);
CREATE INDEX copilot_user_daily_login ON copilot_user_daily (lower(login));

CREATE TABLE copilot_user_daily_facts (
    scope_kind TEXT   NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name TEXT   NOT NULL,
    day        DATE   NOT NULL,
    user_id    BIGINT NOT NULL,
    breakdown  TEXT   NOT NULL,
    key1       TEXT   NOT NULL DEFAULT '',
    key2       TEXT   NOT NULL DEFAULT '',
    metric     TEXT   NOT NULL,
    value      DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (scope_kind, scope_name, day, user_id, breakdown, key1, key2, metric)
);
CREATE INDEX copilot_user_daily_facts_user_day ON copilot_user_daily_facts (user_id, day DESC);

CREATE TABLE copilot_repo_daily (
    scope_kind      TEXT   NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name      TEXT   NOT NULL,
    day             DATE   NOT NULL,
    repo_id         BIGINT NOT NULL,
    organization_id TEXT,
    repo_owner_name TEXT   NOT NULL,
    repo_name       TEXT   NOT NULL,
    repo_visibility TEXT,

    pr_total_created                          BIGINT,
    pr_total_reviewed                         BIGINT,
    pr_total_merged                           BIGINT,
    pr_total_created_by_copilot               BIGINT,
    pr_total_reviewed_by_copilot              BIGINT,
    pr_total_merged_created_by_copilot        BIGINT,
    pr_total_merged_reviewed_by_copilot       BIGINT,
    pr_total_suggestions                      BIGINT,
    pr_total_applied_suggestions              BIGINT,
    pr_total_copilot_suggestions              BIGINT,
    pr_total_copilot_applied_suggestions      BIGINT,
    pr_median_minutes_to_merge                DOUBLE PRECISION,
    pr_median_minutes_to_merge_copilot_authored DOUBLE PRECISION,
    pr_median_minutes_to_merge_copilot_reviewed DOUBLE PRECISION,

    raw        JSONB       NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_name, day, repo_id)
);
CREATE INDEX copilot_repo_daily_repo ON copilot_repo_daily (repo_owner_name, repo_name, day DESC);

CREATE TABLE copilot_user_teams (
    scope_kind TEXT   NOT NULL CHECK (scope_kind = 'org'),
    scope_name TEXT   NOT NULL,
    day        DATE   NOT NULL,
    team_id    BIGINT NOT NULL,
    user_id    BIGINT NOT NULL,
    team_slug  TEXT   NOT NULL,
    login      TEXT   NOT NULL,
    PRIMARY KEY (scope_kind, scope_name, day, team_id, user_id)
);
CREATE INDEX copilot_user_teams_slug ON copilot_user_teams (scope_name, team_slug, day DESC);

CREATE TABLE copilot_seats_current (
    scope_kind                TEXT   NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name                TEXT   NOT NULL,
    user_id                   BIGINT NOT NULL,
    login                     TEXT   NOT NULL,
    org_name                  TEXT,
    assigning_team_slug       TEXT,
    plan_type                 TEXT,
    created_at                TIMESTAMPTZ,
    updated_at                TIMESTAMPTZ,
    last_activity_at          TIMESTAMPTZ,
    last_activity_editor      TEXT,
    last_authenticated_at     TIMESTAMPTZ,
    pending_cancellation_date DATE,
    PRIMARY KEY (scope_kind, scope_name, user_id)
);
CREATE INDEX copilot_seats_current_user ON copilot_seats_current (user_id);

CREATE TABLE copilot_seats_snapshot (
    scope_kind                TEXT   NOT NULL CHECK (scope_kind IN ('org', 'enterprise')),
    scope_name                TEXT   NOT NULL,
    snapshot_date             DATE   NOT NULL,
    user_id                   BIGINT NOT NULL,
    login                     TEXT   NOT NULL,
    org_name                  TEXT,
    assigning_team_slug       TEXT,
    plan_type                 TEXT,
    created_at                TIMESTAMPTZ,
    updated_at                TIMESTAMPTZ,
    last_activity_at          TIMESTAMPTZ,
    last_activity_editor      TEXT,
    last_authenticated_at     TIMESTAMPTZ,
    pending_cancellation_date DATE,
    PRIMARY KEY (scope_kind, scope_name, snapshot_date, user_id)
);
CREATE INDEX copilot_seats_snapshot_user ON copilot_seats_snapshot (user_id, snapshot_date DESC);

CREATE TABLE copilot_billing_summary (
    org_name                  TEXT NOT NULL,
    snapshot_date             DATE NOT NULL,
    seat_total                INTEGER,
    seat_added_this_cycle     INTEGER,
    seat_pending_invitation   INTEGER,
    seat_pending_cancellation INTEGER,
    seat_active_this_cycle    INTEGER,
    seat_inactive_this_cycle  INTEGER,
    plan_type                 TEXT,
    seat_management_setting   TEXT,
    policies                  JSONB NOT NULL DEFAULT '{}',
    fetched_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_name, snapshot_date)
);

CREATE TABLE billing_usage (
    enterprise      TEXT    NOT NULL,
    month           DATE    NOT NULL,
    product         TEXT    NOT NULL,
    sku             TEXT    NOT NULL,
    org_name        TEXT    NOT NULL DEFAULT '',
    repo_name       TEXT    NOT NULL DEFAULT '',
    quantity        NUMERIC NOT NULL,
    unit_type       TEXT,
    price_per_unit  NUMERIC,
    gross_amount    NUMERIC,
    discount_amount NUMERIC,
    net_amount      NUMERIC,
    fetched_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, month, product, sku, org_name, repo_name)
);

CREATE TABLE ai_credit_usage (
    enterprise                TEXT    NOT NULL,
    usage_date                DATE    NOT NULL,
    username                  TEXT    NOT NULL,
    product                   TEXT    NOT NULL,
    sku                       TEXT    NOT NULL,
    model                     TEXT    NOT NULL DEFAULT '',
    org_name                  TEXT    NOT NULL DEFAULT '',
    repo_name                 TEXT    NOT NULL DEFAULT '',
    cost_center               TEXT    NOT NULL DEFAULT '',
    quantity                  NUMERIC NOT NULL,
    unit_type                 TEXT,
    applied_cost_per_quantity NUMERIC,
    gross_amount              NUMERIC,
    discount_amount           NUMERIC,
    net_amount                NUMERIC,
    total_monthly_quota       NUMERIC,
    aic_quantity              NUMERIC,
    aic_gross_amount          NUMERIC,
    input_tokens              NUMERIC,
    output_tokens             NUMERIC,
    cache_read_tokens         NUMERIC,
    cache_write_tokens        NUMERIC,
    fetched_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, usage_date, username, product, sku, model, org_name, repo_name, cost_center)
);
CREATE INDEX ai_credit_usage_user_date ON ai_credit_usage (lower(username), usage_date DESC);

CREATE TABLE premium_request_usage_legacy (
    enterprise        TEXT     NOT NULL,
    year              SMALLINT NOT NULL,
    month             SMALLINT NOT NULL CHECK (month BETWEEN 1 AND 12),
    org_name          TEXT     NOT NULL DEFAULT '',
    login             TEXT     NOT NULL DEFAULT '',
    product           TEXT     NOT NULL,
    sku               TEXT     NOT NULL,
    model             TEXT     NOT NULL DEFAULT '',
    unit_type         TEXT,
    price_per_unit    NUMERIC,
    gross_quantity    NUMERIC,
    gross_amount      NUMERIC,
    discount_quantity NUMERIC,
    discount_amount   NUMERIC,
    net_quantity      NUMERIC,
    net_amount        NUMERIC,
    fetched_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, year, month, org_name, login, product, sku, model)
);

-- +goose Down
DROP TABLE premium_request_usage_legacy;
DROP TABLE ai_credit_usage;
DROP TABLE billing_usage;
DROP TABLE copilot_billing_summary;
DROP TABLE copilot_seats_snapshot;
DROP TABLE copilot_seats_current;
DROP TABLE copilot_user_teams;
DROP TABLE copilot_repo_daily;
DROP TABLE copilot_user_daily_facts;
DROP TABLE copilot_user_daily;
DROP TABLE copilot_daily_facts;
DROP TABLE copilot_daily;
DROP TABLE copilot_report_raw;
DROP TABLE github_user_emails;
DROP TABLE github_users;
DROP TABLE collector_runs;
DROP TABLE scopes;
