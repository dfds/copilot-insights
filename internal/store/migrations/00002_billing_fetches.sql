-- +goose Up

CREATE TABLE ai_credit_months (
    enterprise TEXT        NOT NULL,
    month      DATE        NOT NULL,
    rows       INTEGER     NOT NULL,
    settled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, month)
);

CREATE TABLE premium_request_legacy_fetches (
    enterprise TEXT        NOT NULL,
    year       SMALLINT    NOT NULL,
    month      SMALLINT    NOT NULL,
    org_name   TEXT        NOT NULL DEFAULT '',
    login      TEXT        NOT NULL DEFAULT '',
    items      INTEGER     NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (enterprise, year, month, org_name, login)
);
CREATE INDEX premium_request_usage_legacy_login ON premium_request_usage_legacy (lower(login), year, month);

-- +goose Down
DROP INDEX premium_request_usage_legacy_login;
DROP TABLE premium_request_legacy_fetches;
DROP TABLE ai_credit_months;
