FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /logchipper .

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /logchipper /app/logchipper

RUN addgroup -S -g 10001 logchipper && \
    adduser -S -u 10001 -G logchipper -H -s /sbin/nologin logchipper && \
    mkdir -p /data && chown logchipper:logchipper /data

VOLUME ["/data"]

USER logchipper

ENV PORT=7070
ENV DB_PATH=/data/logchipper.db
ENV RETENTION_DAYS=30
ENV SYSLOG_ADDR=:5514
ENV SYSLOG_ENABLE=true
ENV AUTH=true
ENV ACCESS_MODE=network
ENV ALLOWED_IPS=""

EXPOSE 7070
EXPOSE 5514/udp
EXPOSE 5514/tcp

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://localhost:7070/healthz || exit 1

ENTRYPOINT ["/app/logchipper"]
