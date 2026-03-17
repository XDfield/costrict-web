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

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/server .

# Expose port
EXPOSE 8080

# Run application
CMD ["./server"]
