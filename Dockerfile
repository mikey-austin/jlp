FROM golang:1.26-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0

FROM base AS dev
# zip: only needed for `make ext-build` (chrome-extension/ -> dist/jlp-extension.zip).
# unzip already ships in this alpine base; zip does not, so it's added
# explicitly here. build-base (gcc + musl-dev + friends): only needed for
# `make test-race` (`go test -race`, which requires cgo — the base
# image's CGO_ENABLED=0 above is overridden per-invocation by that
# target, not here, so a plain `go build`/`make test` still gets the
# faster, cgo-free static build).
RUN apk add --no-cache git curl postgresql17-client zip build-base && \
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
