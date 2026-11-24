# syntax=docker/dockerfile:1
# Targets:
#   test        unit tests (the database ones are skipped without TEST_PG_URL)
#   migration   the migration tool and the committed migrations; the API never migrates itself
#   production  the API: static binaries on scratch, non-root, health-checked (default target)

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS modules
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

FROM modules AS test
COPY . .
ENV CGO_ENABLED=0
CMD ["go", "test", "-p", "1", "./..."]

FROM modules AS build
ARG TARGETOS
ARG TARGETARCH
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/app ./cmd/migrate ./cmd/healthcheck

FROM scratch AS migration
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/migrate /migrate
COPY --from=build /src/migrations /migrations
USER 1000:1000
ENTRYPOINT ["/migrate", "-path", "/migrations"]
CMD ["up"]

FROM scratch AS production
# The release's commit, shown by the app as its version.
ARG APP_VERSION=unknown
ENV APP_VERSION=$APP_VERSION
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /app
COPY --from=build /out/healthcheck /healthcheck
USER 1000:1000
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 CMD ["/healthcheck"]
ENTRYPOINT ["/app"]
