# Build context: repo root (needs infra/sqlite.sql).
FROM golang:1.27-alpine AS build
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go-sqlite/go.mod go-sqlite/go.sum ./
RUN go mod download
COPY go-sqlite/*.go ./
# mattn/go-sqlite3 needs cgo; link statically against musl so the binary still runs on scratch
RUN CGO_ENABLED=1 go build -trimpath -tags "sqlite_omit_load_extension osusergo netgo" \
    -ldflags='-s -w -extldflags "-static"' -o /orders-api .

FROM scratch
COPY --from=build /orders-api /orders-api
COPY infra/sqlite.sql /sqlite.sql
ENV SQLITE_PATH=/data/app.db SQLITE_INIT=/sqlite.sql
USER 65534
ENTRYPOINT ["/orders-api"]
