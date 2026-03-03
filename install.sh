#!/usr/bin/env bash
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  AuraGo Quick Installer  (Linux x86_64)
#
#  Usage:
#    curl -fsSL https://raw.githubusercontent.com/antibyte/AuraGo/main/install.sh | bash
#
#  What it does:
#    1. Clones the AuraGo repo (includes pre-built Linux binaries)
#    2. Sets execute permissions on binaries
#    3. Creates runtime directories
#    4. Generates a random AES-256 master key -> .env
#    5. Optionally installs a systemd service (if run as root)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
set -euo pipefail

REPO="https://github.com/antibyte/AuraGo.git"
INSTALL_DIR="${AURAGO_DIR:-$HOME/aurago}"
SYSTEMD_SERVICE="aurago"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info() { echo -e "${CYAN}[AuraGo]${NC} $*"; }
ok()   { echo -e "${GREEN}[  OK  ]${NC} $*"; }
warn() { echo -e "${YELLOW}[ WARN ]${NC} $*"; }
die()  { echo -e "${RED}[ERROR ]${NC} $*"; exit 1; }

echo ""
echo -e "${CYAN}╔══════════════════════════════════════╗${NC}"
echo -e "${CYAN}║         AuraGo Installer             ║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════╝${NC}"
echo ""

info "Checking dependencies..."
command -v git >/dev/null 2>&1 || die "git is required. Install with: apt install git"
ARCH=$(uname -m)

# ── Python detection & optional installation ────────────────────────────
PYTHON_MISSING_UNKNOWN=false  # set true if python absent and distro unrecognised

_detect_pkg_manager() {
    if command -v apt-get >/dev/null 2>&1; then echo "apt"
    elif command -v dnf     >/dev/null 2>&1; then echo "dnf"
    elif command -v yum     >/dev/null 2>&1; then echo "yum"
    elif command -v pacman  >/dev/null 2>&1; then echo "pacman"
    elif command -v apk     >/dev/null 2>&1; then echo "apk"
    elif command -v zypper  >/dev/null 2>&1; then echo "zypper"
    else echo "unknown"
    fi
}

_install_python() {
    local mgr="$1"
    local SUDO=""
    [ "$(id -u)" -ne 0 ] && SUDO="sudo"
    case "$mgr" in
        apt)     $SUDO apt-get install -y python3 python3-pip python3-venv ;;
        dnf)     $SUDO dnf install -y python3 python3-pip ;;
        yum)     $SUDO yum install -y python3 python3-pip ;;
        pacman)  $SUDO pacman -Sy --noconfirm python python-pip ;;
        apk)     $SUDO apk add --no-cache python3 py3-pip ;;
        zypper)  $SUDO zypper install -y python3 python3-pip ;;
    esac
}

_python_ok=true
command -v python3 >/dev/null 2>&1 || _python_ok=false
# Also require pip3 or python3 -m pip to be usable
if $_python_ok; then
    python3 -m pip --version >/dev/null 2>&1 || _python_ok=false
fi

if ! $_python_ok; then
    PKG_MGR=$(_detect_pkg_manager)
    if [ "$PKG_MGR" = "unknown" ]; then
        warn "Python 3 or pip not found and package manager could not be detected."
        PYTHON_MISSING_UNKNOWN=true
    else
        warn "Python 3 / pip not found (or pip not usable)."
        read -r -p "Install Python 3, pip and venv via $PKG_MGR? [Y/n]: " PY_REPLY < /dev/tty || true
        if [[ "${PY_REPLY:-y}" =~ ^[Yy]$ ]]; then
            info "Installing Python via $PKG_MGR ..."
            if _install_python "$PKG_MGR"; then
                ok "Python 3 and pip installed successfully."
            else
                warn "Installation failed. You may need to install Python manually."
                PYTHON_MISSING_UNKNOWN=true
            fi
        else
            warn "Skipping Python installation. Some AuraGo features (Python skills) will not work."
            PYTHON_MISSING_UNKNOWN=true
        fi
    fi
else
    ok "Python 3 + pip found."
fi
[ "$ARCH" = "x86_64" ] || warn "Architecture $ARCH detected. Pre-built binaries are x86_64 only. You may need to compile from source (requires Go 1.21+)."
ok "Dependency check done."

if [ -d "$INSTALL_DIR/.git" ]; then
    info "Existing installation found at $INSTALL_DIR -- updating..."
    git -C "$INSTALL_DIR" pull --ff-only
    ok "Updated to latest."
