# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/tele-drop .

FROM alpine:3
# ca-certificates is required for HTTPS calls to Telegram and S3-compatible endpoints.
RUN apk add --no-cache ca-certificates
WORKDIR /app

COPY --from=build /out/tele-drop /app/tele-drop

# Secrets are injected at runtime via environment variables (see config.yaml ${ENV} placeholders).
ENV CONFIG_PATH=/app/config.yaml
ENTRYPOINT ["/app/tele-drop", "-config", "/app/config.yaml"]
