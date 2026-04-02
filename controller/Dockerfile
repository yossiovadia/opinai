# Stage 1: Build Go binary
FROM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /build
COPY controller-go/go.mod controller-go/go.sum ./
RUN go mod download
COPY controller-go/ ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /opinai-go ./cmd/opinai

# Stage 2: Minimal runtime with Go binary + python3 for pip (runner needs it)
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq git openssl ca-certificates \
    python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Install gh CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      | tee /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /opinai-go /app/opinai-go

WORKDIR /app

EXPOSE 8080 8443

CMD ["/app/opinai-go"]
