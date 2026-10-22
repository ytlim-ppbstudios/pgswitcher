# PgBouncer 바이너리는 베이스 이미지 것을 그대로 쓴다. 이 레포엔 pgswitcher(Go)만 있다.
ARG PGBOUNCER_IMAGE=docker.io/edoburu/pgbouncer:v1.26.1-p0

FROM docker.io/library/golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pgswitcher .

FROM ${PGBOUNCER_IMAGE}
COPY --from=build /pgswitcher /usr/local/bin/pgswitcher
USER postgres
EXPOSE 5432 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/pgswitcher"]
