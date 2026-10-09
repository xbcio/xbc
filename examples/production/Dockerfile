# syntax=docker/dockerfile:1

# The production example's image. Build it from the repository root:
#
#   docker build -f examples/production/Dockerfile -t xbc-production .
#
# The whole repository is the build context on purpose: examples/go.mod
# resolves every XBC module through this repository's go.work, so a context
# limited to this directory cannot compile. .dockerignore keeps the
# repository's history and local design notes out of it.
#
# Nothing here is specific to this example. Any XBC application image has the
# same three parts: a cgo-capable build from the workspace, a non-root runtime
# with a writable data directory, and the configuration a deployment would
# otherwise have to restate. See docs/recipes.md for the run and migration
# procedure around this image.

FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY . .

# CGO_ENABLED=1 is required rather than incidental: the driver behind
# plugins.gorm.driver=sqlite is cgo-backed, and with cgo disabled this binary
# still builds and then cannot open a database. The module and build caches are
# BuildKit mounts instead of layers, so a source change rebuilds only what
# changed and the image carries no compiler cache.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/production ./examples/production

# A non-root process cannot create its data directory on a container
# filesystem, and SQLite cannot create its database file in one. Creating it
# here lets the runtime stage copy it with the ownership already applied.
RUN mkdir -p /out/data

FROM gcr.io/distroless/base-debian12:nonroot

# base-debian12 carries the glibc this cgo-built binary links against and the
# CA bundle an outbound TLS connection needs. There is no shell and no package
# manager in the image, which is the point: nothing can be installed into a
# running container.
COPY --from=build /out/production /usr/local/bin/production
# 65532 is distroless's nonroot uid and gid, the user the tag above selects.
COPY --from=build --chown=65532:65532 /out/data /var/lib/xbc

# --build-arg VERSION=1.4.0 stamps the image. The value reaches the public
# version route through plugins.version, which is why the route reads its
# metadata from configuration rather than from a compiled constant: the same
# binary reports the version of the image it was built into.
ARG VERSION=dev

# The image is startable with environment variables alone; no configuration
# file is baked in. What it sets is the part that describes the process rather
# than the deployment: the API prefix the routes are served below, one JSON
# object per line on stdout for the log collector, a filesystem database, the
# outbox that shares its transactions, the scrape endpoint, and migration at
# boot so a single container comes up ready to serve. A real deployment
# overrides the driver and DSN with its own database and runs `migrate` as a
# separate step -- see docs/recipes.md.
ENV XBC_PLUGINS_VERSION_VERSION=${VERSION} \
    XBC_WEB_BASE_PATH=/api/v1 \
    XBC_LOG_CONSOLE_FORMAT=json \
    XBC_PLUGINS_GORM_DEFAULT_DRIVER=sqlite \
    XBC_PLUGINS_GORM_DEFAULT_DSN="file:/var/lib/xbc/production.db?_busy_timeout=5000&_journal_mode=WAL" \
    XBC_PLUGINS_OUTBOX_MIGRATE=true \
    XBC_PLUGINS_METRICS_HTTP_ENABLED=true \
    XBC_AUTO_MIGRATE=true

# The one setting the image cannot supply. plugins.jwt.secret has no default
# anywhere, so a container started without it fails at startup by name instead
# of running with a guessable key:
#
#   docker run -p 8080:8080 -e XBC_PLUGINS_JWT_SECRET=<at least 32 bytes> xbc-production

WORKDIR /var/lib/xbc
USER 65532:65532
EXPOSE 8080

# The runtime handles SIGTERM as a graceful stop within xbc.shutdown_timeout,
# so the orchestrator's grace period has to exceed the stop budget or the
# process is killed mid-shutdown. See "The supervisor's stop grace period" in
# docs/recipes.md.
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/production"]
