# Build stage
FROM golang:1.21-alpine AS builder

# Set proxy for build
ARG HTTPS_PROXY
ARG HTTP_PROXY

WORKDIR /app

# Install ca-certificates
RUN apk add --no-cache ca-certificates

# Set Go module proxy
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

# Copy all source code
COPY . .

# Build application (will automatically download dependencies)
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
