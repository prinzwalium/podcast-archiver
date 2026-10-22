# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/podcast-archiver ./cmd/podcast-archiver

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata su-exec
COPY --from=build /out/podcast-archiver /usr/local/bin/podcast-archiver
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
VOLUME ["/data"]
ENV PA_DATA_DIR=/data PA_LISTEN_ADDR=:8080
# PUID/PGID: run as this host-mapped user (Unraid convention). Defaults to 1000:1000.
ENV PUID=1000 PGID=1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
