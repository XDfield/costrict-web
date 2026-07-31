# Build stage - use BUILDPLATFORM for native execution, cross-compile via GOARCH
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Set proxy for build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG TARGETARCH

WORKDIR /app

# Install ca-certificates and build dependencies
RUN apk add --no-cache ca-certificates git

# Set Go module proxy
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy all source code (including pre-generated docs)
COPY . .

# Build application - cross-compile for target architecture
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o server ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o migrate ./cmd/migrate
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o gitserver-config ./cmd/gitserver-config
# Operator CLI for retrofitting Gitea provisioning onto pre-existing cs-user
# users (cb83dcd). Same posture as migrate / gitserver-config: in-image so
# operators can `kubectl exec` it without rolling a bespoke job image.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o user-provision-backfill ./cmd/user-provision-backfill

# Final stage
FROM alpine:latest

# Set proxy for final stage
ARG HTTPS_PROXY
ARG HTTP_PROXY

RUN apk --no-cache add ca-certificates curl jq

# Create non-root user for security
RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S appuser -G appgroup

WORKDIR /app

# Copy binaries from builder with execute permission
COPY --from=builder --chmod=755 /app/server ./server
COPY --from=builder --chmod=755 /app/migrate ./migrate
COPY --from=builder --chmod=755 /app/gitserver-config ./gitserver-config
COPY --from=builder --chmod=755 /app/user-provision-backfill ./user-provision-backfill
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/costrict-entrypoint

# Change ownership to non-root user
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port
EXPOSE 8080

# Run application
ENTRYPOINT ["/usr/local/bin/costrict-entrypoint"]
CMD ["./server"]
