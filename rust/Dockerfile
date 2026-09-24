FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src
# cache dependencies in a separate layer
COPY Cargo.toml Cargo.lock ./
RUN mkdir src && echo 'fn main() {}' > src/main.rs && cargo build --release && rm -rf src
COPY src ./src
RUN touch src/main.rs && cargo build --release

FROM scratch
COPY --from=build /src/target/release/orders-api /orders-api
USER 65534
ENTRYPOINT ["/orders-api"]
