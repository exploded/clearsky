#!/bin/bash
# server-setup.sh
#
# One-time setup for clearsky.mchugh.au on Linode Debian.
# Run as root or with sudo:
#   sudo bash scripts/server-setup.sh
#
# Assumes Caddy and the "deploy" user (with the GitHub Actions SSH key) already
# exist from an earlier project's setup (e.g. moon/train). If they don't, run
# that project's server-setup.sh first. Caddy auto-provisions TLS via
# Let's Encrypt.
#
# clearsky embeds its templates/static/migrations/tzdata, so no web assets are
# copied at deploy time - only the binary.
#
# After running, you still need to (manually):
#   1. Add DNS A record clearsky.mchugh.au -> Linode IP
#   2. Add a site block to /etc/caddy/Caddyfile proxying to 127.0.0.1:8994
#      and run: sudo systemctl reload caddy
#   3. Edit /var/www/clearsky/.env with real Discord/SMTP values
#   4. systemctl enable --now clearsky

set -e

DEPLOY_USER="deploy"

echo "=== clearsky - Server Deployment Setup ==="

# ---------------------------------------------------------------
# 1. Application directory
# ---------------------------------------------------------------
APP_DIR="/var/www/clearsky"
if [ -d "$APP_DIR" ]; then
    echo "[ok] Application directory $APP_DIR already exists"
else
    mkdir -p "$APP_DIR"
    chown www-data:www-data "$APP_DIR"
    echo "[ok] Created application directory $APP_DIR"
fi

# ---------------------------------------------------------------
# 2. .env template (edit with real notification creds afterwards)
# ---------------------------------------------------------------
ENV_FILE="$APP_DIR/.env"
if [ -f "$ENV_FILE" ]; then
    echo "[ok] .env file already exists at $ENV_FILE (not overwriting)"
else
    cat > "$ENV_FILE" << 'ENV_TEMPLATE'
# --- Server ---
CLEARSKY_ADDR=:8994
CLEARSKY_DB=clearsky.db
CLEARSKY_BASE_URL=https://clearsky.mchugh.au
CLEARSKY_LOG_LEVEL=info

# --- Site + schedule ---
CLEARSKY_TZ=Australia/Melbourne
CLEARSKY_RUN_HOUR=18
CLEARSKY_RUN_MINUTE=0
CLEARSKY_LAT=-37.79
CLEARSKY_LON=145.18
CLEARSKY_CATCHUP_ON_START=true

# --- Notifications (GO nights only). Leave blank to disable a channel. ---
CLEARSKY_DISCORD_WEBHOOK_URL=
CLEARSKY_SMTP_HOST=smtp.gmail.com
CLEARSKY_SMTP_PORT=587
CLEARSKY_SMTP_USER=
CLEARSKY_SMTP_PASS=
CLEARSKY_EMAIL_TO=
ENV_TEMPLATE
    chown www-data:www-data "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    echo "[ok] Created .env at $ENV_FILE - edit notification creds before starting"
fi

# ---------------------------------------------------------------
# 3. systemd service
# ---------------------------------------------------------------
SERVICE_FILE="/etc/systemd/system/clearsky.service"
cat > "$SERVICE_FILE" << 'SERVICE'
[Unit]
Description=clearsky astrophotography go/no-go
After=network.target

[Service]
Type=simple
User=www-data
Group=www-data
WorkingDirectory=/var/www/clearsky
EnvironmentFile=/var/www/clearsky/.env
ExecStart=/var/www/clearsky/clearsky
Restart=on-failure
RestartSec=5

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
# SQLite needs to write its DB + WAL files in the working directory
ReadWritePaths=/var/www/clearsky

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
echo "[ok] Created systemd service at $SERVICE_FILE"

# ---------------------------------------------------------------
# 4. Install the deploy script (runs as root via sudo).
# ---------------------------------------------------------------
DEPLOY_SCRIPT_URL="https://raw.githubusercontent.com/exploded/clearsky/master/scripts/deploy-clearsky"
if ! curl -fsSL "$DEPLOY_SCRIPT_URL" -o /usr/local/bin/deploy-clearsky; then
    echo "[error] Failed to download deploy-clearsky from $DEPLOY_SCRIPT_URL"
    echo "        (If this is the first deploy and the repo is empty,"
    echo "         the deploy bundle's self-update logic will install it"
    echo "         on first deployment. You can also copy it manually.)"
fi
[ -f /usr/local/bin/deploy-clearsky ] && chmod +x /usr/local/bin/deploy-clearsky

# ---------------------------------------------------------------
# 5. sudoers - allow the existing deploy user to run our deploy script
# ---------------------------------------------------------------
SUDOERS_FILE="/etc/sudoers.d/clearsky-deploy"
cat > "$SUDOERS_FILE" << 'EOF'
deploy ALL=(ALL) NOPASSWD: /usr/local/bin/deploy-clearsky
deploy ALL=(ALL) NOPASSWD: /usr/bin/systemctl stop clearsky
EOF
chmod 440 "$SUDOERS_FILE"
visudo -c -f "$SUDOERS_FILE"
echo "[ok] sudoers entry created at $SUDOERS_FILE"

# ---------------------------------------------------------------
# 6. Caddy site block - print snippet for the user to add manually
# ---------------------------------------------------------------
echo "[note] Add the following block to /etc/caddy/Caddyfile, then run:"
echo "       sudo systemctl reload caddy"
echo "       Caddy will provision the TLS cert on first request."
cat << 'CADDY'

clearsky.mchugh.au {
    reverse_proxy 127.0.0.1:8994
}

CADDY

echo ""
echo "=== Setup complete ==="
echo "Next manual steps:"
echo "  1. DNS: point clearsky.mchugh.au A record at this server"
echo "  2. Add the Caddy block above to /etc/caddy/Caddyfile, then:"
echo "     sudo systemctl reload caddy"
echo "  3. Edit $ENV_FILE with real CLEARSKY_DISCORD_WEBHOOK_URL / SMTP creds"
echo "  4. systemctl enable --now clearsky"
echo ""
echo "Existing GitHub Actions secrets (DEPLOY_HOST/USER/SSH_KEY) from other"
echo "projects can be re-used; just push to master to deploy."
