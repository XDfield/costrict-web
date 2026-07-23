# cs-user service Dockerfile — multi-stage build.
# Syntax: docker build -f cs-user/Dockerfile -t cs-user:dev .

FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache deps first.
COPY cs-user/go.mod cs-user/go.sum* ./
RUN go mod download

# Build all three binaries (API server + migrate tool + ETL tool) in one
# layer to share the warm compile cache.
COPY cs-user/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cs-user ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cs-user-migrate ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cs-user-etl ./cmd/etl

FROM alpine:3.20

# curl + jq ship in the image so ops scripts under cs-user/scripts/ can
# run from inside the container (kubectl exec / docker run).
RUN apk add --no-cache ca-certificates tzdata curl jq && \
    adduser -D -u 1000 csuser

USER csuser
COPY --from=builder /out/cs-user /app/cs-user
COPY --from=builder /out/cs-user-migrate /app/cs-user-migrate
COPY --from=builder /out/cs-user-etl /app/cs-user-etl

EXPOSE 8081
ENV CS_USER_HTTP_PORT=8081 \
    CS_USER_HTTP_MODE=release

ENTRYPOINT ["/app/cs-user"]
