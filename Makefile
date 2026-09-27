BINARY_NAME=copilot-insights

# Matches docker-compose.yml; store tests skip when CPI_TEST_DB_HOST is unset.
TEST_DB_ENV=CPI_TEST_DB_HOST=localhost CPI_TEST_DB_PORT=5533 CPI_TEST_DB_USER=postgres \
	CPI_TEST_DB_PASSWORD=secret CPI_TEST_DB_SSLMODE=disable

build:
	go build -o bin/$(BINARY_NAME) ./cmd/main.go

run:
	go run ./cmd/main.go

test:
	go test ./...

test-race:
	go test -race ./...

test-integration: db-up
	$(TEST_DB_ENV) go test -race ./...

test-coverage:
	go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out -o coverage.html

db-up:
	docker compose up -d --wait db

db-down:
	docker compose down

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

docker-build:
	docker build -t dfdsdk/$(BINARY_NAME):latest .

# Runs the endpoint probe into scratch/. Pass flags
# via PROBE_ARGS, e.g. PROBE_ARGS="-app-id 5049844 -app-key-path ./key.pem".
capture:
	go run ./cmd/probe -out scratch $(PROBE_ARGS)

verify: fmt tidy test-race

clean:
	rm -rf bin/ coverage.out coverage.html

.PHONY: build run test test-race test-integration test-coverage db-up db-down lint fmt tidy docker-build capture verify clean
