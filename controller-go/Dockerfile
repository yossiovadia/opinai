FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /opinai-go ./cmd/opinai

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl jq git bash openssl
COPY --from=builder /opinai-go /usr/local/bin/opinai-go
EXPOSE 8081 8444
ENTRYPOINT ["opinai-go"]
