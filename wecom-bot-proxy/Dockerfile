FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /wecom-bot-proxy ./cmd/proxy

FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /wecom-bot-proxy /usr/local/bin/wecom-bot-proxy
COPY config.yaml.example /etc/wecom-bot-proxy/config.yaml

EXPOSE 9090

ENTRYPOINT ["wecom-bot-proxy"]
CMD ["-config", "/etc/wecom-bot-proxy/config.yaml"]
