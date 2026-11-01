# The Go stage runs on the build machine's own architecture and
# cross-compiles: under QEMU an arm64 compile of this module took over 20
# minutes. Only the final stage is per-platform.
FROM --platform=$BUILDPLATFORM golang:1.26.10-trixie@sha256:a050dcceee6eb9bf110151662dfac7662f3bec40927c7dee48731e3d92083d42 AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The commit this image was built from, so every log line names its release.
# The build workflow passes it; a local build without it says "unknown".
ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.revision=${GIT_SHA}" -o /out/orbit ./cmd/orbit

# Node is the runtime the agent CLIs need (Codex is a Node launcher; Claude
# Code's npm package installs a native binary through a postinstall script).
FROM node:24.22.2-trixie-slim@sha256:54f84b4cf8432b21aaffd5d3ce8d41e12cea0d10d33c09d2f3c9879d49d6b446

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tini \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Exactly the lockfile. Renovate bumps deploy/cli/package.json, so a CLI
# release reaches the image through a reviewed change like any other pin.
WORKDIR /opt/cli
COPY deploy/cli/package.json deploy/cli/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund && npm cache clean --force

COPY --from=build /out/orbit /usr/local/bin/orbit

# uid 10001 owns nothing else in the image. /data holds scratch run
# directories (every byte a run reads is stored in Postgres) and the Claude
# Code sessions of agents with `session`, so it is a volume: on a tmpfs or the
# container layer those agents start fresh after a restart.
RUN useradd --uid 10001 --create-home --home-dir /home/orbit --shell /usr/sbin/nologin orbit \
    && mkdir -p /data && chown orbit:orbit /data
USER orbit
ENV PATH="/opt/cli/node_modules/.bin:${PATH}" \
    ORBIT_DATA_DIR=/data \
    ORBIT_CONFIG_DIR=/config \
    ORBIT_LISTEN=:8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["orbit", "healthcheck"]
# tini reaps the CLIs' child processes, which orbit starts in their own
# process groups and kills as a group on timeout.
ENTRYPOINT ["tini", "--", "orbit"]
CMD ["serve"]
