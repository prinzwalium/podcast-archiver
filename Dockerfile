# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/podcast-archiver ./cmd/podcast-archiver

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S app -G app
COPY --from=build /out/podcast-archiver /usr/local/bin/podcast-archiver
USER app
VOLUME ["/data"]
ENV PA_DATA_DIR=/data PA_LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["podcast-archiver"]
