FROM golang:1.26-alpine AS builder

WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/new-iot ./cmd

FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=builder /out/new-iot /app/new-iot

ENTRYPOINT ["/app/new-iot"]
