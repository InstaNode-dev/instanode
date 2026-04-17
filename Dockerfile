FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/instant-lite .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates postgresql-client gettext
COPY --from=build /bin/instant-lite /usr/local/bin/instant-lite
COPY schema.sql /app/schema.sql
COPY config.prod.yaml.tpl /app/config.prod.yaml.tpl
ENV CONFIG_PATH=/app/config.yaml
EXPOSE 8080
CMD ["sh", "-c", "envsubst < /app/config.prod.yaml.tpl > /app/config.yaml && instant-lite"]
