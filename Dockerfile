FROM golang:1.24.3-alpine AS build

WORKDIR /app

COPY . .

RUN go mod download
RUN go mod verify

RUN CGO_ENABLED=0 GOOS=linux go build -o server .

FROM alpine:edge

WORKDIR /app

COPY --from=build /app/server .

RUN apk --no-cache add ca-certificates tzdata

EXPOSE 8080

ENTRYPOINT ["/app/server"]
