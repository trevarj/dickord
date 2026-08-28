FROM golang:1.25.6-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=0.1.0' -o /dickord .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    addgroup -S -g 65532 dickord && adduser -S -D -H -u 65532 -G dickord dickord
COPY --from=build /dickord /usr/local/bin/dickord
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/dickord"]
CMD ["-config", "/run/config/dickord.json"]
