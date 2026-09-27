-- +goose Up

CREATE TABLE billing_budgets (
    enterprise            TEXT        NOT NULL,
    budget_id             TEXT        NOT NULL,
    budget_type           TEXT        NOT NULL,
    scope                 TEXT        NOT NULL,
    entity_name           TEXT,
    login                 TEXT,
    product_sku           TEXT,
    amount                NUMERIC     NOT NULL,
    consumed              NUMERIC,
    prevent_further_usage BOOLEAN     NOT NULL,
    fetched_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, budget_id)
);

CREATE TABLE billing_budget_user_states (
    enterprise         TEXT        NOT NULL,
    budget_id          TEXT        NOT NULL,
    login              TEXT        NOT NULL,
    consumed           NUMERIC     NOT NULL,
    target             NUMERIC     NOT NULL,
    override_budget_id TEXT,
    fetched_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, budget_id, login)
);
CREATE INDEX billing_budget_user_states_login ON billing_budget_user_states (lower(login));

-- +goose Down
DROP TABLE billing_budget_user_states;
DROP TABLE billing_budgets;
