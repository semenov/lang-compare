FROM golang:1.27-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init catalog && CGO_ENABLED=0 go build -ldflags="-s -w" -o /catalog .
FROM scratch
COPY --from=build /catalog /catalog
ENTRYPOINT ["/catalog"]
