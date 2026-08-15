#!/usr/bin/env bash
# One entrypoint for both run modes this image supports: a plain
# `docker run`/docker-compose container, and a Home Assistant add-on. It
# tells them apart by whether bashio is present (only true when built
# FROM a Home Assistant base image, per the publish-ha-addon.yml CI build)
# — verified against the real ghcr.io/home-assistant/amd64-base:3.19
# image, not assumed. bashio
# itself needs bash (its lib sources fail under plain POSIX sh with a
# syntax error), which is why this script's shebang is bash and the
# Dockerfile installs it unconditionally — cheap, and harmless on the
# plain-Docker path, which never calls a bashio:: function at all.
set -e

if [ -f /usr/lib/bashio/bashio.sh ]; then
    # Home Assistant add-on: options come from the Configuration tab via
    # bashio, not container env vars.
    . /usr/lib/bashio/bashio.sh

    ADMIN_USERNAME=$(bashio::config 'admin_username')
    ADMIN_PASSWORD=$(bashio::config 'admin_password')
    BRANCH_PATTERN=$(bashio::config 'branch_pattern')
    RUNNER_ENABLED=$(bashio::config 'runner_enabled')
    # These four are schema-optional ("type?") with no entry in
    # config.yaml's `options:` at all, per Home Assistant's own convention
    # for a field with no default — which means the key can be entirely
    # absent from options.json, not just empty. bashio::config then
    # returns the literal string "null" (verified against the real bashio
    # source, /usr/lib/bashio/config.sh, and against a real mocked
    # Supervisor API — not assumed).
    #
    # Passing '' as bashio::config's second argument does NOT fix this,
    # even though it looks like it should: that source reads
    # `local default_value=${2:-null}`, and bash's ${var:-default} treats
    # an *explicitly passed empty string* the same as "not passed at
    # all" — so the fallback is still "null" regardless. Caught by
    # actually running this against a mocked Supervisor API rather than
    # trusting the source read-through; an empty-string second argument
    # silently doesn't do what it looks like it does. Post-processing the
    # "null" sentinel ourselves is what actually works — verified the
    # same way.
    # Not `[ "$X" = null ] && X=""`: under set -e, a bare `cmd1 && cmd2`
    # statement propagates cmd1's exit status when it's false — which is
    # the *normal* case here (a real value, not the literal "null") — and
    # would abort the whole script on every ordinary run. `if` is exempt
    # from errexit for its own test regardless of true/false, which is
    # what's actually needed. (Caught this by tracing through what set -e
    # does here, then confirming both branches below empirically.)
    GEMINI_API_KEY=$(bashio::config 'gemini_api_key')
    if [ "${GEMINI_API_KEY}" = "null" ]; then GEMINI_API_KEY=""; fi
    ANTHROPIC_API_KEY=$(bashio::config 'anthropic_api_key')
    if [ "${ANTHROPIC_API_KEY}" = "null" ]; then ANTHROPIC_API_KEY=""; fi
    OPENAI_API_KEY=$(bashio::config 'openai_api_key')
    if [ "${OPENAI_API_KEY}" = "null" ]; then OPENAI_API_KEY=""; fi
    SLACK_SIGNING_SECRET=$(bashio::config 'slack_signing_secret')
    if [ "${SLACK_SIGNING_SECRET}" = "null" ]; then SLACK_SIGNING_SECRET=""; fi
    WEB_BASE_URL=$(bashio::config 'web_base_url')
    if [ "${WEB_BASE_URL}" = "null" ]; then WEB_BASE_URL=""; fi
    DATA_DIR=/data

    if bashio::var.is_empty "${ADMIN_PASSWORD}"; then
        bashio::log.fatal "admin_password is not set — open this add-on's Configuration tab and set one before starting."
        exit 1
    fi
else
    # Plain Docker / docker-compose: options come straight from container
    # env vars (see docker-compose.yml).
    ADMIN_USERNAME="${OCTOPUS_ADMIN_USERNAME:-admin}"
    ADMIN_PASSWORD="${OCTOPUS_ADMIN_PASSWORD:-}"
    BRANCH_PATTERN="${OCTOPUS_BRANCH_PATTERN:-octopus/{ticket_id}}"
    RUNNER_ENABLED="${OCTOPUS_RUNNER_ENABLED:-false}"
    GEMINI_API_KEY="${GEMINI_API_KEY:-}"
    ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}"
    OPENAI_API_KEY="${OPENAI_API_KEY:-}"
    SLACK_SIGNING_SECRET="${SLACK_SIGNING_SECRET:-}"
    WEB_BASE_URL="${OCTOPUS_WEB_BASE_URL:-}"
    DATA_DIR=/app/data

    if [ -z "${ADMIN_PASSWORD}" ]; then
        echo "OCTOPUS_ADMIN_PASSWORD is not set. Set it (in a .env file next to docker-compose.yml, or -e for a plain docker run) and restart." >&2
        exit 1
    fi
fi

mkdir -p "${DATA_DIR}"
CONFIG_PATH="${DATA_DIR}/octopus-config.yaml"

# Hashed right here at startup — nobody has to run a CLI command by hand
# first. Piped over stdin rather than passed as an argument so the
# plaintext password never shows up in this container's process list.
ADMIN_PASSWORD_HASH=$(echo "${ADMIN_PASSWORD}" | /app/octopus hash-password)

# octopus's own config.Load runs the whole file through
# os.Expand(content, os.Getenv) — a substitution pass meant for
# octopus.example.yaml's normal ${VAR} placeholders. Writing any of these
# values directly into the file (what an unquoted heredoc would do) is
# unsafe for anything that might contain a literal "$" — and a bcrypt hash
# always does ($2a$10$...): os.Expand would treat $2, $10, and the rest as
# more environment-variable references and silently corrupt it down to a
# handful of leftover characters. Confirmed by actually generating a hash
# and running it through os.Expand, not by assumption. Fix: write
# ${ENV_VAR_NAME} placeholders into the config (the heredoc below is
# quoted — 'EOF', not EOF — specifically so this script's own
# substitution doesn't touch them) and export the real values as process
# environment variables for octopus's config.Load to resolve in the one
# substitution pass that's actually safe.
export ADMIN_USERNAME ADMIN_PASSWORD_HASH BRANCH_PATTERN WEB_BASE_URL RUNNER_ENABLED DATA_DIR
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

if [ -f /usr/lib/bashio/bashio.sh ]; then
    bashio::log.info "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
else
    echo "Starting Octopus on :8080 (config written to ${CONFIG_PATH})"
fi
exec /app/octopus
