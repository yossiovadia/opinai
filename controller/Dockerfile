# Stage 1: Build Go binary
FROM golang:1.23-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /build
COPY controller-go/go.mod controller-go/go.sum ./
RUN go mod download
COPY controller-go/ ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /opinai-go ./cmd/opinai

# Stage 2: Python + Go runtime
FROM python:3.12-slim

RUN apt-get update && apt-get install -y \
    curl jq git openssl build-essential \
    && rm -rf /var/lib/apt/lists/*

# Install gh CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      | tee /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y gh && rm -rf /var/lib/apt/lists/*

COPY controller/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY controller/opinai_controller.py controller/opinai_runner.py controller/dashboard.py controller/database.py controller/sandbox.py /app/
COPY controller/static/ /app/static/
COPY --from=go-builder /opinai-go /app/opinai-go

WORKDIR /app

EXPOSE 8080 8081 8443 8444

CMD ["sh", "-c", "/app/opinai-go --http=:8081 --https=:8444 & python opinai_controller.py"]
