# The workstation image: an agent's persistent box (sandboxd, gVisor on
# hetzner amd64 and oci arm64), where a person gets Claude Code and a
# laptop's tools in a browser terminal. Claude Code is exactly deploy/cli's
# lockfile, as in every other image. It holds no credential: orbit puts a
# GitHub token at /run/orbit/gh-token when it wants gh and git to have one.
#
# Every pin is a digest or a checksum Renovate updates: the images below by
# digest, yq by its release assets' sha256 (renovate.json's custom manager),
# pnpm by deploy/workstation/package.json's packageManager hash. The Debian,
# GitHub CLI and Docker apt packages are the repositories' current ones,
# their signing keys pinned by sha256.

# go.mod's toolchain.
FROM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS go
FROM node:24.22.2-trixie-slim@sha256:54f84b4cf8432b21aaffd5d3ce8d41e12cea0d10d33c09d2f3c9879d49d6b446 AS node
FROM ghcr.io/astral-sh/uv:0.13.6@sha256:af22a52f5eafef3d78e551599d405c02df61919d18028f0cea8eda93bc9986d0 AS uv

FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
ARG TARGETARCH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl fd-find fzf git iproute2 jq less locales make nano neovim \
        openssh-client postgresql-client procps python3 ripgrep sqlite3 sudo tmux unzip wget \
    && sed -i 's/^# *\(en_US.UTF-8 UTF-8\)/\1/' /etc/locale.gen && locale-gen \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# gh and Docker (engine, CLI, containerd, buildx, compose) from their own
# apt repositories, for an inner dockerd. A key that no longer matches its
# checksum fails the build.
ARG DOCKER_APT_GPG_SHA256=1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570
ARG GH_APT_GPG_SHA256=6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b
RUN install -d -m 0755 /etc/apt/keyrings \
    && curl -fsSL -o /etc/apt/keyrings/docker.asc https://download.docker.com/linux/debian/gpg \
    && echo "${DOCKER_APT_GPG_SHA256}  /etc/apt/keyrings/docker.asc" | sha256sum -c - \
    && curl -fsSL -o /etc/apt/keyrings/githubcli-archive-keyring.gpg https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    && echo "${GH_APT_GPG_SHA256}  /etc/apt/keyrings/githubcli-archive-keyring.gpg" | sha256sum -c - \
    && chmod a+r /etc/apt/keyrings/* \
    && arch="$(dpkg --print-architecture)" \
    && echo "deb [arch=${arch} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable" > /etc/apt/sources.list.d/docker.list \
    && echo "deb [arch=${arch} signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        containerd.io docker-buildx-plugin docker-ce docker-ce-cli docker-compose-plugin gh \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# yq, the release binary for this architecture, checked against its sha256.
# renovate: datasource=github-release-attachments depName=mikefarah/yq
ARG YQ_VERSION_AMD64=v4.54.3 YQ_SHA256_AMD64=129534601871538efe0ba4c5e62a34fb1d5f469bcb975c4fddeb467d82974973
# renovate: datasource=github-release-attachments depName=mikefarah/yq
ARG YQ_VERSION_ARM64=v4.54.3 YQ_SHA256_ARM64=89f69310b61b68d1245065a1241d1fc843f7a36cd6103ffd77ad987613bd1b67
RUN case "${TARGETARCH}" in \
        amd64) v="${YQ_VERSION_AMD64}" sum="${YQ_SHA256_AMD64}" ;; \
        arm64) v="${YQ_VERSION_ARM64}" sum="${YQ_SHA256_ARM64}" ;; \
        *) echo "no yq for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${v}/yq_linux_${TARGETARCH}" \
    && echo "${sum}  /usr/local/bin/yq" | sha256sum -c - \
    && chmod 0755 /usr/local/bin/yq

COPY --from=go /usr/local/go /usr/local/go
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack

# pnpm through corepack, installed where every user reads it, so it never
# downloads at first use.
ENV COREPACK_HOME=/usr/local/share/corepack \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0
COPY deploy/workstation/package.json /tmp/workstation/package.json
RUN corepack install -g "$(jq -r .packageManager /tmp/workstation/package.json)" \
    && corepack enable pnpm \
    && pnpm --version \
    && chmod -R a+rX "${COREPACK_HOME}" \
    && rm -r /tmp/workstation

# Exactly the lockfile. Renovate bumps deploy/cli/package.json, so a CLI
# release reaches the image through a reviewed change like any other pin.
WORKDIR /opt/cli
COPY deploy/cli/package.json deploy/cli/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund && npm cache clean --force

# gh and git take orbit's token when there is one (the wrapper is first on
# PATH; the helper serves github.com for every repository).
COPY --chmod=0755 deploy/workstation/gh /usr/local/bin/gh
COPY --chmod=0755 deploy/workstation/git-credential-orbit /usr/local/bin/git-credential-orbit
COPY --chmod=0644 deploy/workstation/tmux.conf /etc/tmux.conf
# Claude Code's status line records each account's rate limits where orbit
# reads them; managed settings, so every account and profile has it.
# (COPY --chmod would give a directory it creates the file's mode.)
RUN install -d -m 0755 /etc/claude-code
COPY --chmod=0755 deploy/workstation/orbit-statusline /usr/local/bin/orbit-statusline
COPY --chmod=0644 deploy/workstation/managed-settings.json /etc/claude-code/managed-settings.json
RUN git config --system credential.https://github.com.helper orbit

# uid 10001, the uid sandboxd runs every process as (sandboxd.UID). sudo is
# for the box's own root work (an inner dockerd, apt); it needs a container
# without no-new-privileges, which is the box profile's choice.
RUN useradd --uid 10001 --user-group --create-home --home-dir /home/agent --shell /bin/bash agent \
    && usermod -aG docker agent \
    && echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent && chmod 0440 /etc/sudoers.d/agent \
    && mkdir /work && chown agent:agent /work
USER 10001:10001
ENV PATH="/opt/cli/node_modules/.bin:/usr/local/go/bin:/home/agent/go/bin:/home/agent/.local/bin:${PATH}" \
    HOME=/home/agent \
    LANG=en_US.UTF-8 \
    TERM=xterm-256color \
    EDITOR=nvim \
    DISABLE_AUTOUPDATER=1
WORKDIR /work
# sandboxd sets its own entrypoint; this keeps a plain `docker run -d` alive.
CMD ["sleep", "infinity"]
