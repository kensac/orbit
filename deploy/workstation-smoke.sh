#!/usr/bin/env bash
# workstation-smoke.sh IMAGE: every tool in the workstation image
# (deploy/workstation.Dockerfile) answers as the box's user, and the gh and
# git credential wiring reads /run/orbit/gh-token only when it is there.
# workstation.yml runs it on a pull request's image on each architecture,
# and on main before any tag points at the pushed one.
set -euo pipefail
image=${1:?usage: deploy/workstation-smoke.sh IMAGE}
docker run --rm "$image" bash -eu -c '
  test "$(id -u)" = 10001 && test "$(id -un)" = agent && test "$HOME" = /home/agent
  sudo -n true
  claude --version
  go version
  node --version
  npm --version
  pnpm --version
  python3 --version
  uv --version
  uvx --version
  yq --version
  gh --version
  git --version
  jq --version
  rg --version | head -1
  fd --version
  fzf --version
  curl --version | head -1
  wget --version | head -1
  make --version | head -1
  gcc --version | head -1
  tmux -V
  nvim --version | head -1
  nano --version | head -1
  sqlite3 --version
  psql --version
  ssh -V
  less --version | head -1
  ps --version
  ip -V
  unzip -v | head -1
  docker --version
  docker buildx version
  docker compose version
  dockerd --version
  containerd --version
  locale -a | grep -qix en_US.utf8
  infocmp tmux-256color >/dev/null
  tmux new-session -d -s smoke && test "$(tmux show -gv set-clipboard)" = on && test "$(tmux show -gv window-size)" = latest && test "$(tmux show -gv mouse)" = on && tmux show -gv update-environment | grep -qx CLAUDE_CODE_OAUTH_TOKEN && tmux kill-server
  export GIT_TERMINAL_PROMPT=0
  if printf "protocol=https\nhost=github.com\n\n" | git credential fill 2>/dev/null | grep -q "^password="; then exit 1; fi
  sudo install -d -m 0755 /run/orbit && echo smoke-token | sudo tee /run/orbit/gh-token >/dev/null
  printf "protocol=https\nhost=github.com\n\n" | git credential fill | grep -qx "password=smoke-token"
  out=$(gh auth status 2>&1 || true); grep -q GH_TOKEN <<<"$out" || { echo "$out"; exit 1; }
  test "$(jq -r .statusLine.command /etc/claude-code/managed-settings.json)" = /usr/local/bin/orbit-statusline
  line=$(echo "{\"model\":{\"display_name\":\"Opus\"},\"rate_limits\":{\"five_hour\":{\"used_percentage\":12.5,\"resets_at\":1793000000}}}" | CLAUDE_CONFIG_DIR=/tmp/acct orbit-statusline)
  test "$line" = "Opus  5h 12.5%"
  jq -e ".observed_at > 0 and .rate_limits.five_hour.used_percentage == 12.5" /tmp/acct/.orbit/usage.json
'
echo "workstation-smoke: ok: $image"
