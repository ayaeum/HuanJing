FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /huanjing ./cmd/server

FROM alpine:3.23
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app && mkdir -p /app/data && chown app:app /app/data
WORKDIR /app
COPY --from=build /huanjing /app/huanjing
COPY web /app/web
ENV ADDR=0.0.0.0:8080 DB_PATH=/app/data/system.db WEB_DIR=/app/web WORKERS=4
USER app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s CMD wget -q -O /dev/null http://127.0.0.1:8080/api/health || exit 1
CMD ["/app/huanjing"]
