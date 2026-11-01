# The build image: what a build run's sandbox runs (orbit sandboxd starts
# it, internal/sandboxd fixes every option). It carries the agent CLI at
# exactly deploy/cli's lockfile, so a build runs the same Claude Code as
# every other run, and the toolchains a repository's checks need. It holds
# no credential and needs none at build time.
#
# Go is copied from the same golang image deploy/Dockerfile builds with, so
# `make check` inside a sandbox uses the toolchain CI does.
FROM golang:1.26.10-trixie@sha256:a050dcceee6eb9bf110151662dfac7662f3bec40927c7dee48731e3d92083d42 AS go

FROM node:24.22.2-trixie-slim@sha256:54f84b4cf8432b21aaffd5d3ce8d41e12cea0d10d33c09d2f3c9879d49d6b446

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git make ripgrep \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go

WORKDIR /opt/cli
COPY deploy/cli/package.json deploy/cli/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund && npm cache clean --force

# uid 10001, the uid sandboxd runs every process as (sandboxd.UID). It owns
# /work, so the fresh volume docker mounts there starts owned by it; the
# rest of the image is root's and the root filesystem is mounted read-only.
RUN useradd --uid 10001 --user-group --no-create-home --home-dir /tmp --shell /usr/sbin/nologin build \
    && mkdir /work && chown build:build /work
USER 10001:10001
ENV PATH="/opt/cli/node_modules/.bin:/usr/local/go/bin:${PATH}" \
    HOME=/tmp
WORKDIR /work
