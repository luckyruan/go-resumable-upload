FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /upload-server ./cmd/server

FROM alpine:3.21
RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app \
    && mkdir -p /data && chown app:app /data
COPY --from=build /upload-server /upload-server
USER 10001:10001
ENV ADDRESS=:8092 DATA_DIR=/data
EXPOSE 8092
ENTRYPOINT ["/upload-server"]
