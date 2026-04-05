FROM golang:1.25-alpine AS builder

RUN apk add --no-cache vips-dev gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o placeholder-cats .

FROM alpine:3.21

RUN apk add --no-cache vips ca-certificates

COPY --from=builder /app/placeholder-cats /usr/local/bin/placeholder-cats

EXPOSE 8080

ENTRYPOINT ["placeholder-cats"]
