#!/usr/bin/with-contenv bashio
set -e

CONFIG_PATH=/data/octopus-config.yaml

ADMIN_USERNAME=$(bashio::config 'admin_username')
ADMIN_PASSWORD=$(bashio::config 'admin_password')
GEMINI_API_KEY=$(bashio::config 'gemini_api_key')
ANTHROPIC_API_KEY=$(bashio::config 'anthropic_api_key')
OPENAI_API_KEY=$(bashio::config 'openai_api_key')
SLACK_SIGNING_SECRET=$(bashio::config 'slack_signing_secret')
BRANCH_PATTERN=$(bashio::config 'branch_pattern')
RUNNER_ENABLED=$(bashio::config 'runner_enabled')
WEB_BASE_URL=$(bashio::config 'web_base_url')

if bashio::var.is_empty "${ADMIN_PASSWORD}"; then
    bashio::log.fatal "admin_password is not set — open this add-on's Configuration tab and set one before starting."
    exit 1
fi

# Hash it right here at startup — nobody has to run a CLI command on a dev
# machine first. Piped over stdin rather than passed as an argument so the
# plaintext password never shows up in this container's process list.
ADMIN_PASSWORD_HASH=$(echo "${ADMIN_PASSWORD}" | /usr/bin/octopus hash-password)

cat > "${CONFIG_PATH}" <<EOF
port: 8080
agents:
  gemini_api_key: "${GEMINI_API_KEY}"
  anthropic_api_key: "${ANTHROPIC_API_KEY}"
  openai_api_key: "${OPENAI_API_KEY}"
slack:
  signing_secret: "${SLACK_SIGNING_SECRET}"
store:
  driver: sqlite
  dsn: /data/octopus.db
git:
  branch_pattern: "${BRANCH_PATTERN}"
  clone_cache_dir: /data/clones
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

bashio::log.info "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
exec /usr/bin/octopus
