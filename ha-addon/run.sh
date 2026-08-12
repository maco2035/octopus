#!/usr/bin/with-contenv bashio
set -e

CONFIG_PATH=/data/octopus-config.yaml

GEMINI_API_KEY=$(bashio::config 'gemini_api_key')
ANTHROPIC_API_KEY=$(bashio::config 'anthropic_api_key')
OPENAI_API_KEY=$(bashio::config 'openai_api_key')
SLACK_BOT_TOKEN=$(bashio::config 'slack_bot_token')
SLACK_SIGNING_SECRET=$(bashio::config 'slack_signing_secret')
BRANCH_PATTERN=$(bashio::config 'branch_pattern')

cat > "${CONFIG_PATH}" <<EOF
port: 8080
agents:
  gemini_api_key: "${GEMINI_API_KEY}"
  anthropic_api_key: "${ANTHROPIC_API_KEY}"
  openai_api_key: "${OPENAI_API_KEY}"
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
EOF

export OCTOPUS_CONFIG="${CONFIG_PATH}"

bashio::log.info "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
exec /usr/bin/octopus
