FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY .. .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o telegram-ollama-bot ./cmd/ollamabot

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    update-ca-certificates

WORKDIR /app

RUN mkdir -p /var/lib/ollamabot && \
    chown -R root:root /var/lib/ollamabot

COPY --from=builder /app/telegram-ollama-bot /usr/local/bin/telegram-ollama-bot

ENV OLLAMA_BASE_URL="http://ollama:11434" \
    OLLAMA_MODEL="llama3.1"

ENTRYPOINT ["telegram-ollama-bot"]
