FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mini-redis .

FROM scratch
COPY --from=build /mini-redis /mini-redis
ENTRYPOINT ["/mini-redis"]
