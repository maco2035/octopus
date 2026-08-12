#!/usr/bin/with-contenv bashio
set -e

CONFIG_PATH=/data/octopus-config.yaml

GEMINI_API_KEY=$(bashio::config 'gemini_api_key')
ANTHROPIC_API_KEY=$(bashio::config 'anthropic_api_key')
OPENAI_API_KEY=$(bashio::config 'openai_api_key')
XAI_API_KEY=$(bashio::config 'xai_api_key')
SLACK_BOT_TOKEN=$(bashio::config 'slack_bot_token')
SLACK_SIGNING_SECRET=$(bashio::config 'slack_signing_secret')
BRANCH_PATTERN=$(bashio::config 'branch_pattern')
ADMIN_USERNAME=$(bashio::config 'admin_username')
ADMIN_PASSWORD_HASH=$(bashio::config 'admin_password_hash')

if bashio::var.is_empty "${ADMIN_PASSWORD_HASH}"; then
    bashio::log.warning "admin_password_hash is not set — the web UI's login will have no valid credentials until you set one."
    bashio::log.warning "Generate a hash on the dev machine with: go run ./cmd/octopus hash-password"
fi

cat > "${CONFIG_PATH}" <<EOF
port: 8080
agents:
  gemini_api_key: "${GEMINI_API_KEY}"
  anthropic_api_key: "${ANTHROPIC_API_KEY}"
  openai_api_key: "${OPENAI_API_KEY}"
  xai_api_key: "${XAI_API_KEY}"
slack:
  bot_token: "${SLACK_BOT_TOKEN}"
  signing_secret: "${SLACK_SIGNING_SECRET}"
store:
  driver: sqlite
  dsn: /data/octopus.db
git:
  branch_pattern: "${BRANCH_PATTERN}"
web:
  enabled: true
auth:
  admin_username: "${ADMIN_USERNAME}"
  admin_password_hash: "${ADMIN_PASSWORD_HASH}"
EOF

export OCTOPUS_CONFIG="${CONFIG_PATH}"

bashio::log.info "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
exec /usr/bin/octopus
