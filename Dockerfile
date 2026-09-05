FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY assets.go config.yaml ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /cgw ./cmd/proxy

FROM alpine:3.22
RUN addgroup -S proxy && adduser -S -G proxy proxy
WORKDIR /app
COPY --from=build /cgw /app/cgw
COPY config.yaml /app/config.yaml
USER proxy
EXPOSE 3002
ENV PROXY_HOST=0.0.0.0
ENTRYPOINT ["/app/cgw"]
