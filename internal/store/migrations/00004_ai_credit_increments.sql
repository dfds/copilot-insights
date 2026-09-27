-- +goose Up

CREATE TABLE ai_credit_increments (
    enterprise      TEXT        NOT NULL,
    captured_at     TIMESTAMPTZ NOT NULL,
    username        TEXT        NOT NULL,
    model           TEXT        NOT NULL,
    org_name        TEXT        NOT NULL,
    quantity        NUMERIC     NOT NULL,
    gross_amount    NUMERIC     NOT NULL,
    discount_amount NUMERIC     NOT NULL,
    net_amount      NUMERIC     NOT NULL,
    PRIMARY KEY (enterprise, captured_at, username, model, org_name)
);

-- +goose Down
DROP TABLE ai_credit_increments;
