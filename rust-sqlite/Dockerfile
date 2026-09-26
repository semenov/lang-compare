# Build context: repo root (needs infra/sqlite.sql).
FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src
# cache dependencies in a separate layer (includes compiling the bundled SQLite C library)
COPY rust-sqlite/Cargo.toml rust-sqlite/Cargo.lock ./
COPY rust-sqlite/.cargo ./.cargo
RUN mkdir src && echo 'fn main() {}' > src/main.rs && cargo build --release && rm -rf src
COPY rust-sqlite/src ./src
RUN touch src/main.rs && cargo build --release

FROM scratch
COPY --from=build /src/target/release/orders-api /orders-api
COPY infra/sqlite.sql /sqlite.sql
ENV SQLITE_PATH=/data/app.db SQLITE_INIT=/sqlite.sql
USER 65534
ENTRYPOINT ["/orders-api"]