elif [ -d "$INSTALL_DIR" ] && [ "$(ls -A "$INSTALL_DIR")" ]; then
    warn "Directory $INSTALL_DIR exists and is not empty."
    read -r -p "This might be a non-git installation. Do you want to try to initialize it? [y/N]: " INIT_REPO < /dev/tty || true
    if [[ "${INIT_REPO:-n}" =~ ^[Yy]$ ]]; then
        info "Initializing git and fetching from $REPO..."
        git -C "$INSTALL_DIR" init
        git -C "$INSTALL_DIR" remote add origin "$REPO"
        git -C "$INSTALL_DIR" fetch
        git -C "$INSTALL_DIR" reset --hard origin/main
        ok "Converted to git repository."
    else
        die "Aborting to prevent overwriting existing files in $INSTALL_DIR."
    fi
else
    info "Cloning into $INSTALL_DIR ..."
    git clone "$REPO" "$INSTALL_DIR"
    ok "Cloned."
fi

cd "$INSTALL_DIR"
mkdir -p data agent_workspace/workdir agent_workspace/tools log
ok "Runtime directories created."

# ── Update binary ───────────────────────────────────────────────────────
GO_MIN_VERSION="1.26"
GO_FOUND=false
if command -v go >/dev/null 2>&1; then
    GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    # Simple version comparison for 1.26+ 
    if [ "$(printf '%s\n%s' "$GO_MIN_VERSION" "$GO_VERSION" | sort -V | head -n1)" = "$GO_MIN_VERSION" ]; then
        GO_FOUND=true
    fi
fi

if $GO_FOUND; then
    info "Go $GO_VERSION found — rebuilding binaries from source for your system..."
    go build -o bin/aurago_linux ./cmd/aurago && ok "bin/aurago_linux built"
    go build -o bin/lifeboat_linux ./cmd/lifeboat && ok "bin/lifeboat_linux built"
else
    info "Using pre-built binaries (Go $GO_MIN_VERSION+ not found or not installed)."
fi

chmod +x bin/aurago_linux bin/lifeboat_linux *.sh 2>/dev/null || true
ok "Binary and script permissions set."

ENV_FILE="$INSTALL_DIR/.env"
if [ -f "$ENV_FILE" ] && grep -q "AURAGO_MASTER_KEY" "$ENV_FILE"; then
    warn ".env already has AURAGO_MASTER_KEY -- keeping existing key."
else
    MASTER_KEY=$(openssl rand -hex 32 2>/dev/null || python3 -c "import secrets; print(secrets.token_hex(32))")
    printf "AURAGO_MASTER_KEY=%s\n" "$MASTER_KEY" > "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    ok "Master key generated -> $ENV_FILE"
    warn "Keep .env safe! Losing it means losing access to your encrypted vault."
fi

cat > "$INSTALL_DIR/start.sh" <<'STARTSH'
#!/bin/bash
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"
./kill_all.sh 2>/dev/null || true

if [ -z "${AURAGO_MASTER_KEY:-}" ] && [ -f "$DIR/.env" ]; then
    source "$DIR/.env"
fi

if [ -z "${AURAGO_MASTER_KEY:-}" ]; then
    echo "ERROR: AURAGO_MASTER_KEY is not set."
    echo "  Run: source .env   or   export AURAGO_MASTER_KEY=your_64_hex_chars"
    exit 1
fi

echo "Starting AuraGo..."
./bin/aurago_linux > aurago.log 2>&1 &
echo "Started (PID=$!). Web UI: http://localhost:8088"
echo "Follow logs: tail -f $DIR/aurago.log"
STARTSH
chmod +x "$INSTALL_DIR/start.sh"
ok "start.sh configured to use pre-built binary."

# ── Network binding ────────────────────────────────────────────────────
echo ""
echo -e "${YELLOW}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${YELLOW}║  ⚠  SECURITY WARNING                                        ║${NC}"
echo -e "${YELLOW}║  NEVER enable outside access on an internet-facing server!  ║${NC}"
echo -e "${YELLOW}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "  Only allow network access if AuraGo runs on a trusted local LAN"
echo "  (e.g. a home server) — never expose it directly to the internet."
echo ""
read -r -p "Enable access from outside? [y/N]: " NET_REPLY < /dev/tty || true

