FROM golang:1.26-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0

FROM base AS dev
RUN apk add --no-cache git curl postgresql17-client && \
    go install github.com/air-verse/air@latest && \
    go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest && \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
CMD ["air", "-c", ".air.toml"]

FROM base AS build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/jlp ./cmd/jlp

FROM gcr.io/distroless/static-debian12 AS prod
WORKDIR /app
COPY --from=build /out/jlp /app/jlp
COPY web /app/web
EXPOSE 8080
ENTRYPOINT ["/app/jlp"]
CMD ["serve"]
