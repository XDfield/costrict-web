FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG TARGETARCH

WORKDIR /app

RUN apk add --no-cache ca-certificates

ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o proxy ./cmd

FROM alpine:latest

ARG HTTPS_PROXY
ARG HTTP_PROXY

RUN apk --no-cache add ca-certificates

RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S appuser -G appgroup

WORKDIR /app

COPY --from=builder --chmod=755 /app/proxy ./proxy
COPY --from=builder /app/filter_rules.yaml ./filter_rules.yaml

RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 8090

CMD ["./proxy"]
