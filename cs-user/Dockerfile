# cs-user service Dockerfile — multi-stage build.
# Syntax: docker build -f cs-user/Dockerfile -t cs-user:dev .

FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache deps first.
COPY cs-user/go.mod cs-user/go.sum* ./
RUN go mod download

# Build.
COPY cs-user/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cs-user ./cmd/api

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 csuser

USER csuser
COPY --from=builder /out/cs-user /app/cs-user

EXPOSE 8081
ENV CS_USER_HTTP_PORT=8081 \
    CS_USER_HTTP_MODE=release

ENTRYPOINT ["/app/cs-user"]
