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
COPY --from=builder /opinai-go /app/opinai
RUN ln -s /app/opinai /app/opinai-go
EXPOSE 8080 8443
ENTRYPOINT ["/app/opinai"]
