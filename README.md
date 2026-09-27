# Copilot Insights

Copilot Insights collects GitHub Copilot usage, seats, billing, budgets, and member data for a GitHub enterprise. It stores the results in Postgres and exposes them through an OIDC-protected REST API. Database migrations run at startup.

## Run locally

```sh
make db-up

export CPI_GITHUB_ENTERPRISE=dfds
export CPI_GITHUB_APPID=123456
export CPI_GITHUB_APPPRIVATEKEYPATH=/path/to/app-private-key.pem
export CPI_DB_HOST=localhost
export CPI_DB_PORT=5533
export CPI_DB_USER=postgres
export CPI_DB_PASSWORD=secret
export CPI_DB_SSLMODE=disable
export CPI_OIDC_ENABLED=false

make run
```

The example disables OIDC for local development. Keep it enabled for deployed environments and set its issuer and audience. The API listens on port 8080 by default; metrics use port 9090. `/healthz` and `/readyz` do not require a token.

## Environment variables

The service reads `CPI_*` variables at startup. Names have no extra underscores within field names: use `CPI_GITHUB_APPID`, for example, rather than `CPI_GITHUB_APP_ID`. An empty default below means the variable has no configured default. Duration values use Go duration syntax such as `30m`, `1h`, or `24h`. List values are comma separated, with surrounding whitespace removed.

### Service

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_LOGLEVEL` | `info` | Log level passed to the bootstrap logger. |
| `CPI_LOGDEBUG` | `false` | Enable debug mode for bootstrap logging and the HTTP router. |
| `CPI_APIPORT` | `8080` | Port for the REST API and health endpoints. Metrics remain on port 9090. |

### GitHub

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_GITHUB_ENTERPRISE` | Required | GitHub enterprise slug to collect. |
| `CPI_GITHUB_APPID` | Required | Numeric GitHub App ID. |
| `CPI_GITHUB_APPPRIVATEKEY` | Empty | GitHub App private key contents. Accepts a multiline PEM or literal `\n` line breaks. Takes precedence over `CPI_GITHUB_APPPRIVATEKEYPATH`. Set this or the path. |
| `CPI_GITHUB_APPPRIVATEKEYPATH` | Empty | Path to a readable GitHub App private key file. Required when `CPI_GITHUB_APPPRIVATEKEY` is empty. |
| `CPI_GITHUB_PAT` | Empty | PAT for enterprise Copilot seats. Without it, enterprise seats are skipped. Also used as a fallback when an organization refuses an App request. |
| `CPI_GITHUB_ORGEXCLUDE` | Empty | Comma-separated organization slugs to leave out of collection. |
| `CPI_GITHUB_CONCURRENCY` | `4` | Maximum concurrent work across scopes within a collector. Must be at least `1`. |
| `CPI_GITHUB_APIVERSION` | `2026-03-10` | Value sent in the `X-GitHub-Api-Version` request header. |
| `CPI_GITHUB_TIMEOUTMS` | `60000` | Timeout for each GitHub HTTP client request, in milliseconds. |

The App must be installed on an enterprise or organization for the service to collect that scope's App-backed data. The PAT does not replace the App configuration at startup.

### Postgres

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_DB_HOST` | Required | Postgres host. |
| `CPI_DB_PORT` | `5432` | Postgres port. Use `5533` with `make db-up` from the host. |
| `CPI_DB_NAME` | `copilot_insights` | Database name. |
| `CPI_DB_USER` | Required | Database user. |
| `CPI_DB_PASSWORD` | Empty | Database password. |
| `CPI_DB_SSLMODE` | `require` | Postgres SSL mode. Use `disable` for the bundled development database. |
| `CPI_DB_MAXOPENCONNS` | `10` | Maximum open and idle database connections when greater than zero. |

The database user needs permission to apply the embedded migrations at startup.

### Collection

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_COLLECT_BILLINGINTERVAL` | `1h` | Interval for enterprise Copilot billing usage. |
| `CPI_COLLECT_AICREDITINTERVAL` | `1h` | Interval for the enterprise AI credit billing report. Used when AI credit collection is enabled. |
| `CPI_COLLECT_BUDGETSINTERVAL` | `1h` | Interval for enterprise budgets and their user states. |
| `CPI_COLLECT_MEMBERSINTERVAL` | `6h` | Interval for App installations and organization members. |
| `CPI_COLLECT_SEATSINTERVAL` | `6h` | Interval for organization and enterprise seat snapshots. |
| `CPI_COLLECT_REPORTSINTERVAL` | `24h` | Interval for daily usage reports and the latest 28-day summary. |
| `CPI_COLLECT_BACKFILLDAYS` | `90` | Number of days in the report backfill window; also sets the starting window for closed AI credit months. Must be `0` to `365`. Daily reports still check at least one day when set to `0`. |
| `CPI_COLLECT_REPORTLAGDAYS` | `1` | Days behind today for the latest daily usage report. Must be nonnegative. |
| `CPI_COLLECT_BILLINGLAGDAYS` | `0` | Days behind today for the end of the current AI credit report window. Must be nonnegative. |
| `CPI_COLLECT_TEAMSENABLED` | `false` | Collect organization user-team usage reports. |
| `CPI_COLLECT_AICREDITENABLED` | `true` | Schedule AI credit report collection. |
| `CPI_COLLECT_PREMIUMBACKFILL` | `true` | Schedule the legacy premium request backfill for January through May 2026. Completed requests are recorded so later runs collect only missing subjects. |
| `CPI_COLLECT_RUNONSTART` | `true` | Run member discovery at startup, then start the other collectors within 30 seconds. When `false`, each job waits for its first interval. |

