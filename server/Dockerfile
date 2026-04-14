# Build stage - use BUILDPLATFORM for native execution, cross-compile via GOARCH
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Set proxy for build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG TARGETARCH

WORKDIR /app

# Install ca-certificates, git, and gcc for CGO (required by go-sqlite3)
RUN apk add --no-cache ca-certificates git gcc musl-dev

# Set Go module proxy
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy all source code (including pre-generated docs)
COPY . .

# Build application with CGO enabled (go-sqlite3 requires cgo)
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -o server ./cmd/api
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -o migrate ./cmd/migrate

# Final stage
FROM alpine:latest

# Set proxy for final stage
ARG HTTPS_PROXY
ARG HTTP_PROXY

RUN apk --no-cache add ca-certificates musl

# Create non-root user for security
RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S appuser -G appgroup

WORKDIR /app

# Copy binaries from builder with execute permission
COPY --from=builder --chmod=755 /app/server ./server
COPY --from=builder --chmod=755 /app/migrate ./migrate

# Change ownership to non-root user
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port
EXPOSE 8080

# Run application
CMD ["./server"]
