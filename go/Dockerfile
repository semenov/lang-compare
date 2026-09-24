FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /orders-api .

FROM scratch
COPY --from=build /orders-api /orders-api
USER 65534
ENTRYPOINT ["/orders-api"]
