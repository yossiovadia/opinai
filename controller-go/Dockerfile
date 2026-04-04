FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /opinai-go ./cmd/opinai

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl jq git bash openssl
RUN apk add --no-cache python3 py3-pip
# kubectl + helm for sandbox deployment steps
RUN curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin/
# Install real oc binary (has start-build, new-app, etc. that kubectl lacks)
# oc is glibc-linked, so install gcompat for musl compatibility
RUN apk add --no-cache gcompat
RUN curl -sL https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/openshift-client-linux.tar.gz | tar xzf - -C /usr/local/bin oc
RUN curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
COPY --from=builder /opinai-go /app/opinai
RUN ln -s /app/opinai /app/opinai-go
EXPOSE 8080 8443
ENTRYPOINT ["/app/opinai"]
