FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tracker .

FROM scratch
COPY --from=build /tracker /tracker
ENV PORT=8080
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/tracker"]
