FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/z228 ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/z228 /usr/local/bin/z228
VOLUME /data
ENV DATA_DIR=/data
EXPOSE 3000 3443 50000/udp 50000/tcp
ENTRYPOINT ["z228"]
