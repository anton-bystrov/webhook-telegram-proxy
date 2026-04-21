FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY . ./

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/webhook-telegram-proxy ./cmd/server

FROM alpine:3.20

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=build /out/webhook-telegram-proxy /app/webhook-telegram-proxy
COPY templates /app/templates

USER app

EXPOSE 8080

ENTRYPOINT ["/app/webhook-telegram-proxy"]
