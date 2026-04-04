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
# kubectl for sandbox deployment steps
RUN curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin/
RUN ln -s /usr/local/bin/kubectl /usr/local/bin/oc
COPY --from=builder /opinai-go /app/opinai
RUN ln -s /app/opinai /app/opinai-go
EXPOSE 8080 8443
ENTRYPOINT ["/app/opinai"]
