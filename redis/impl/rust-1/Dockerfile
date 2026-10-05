FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev build-base
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release

FROM scratch
COPY --from=build /src/target/release/mini-redis /mini-redis
ENTRYPOINT ["/mini-redis"]