All six configurable intervals must be at least `1m`. Jobs run again after their interval plus up to 10% jitter; a job cannot overlap its own previous run. The legacy premium request and retention jobs have fixed 24-hour intervals.

### Retention

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_RETENTION_RAWDAYS` | `400` | Age in days after which the daily retention job prunes raw report payloads. It does not prune normalized report tables. |

### API authentication

| Variable | Default | Description |
| --- | --- | --- |
| `CPI_OIDC_ENABLED` | `true` | Verify bearer tokens on `/api/v1`. Set to `false` only for local development. |
| `CPI_OIDC_ISSUERURL` | Required when OIDC is enabled | OIDC issuer URL used for discovery and token validation. |
| `CPI_OIDC_AUDIENCE` | Required when OIDC is enabled | Expected token audience. |
| `CPI_OIDC_REQUIREDROLES` | `copilot.read` | Comma-separated accepted role values for API access. A token needs at least one. |
| `CPI_OIDC_ADMINROLES` | `copilot.admin` | Comma-separated accepted role values for admin routes. A token also needs one of the required API roles. |

Role values are case sensitive and must match the token's `roles` claim. Both role lists must contain at least one value when OIDC is enabled. With OIDC disabled, the API and admin routes have no token or role checks.

## API

The REST API uses `/api/v1` and returns data in a `{data, meta}` envelope. Read routes cover scopes, collector status, summary, daily and 28-day reports, billing, AI credits, budgets, seats, identities, and users. See the route registration in `internal/api/api.go` for the full path list.

`POST /api/v1/admin/collect/:collector` starts a collector run. Collector names are `members`, `seats`, `reports`, `billing`, `budgets`, `aicredit`, `premium-legacy`, and `retention`; `aicredit` and `premium-legacy` appear only when their corresponding settings enable them.

## Tests

`make test` runs without Postgres. Integration tests skip when `CPI_TEST_DB_HOST` is unset. `make test-integration` starts the bundled Postgres instance and runs all tests with the race detector. The test helper creates `copilot_insights_test` if needed and gives each test a separate schema.

| Variable | Default when `CPI_TEST_DB_HOST` is set | Description |
| --- | --- | --- |
| `CPI_TEST_DB_HOST` | Unset | Enables Postgres integration tests when set. |
| `CPI_TEST_DB_PORT` | `5533` | Test database port. |
| `CPI_TEST_DB_NAME` | `copilot_insights_test` | Test database name. |
| `CPI_TEST_DB_USER` | `postgres` | Test database user. |
| `CPI_TEST_DB_PASSWORD` | `secret` | Test database password. |
| `CPI_TEST_DB_SSLMODE` | `disable` | Test database SSL mode. |

## Deploy with Helm

The chart is in `chart/`. Put non-secret `CPI_*` values under `env` in your Helm values. Set `envFromSecret` to the name of a Kubernetes Secret containing sensitive variables such as `CPI_GITHUB_APPPRIVATEKEY`, `CPI_GITHUB_PAT`, `CPI_DB_USER`, and `CPI_DB_PASSWORD`. The chart uses one replica and exposes ports 8080 and 9090 by default. Set `ingress.host` when ingress is enabled.
