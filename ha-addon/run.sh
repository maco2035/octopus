#!/usr/bin/with-contenv bashio
set -e

CONFIG_PATH=/data/octopus-config.yaml

ADMIN_USERNAME=$(bashio::config 'admin_username')
ADMIN_PASSWORD=$(bashio::config 'admin_password')
BRANCH_PATTERN=$(bashio::config 'branch_pattern')
RUNNER_ENABLED=$(bashio::config 'runner_enabled')

# These are all schema-optional ("type?") with no entry in `options:` at
# all, per Home Assistant's own convention for a field with no default —
# which means the key can be entirely absent from options.json, not just
# empty. Verified against the actual bashio shipped in
# ghcr.io/home-assistant/amd64-base:3.19 (/usr/lib/bashio/config.sh):
# bashio::config called with only one argument returns the literal string
# "null" (not empty!) for an absent key — passing '' as the second
# argument is what makes a missing key come back as an empty string
# instead. Skipping this for the four required-with-defaults values above
# is fine; they always have a real value in options.json.
GEMINI_API_KEY=$(bashio::config 'gemini_api_key' '')
ANTHROPIC_API_KEY=$(bashio::config 'anthropic_api_key' '')
OPENAI_API_KEY=$(bashio::config 'openai_api_key' '')
SLACK_SIGNING_SECRET=$(bashio::config 'slack_signing_secret' '')
WEB_BASE_URL=$(bashio::config 'web_base_url' '')

if bashio::var.is_empty "${ADMIN_PASSWORD}"; then
    bashio::log.fatal "admin_password is not set — open this add-on's Configuration tab and set one before starting."
    exit 1
fi

# Hash it right here at startup — nobody has to run a CLI command on a dev
# machine first. Piped over stdin rather than passed as an argument so the
# plaintext password never shows up in this container's process list.
ADMIN_PASSWORD_HASH=$(echo "${ADMIN_PASSWORD}" | /usr/bin/octopus hash-password)

# octopus's own config.Load runs the whole file through
# os.Expand(content, os.Getenv) — a SECOND round of ${VAR} substitution on
# top of this script's own, meant for config.example.yaml's normal
# "${SOME_ENV_VAR}" placeholders. Writing any secret directly into the
# file here (which an unquoted heredoc would do) is unsafe for anything
# containing a literal "$" — and a bcrypt hash always does ($2a$10$...):
# os.Expand treats $2, $10, and the rest of the hash as more
# environment-variable references and silently corrupts it down to a
# handful of leftover characters. Confirmed by actually generating a hash
# and running it through os.Expand, not by assumption — this is exactly
# why config.example.yaml uses ${OCTOPUS_ADMIN_PASSWORD_HASH} instead of a
# literal hash too.
#
# The fix: write ${ENV_VAR_NAME} placeholders into the octopus config
# (the heredoc below is quoted — 'EOF', not EOF — specifically so this
# script's own substitution doesn't touch them), and export the real
# values as process environment variables for octopus's config.Load to
# resolve at the one point that's actually safe: a single substitution
# pass, not two.
export ADMIN_USERNAME ADMIN_PASSWORD_HASH BRANCH_PATTERN WEB_BASE_URL RUNNER_ENABLED
export GEMINI_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY SLACK_SIGNING_SECRET

cat > "${CONFIG_PATH}" <<'EOF'
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