if [[ "${NET_REPLY:-n}" =~ ^[Yy]$ ]]; then
    SERVER_HOST="0.0.0.0"
    warn "Web UI will listen on ALL interfaces (0.0.0.0:8088)."
    warn "Make sure your firewall only allows trusted hosts!"
else
    SERVER_HOST="127.0.0.1"
    ok "Web UI will only be reachable locally (127.0.0.1:8088). ✅"
fi

# Patch config.yaml — update the host: field under the server: block
CONFIG_FILE="$INSTALL_DIR/config.yaml"
if [ -f "$CONFIG_FILE" ]; then
    # Use awk to only replace 'host:' within the 'server:' section
    awk -v host="$SERVER_HOST" '
        /^server:/ { in_server=1 }
        /^[a-z]/ && !/^server:/ { in_server=0 }
        in_server && /^[[:space:]]+host:/ { sub(/host:.*/, "host: " host) }
        { print }
    ' "$CONFIG_FILE" > "$CONFIG_FILE.tmp" && mv "$CONFIG_FILE.tmp" "$CONFIG_FILE"
    ok "config.yaml → server.host set to $SERVER_HOST"
else
    warn "config.yaml not found — skipping host configuration."
fi


SERVICE_INSTALLED=false
if command -v systemctl >/dev/null 2>&1; then
    echo ""
    if [ -f "/etc/systemd/system/${SYSTEMD_SERVICE}.service" ]; then
        info "Systemd service already exists. You can update/reinstall it."
    else
        info "Systemd detected. Installing as a service allows AuraGo to start automatically on boot."
    fi
    read -r -p "Install as systemd service? [Y/n]: " INSTALL_SERVICE < /dev/tty || true
    if [[ "${INSTALL_SERVICE:-y}" =~ ^[Yy]$ ]]; then
        SERVICE_INSTALLED=true
        source "$ENV_FILE"
        
        # Use sudo/tee to write the service file if not root
        SUDO_CMD=""
        if [ "$(id -u)" -ne 0 ]; then
            SUDO_CMD="sudo"
            info "Requesting root privileges to install the service..."
        fi

        $SUDO_CMD tee /etc/systemd/system/${SYSTEMD_SERVICE}.service > /dev/null <<EOF
[Unit]
Description=AuraGo AI Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/bin/aurago_linux --config ${INSTALL_DIR}/config.yaml
Restart=on-failure
RestartSec=5
EnvironmentFile=-${INSTALL_DIR}/.env

[Install]
WantedBy=multi-user.target
EOF
        $SUDO_CMD systemctl daemon-reload
        $SUDO_CMD systemctl enable "$SYSTEMD_SERVICE"
        ok "Systemd service installed. Start with: sudo systemctl start $SYSTEMD_SERVICE"
    fi
fi

echo ""
echo -e "${GREEN}AuraGo installed at: $INSTALL_DIR${NC}"
echo ""

if [ "$SERVICE_INSTALLED" = "true" ]; then
    echo -e "  ${CYAN}Systemd Service:${NC} sudo systemctl status $SYSTEMD_SERVICE"
    echo -e "  ${CYAN}Start/Stop:     ${NC} sudo systemctl start/stop $SYSTEMD_SERVICE"
    echo -e "  ${CYAN}Logs:           ${NC} sudo journalctl -u $SYSTEMD_SERVICE -f"
else
    echo "  1. Edit config:  nano $INSTALL_DIR/config.yaml"
    echo "     Set at minimum: llm.api_key"
    echo "  2. Load key:     source $INSTALL_DIR/.env"
    echo "  3. Start:        cd $INSTALL_DIR && ./start.sh"
    echo "  4. Open UI:      http://localhost:8088"
fi

echo ""
echo -e "  ${CYAN}Update later:    cd $INSTALL_DIR && ./update.sh${NC}"
echo ""
echo -e "${GREEN}Setup complete! You can now finish the configuration in the Web UI.${NC}"
echo -e "Go to the ${BOLD}CONFIG${NC} section to set up your LLM Provider and API keys."
echo ""

if [ "$PYTHON_MISSING_UNKNOWN" = "true" ]; then
    echo -e "${YELLOW}╔══════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${YELLOW}║  ⚠  IMPORTANT                                                    ║${NC}"
    echo -e "${YELLOW}║  Please install Python and Pip to use all features of AuraGo !  ║${NC}"
    echo -e "${YELLOW}╚══════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
fi