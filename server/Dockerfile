# Build stage
FROM golang:1.23-alpine AS builder

# Set proxy for build
ARG HTTPS_PROXY
ARG HTTP_PROXY

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

# Build application
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./cmd/api

# Final stage
FROM alpine:latest

# Set proxy for final stage
ARG HTTPS_PROXY
ARG HTTP_PROXY

RUN apk --no-cache add ca-certificates

# Create non-root user for security
RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S appuser -G appgroup

WORKDIR /app

# Copy binary from builder with execute permission
COPY --from=builder --chmod=755 /app/server ./server

# Change ownership to non-root user
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port
EXPOSE 8080

# Run application
CMD ["./server"]
