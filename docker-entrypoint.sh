#!/usr/bin/env bash
set -e

CONFIG_FILE="/app/config.yaml"
ENV_FILE="/app/data/.env"

# 1. Handle config.yaml creation
# If a user deploys via Dockge/Portainer without creating the file on the host first,
# Docker creates an empty directory named "config.yaml". We must fix this.
if [ -d "$CONFIG_FILE" ]; then
    echo "[Entrypoint] Detected config.yaml as a directory. Fixing..."
    rm -rf "$CONFIG_FILE"
fi

if [ ! -f "$CONFIG_FILE" ]; then
    echo "[Entrypoint] config.yaml not found. Generating default configuration..."
    # We copy the default config template from the repo (it should be copied to /app/config.yaml.default in the Dockerfile)
    if [ -f "/app/config.yaml.default" ]; then
        cp "/app/config.yaml.default" "$CONFIG_FILE"
    else
        echo "server:" > "$CONFIG_FILE"
        echo "  port: 8088" >> "$CONFIG_FILE"
        echo "  host: 0.0.0.0" >> "$CONFIG_FILE"
    fi
    # Ensure correct permissions so the Web UI can save changes
    chmod 644 "$CONFIG_FILE"
fi

# 1.1. Automatic Merge
# Merge the existing config with the default one to add missing keys
if [ -f "/app/config.yaml.default" ] && [ -f "/app/config-merger" ]; then
    echo "[Entrypoint] Merging configuration to ensure all options are present..."
    /app/config-merger -source "$CONFIG_FILE" -template "/app/config.yaml.default"
fi

# 2. Handle Master Key generation
# If the user didn't pass AURAGO_MASTER_KEY in docker-compose.yml, we need to generate one
# and persist it so the database isn't locked out on the next restart.
if [ -z "${AURAGO_MASTER_KEY:-}" ]; then
    if [ -f "$ENV_FILE" ]; then
        echo "[Entrypoint] Loading master key from $ENV_FILE"
        source "$ENV_FILE"
        export AURAGO_MASTER_KEY
    else
        echo "[Entrypoint] AURAGO_MASTER_KEY not set. Generating a new secure key..."
        NEW_KEY=$(cat /dev/urandom | tr -dc 'a-f0-9' | fold -w 64 | head -n 1)
        export AURAGO_MASTER_KEY="$NEW_KEY"
        
        # Save it to the persistent data volume
        echo "AURAGO_MASTER_KEY=\"$NEW_KEY\"" > "$ENV_FILE"
        chmod 600 "$ENV_FILE"
        
        echo "=========================================================================="
        echo "⚠️  IMPORTANT SECURITY NOTICE ⚠️"
        echo "A new Master Key was automatically generated to encrypt your vault."
        echo "The key has been saved to your data volume: data/.env"
        echo "Please back up this key! If you lose it, you lose access to your memory."
        echo "=========================================================================="
    fi
fi

# Execute the main process (aurago)
echo "[Entrypoint] Starting AuraGo..."
exec "$@"
