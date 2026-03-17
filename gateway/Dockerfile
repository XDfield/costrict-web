# Build stage
FROM golang:1.23-alpine AS builder

ARG HTTPS_PROXY
ARG HTTP_PROXY

WORKDIR /app

RUN apk add --no-cache ca-certificates

ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o gateway ./cmd

# Final stage
FROM alpine:latest

ARG HTTPS_PROXY
ARG HTTP_PROXY

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/gateway .

EXPOSE 8080

CMD ["./gateway"]
