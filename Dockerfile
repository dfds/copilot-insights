FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/main.go ./cmd/main.go
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -o /app/copilot-insights ./cmd/main.go

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 appuser
USER appuser
COPY --from=build /app/copilot-insights /app/copilot-insights
EXPOSE 8080 9090
CMD ["/app/copilot-insights"]
