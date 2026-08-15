#!/bin/sh
set -e

# Under Home Assistant Supervisor, /data is always provided as this add-on's
# persistent storage — plain `docker run` / docker-compose never creates a
# top-level /data (our compose file bind-mounts straight onto /app/data instead).
if [ -d /data ]; then
  DATA_DIR=/data
else
  DATA_DIR=/app/data
fi

mkdir -p "${DATA_DIR}"
CONFIG_PATH="${DATA_DIR}/octopus-config.yaml"

ADMIN_USERNAME="${OCTOPUS_ADMIN_USERNAME:-admin}"
ADMIN_PASSWORD="${OCTOPUS_ADMIN_PASSWORD:-admin}"
BRANCH_PATTERN="${OCTOPUS_BRANCH_PATTERN:-octopus/{ticket_id}}"
RUNNER_ENABLED="${OCTOPUS_RUNNER_ENABLED:-true}"
ANTIGRAVITY_API_KEY="${ANTIGRAVITY_API_KEY:-}"
ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}"
OPENAI_API_KEY="${OPENAI_API_KEY:-}"
SLACK_SIGNING_SECRET="${SLACK_SIGNING_SECRET:-}"
WEB_BASE_URL="${OCTOPUS_WEB_BASE_URL:-}"

# If /data/options.json exists (e.g. customized via Home Assistant UI), read values from it
if [ -f /data/options.json ] && command -v jq >/dev/null 2>&1; then
  OPT_USER=$(jq -r '.admin_username // empty' /data/options.json 2>/dev/null || true)
  OPT_PASS=$(jq -r '.admin_password // empty' /data/options.json 2>/dev/null || true)
  OPT_RUNNER=$(jq -r '.runner_enabled // empty' /data/options.json 2>/dev/null || true)
  OPT_BRANCH=$(jq -r '.branch_pattern // empty' /data/options.json 2>/dev/null || true)
  OPT_ANTIGRAVITY=$(jq -r '.antigravity_api_key // empty' /data/options.json 2>/dev/null || true)
  OPT_ANTHROPIC=$(jq -r '.anthropic_api_key // empty' /data/options.json 2>/dev/null || true)
  OPT_OPENAI=$(jq -r '.openai_api_key // empty' /data/options.json 2>/dev/null || true)
  OPT_SLACK=$(jq -r '.slack_signing_secret // empty' /data/options.json 2>/dev/null || true)
  OPT_WEB=$(jq -r '.web_base_url // empty' /data/options.json 2>/dev/null || true)

  [ -n "$OPT_USER" ] && [ "$OPT_USER" != "null" ] && ADMIN_USERNAME="$OPT_USER"
  [ -n "$OPT_PASS" ] && [ "$OPT_PASS" != "null" ] && ADMIN_PASSWORD="$OPT_PASS"
  [ -n "$OPT_RUNNER" ] && [ "$OPT_RUNNER" != "null" ] && RUNNER_ENABLED="$OPT_RUNNER"
  [ -n "$OPT_BRANCH" ] && [ "$OPT_BRANCH" != "null" ] && BRANCH_PATTERN="$OPT_BRANCH"
  [ -n "$OPT_ANTIGRAVITY" ] && [ "$OPT_ANTIGRAVITY" != "null" ] && ANTIGRAVITY_API_KEY="$OPT_ANTIGRAVITY"
  [ -n "$OPT_ANTHROPIC" ] && [ "$OPT_ANTHROPIC" != "null" ] && ANTHROPIC_API_KEY="$OPT_ANTHROPIC"
  [ -n "$OPT_OPENAI" ] && [ "$OPT_OPENAI" != "null" ] && OPENAI_API_KEY="$OPT_OPENAI"
  [ -n "$OPT_SLACK" ] && [ "$OPT_SLACK" != "null" ] && SLACK_SIGNING_SECRET="$OPT_SLACK"
  [ -n "$OPT_WEB" ] && [ "$OPT_WEB" != "null" ] && WEB_BASE_URL="$OPT_WEB"
fi

ADMIN_PASSWORD_HASH=$(echo "${ADMIN_PASSWORD}" | /app/octopus hash-password)

export ADMIN_USERNAME ADMIN_PASSWORD_HASH BRANCH_PATTERN WEB_BASE_URL RUNNER_ENABLED DATA_DIR
export ANTIGRAVITY_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY SLACK_SIGNING_SECRET

cat > "${CONFIG_PATH}" <<'EOF'
port: 8080
agents:
  antigravity_api_key: "${ANTIGRAVITY_API_KEY}"
  anthropic_api_key: "${ANTHROPIC_API_KEY}"
  openai_api_key: "${OPENAI_API_KEY}"
slack:
  signing_secret: "${SLACK_SIGNING_SECRET}"
store:
  driver: sqlite
  dsn: "${DATA_DIR}/octopus.db"
git:
  branch_pattern: "${BRANCH_PATTERN}"
  clone_cache_dir: "${DATA_DIR}/clones"
web:
  enabled: true
  base_url: "${WEB_BASE_URL}"
auth:
  admin_username: "${ADMIN_USERNAME}"
  admin_password_hash: "${ADMIN_PASSWORD_HASH}"
runner:
  enabled: ${RUNNER_ENABLED}
EOF

export OCTOPUS_CONFIG="${CONFIG_PATH}"
echo "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
exec /app/octopus
