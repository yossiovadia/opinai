# Stage 1: Build Go binary
FROM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /build
COPY controller-go/go.mod controller-go/go.sum ./
RUN go mod download
COPY controller-go/ ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /opinai-go ./cmd/opinai

# Stage 2: Download K8s CLI tools
FROM alpine:3.20 AS tools
RUN apk add --no-cache curl tar gzip
WORKDIR /tools
RUN curl -sLO "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
 && chmod +x kubectl
RUN curl -sL "https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/openshift-client-linux.tar.gz" \
      | tar xz oc \
 && chmod +x oc
RUN curl -sL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv5.7.0/kustomize_v5.7.0_linux_amd64.tar.gz" \
      | tar xz \
 && chmod +x kustomize
RUN curl -sL "https://get.helm.sh/helm-v3.17.0-linux-amd64.tar.gz" \
      | tar xz linux-amd64/helm --strip-components=1 \
 && chmod +x helm

# Stage 3: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq git openssl ca-certificates make \
    python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Install gh CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      | tee /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# K8s CLI tools
COPY --from=tools /tools/kubectl /usr/local/bin/
COPY --from=tools /tools/oc /usr/local/bin/
COPY --from=tools /tools/kustomize /usr/local/bin/
COPY --from=tools /tools/helm /usr/local/bin/

# Go binary
COPY --from=go-builder /opinai-go /app/opinai-go

WORKDIR /app

EXPOSE 8080 8443

CMD ["/app/opinai-go"]
