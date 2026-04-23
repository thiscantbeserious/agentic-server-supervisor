#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# 0. Check for Rust/Cargo
# Ensure we check common paths even if not in current PATH
export PATH="$HOME/.cargo/bin:$PATH"

if which cargo &> /dev/null || which rustup &> /dev/null || [ -f "$HOME/.cargo/bin/cargo" ]; then
  [ -f "$HOME/.cargo/env" ] && source "$HOME/.cargo/env"
  echo "✅ Rust/Cargo detected ($(cargo --version 2>/dev/null || echo 'installed via rustup'))."
else
  echo "🦀 Rust/Cargo not found. Installing Rustup..."
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
  source "$HOME/.cargo/env"
  echo "✅ Rust installed."
fi

# 0.5 Install TOML editor (tomato)
if ! command -v tomato &> /dev/null; then
  echo "🍅 Installing tomato-toml editor..."
  cargo install tomato-toml
fi

# 1. Install ZeroClaw if not present
if ! command -v zeroclaw &> /dev/null; then
  echo "📥 ZeroClaw not found. Installing via Option A (Clone + Local script)..."
  TMP_DIR=$(mktemp -d)
  git clone https://github.com/zeroclaw-labs/zeroclaw.git "$TMP_DIR/zeroclaw"
  cd "$TMP_DIR/zeroclaw"
  ./install.sh
  cd - > /dev/null
  rm -rf "$TMP_DIR"
  echo "✅ ZeroClaw installed."
else
  echo "✅ ZeroClaw is already installed."
fi

# 2. Setup project-local structure
echo "📂 Setting up local project workspace..."
mkdir -p workspace/SOPs workspace/sessions workspace/memory workspace/state

# Auto-detect unique Agent Name (hostname[ip])
HOSTNAME=$(hostname)
IP_ADDR=$(ip route get 1 2>/dev/null | awk '{print $7;exit}')
[ -z "$IP_ADDR" ] && IP_ADDR="127.0.0.1"
AGENT_NAME="${HOSTNAME}[${IP_ADDR}]"
echo "🤖 Auto-detected Agent Name: $AGENT_NAME"

# Update agent name using tomato (double quotes ensure it's saved as a TOML string)
tomato set agent.name "\"$AGENT_NAME\"" config.toml

# 3. Interactive Onboarding
echo "🛠️ Starting ZeroClaw Onboarding..."
echo "This will help you configure your AI provider and channels."
zeroclaw onboard --config-dir "$(pwd)"

# 4. Auto-detect System Commands
echo "🔍 Detecting potential read-only monitoring commands on your system..."

CANDIDATES=(
    "df" "free" "uptime" "cat" "head" "tail" "grep" "wc" "ls" "find" 
    "journalctl" "ss" "ip" "ping" "dig" "git" "ps" "top" "htop" 
    "vmstat" "iostat" "netstat" "lsof" "lsblk" "lscpu" "lsusb" 
    "sensors" "who" "last" "nmcli" "timedatectl"
)

EXISTING_COMMANDS=()
CHECKLIST_ITEMS=()

for cmd in "${CANDIDATES[@]}"; do
    if which "$cmd" &> /dev/null; then
        EXISTING_COMMANDS+=("$cmd")
        CHECKLIST_ITEMS+=("$cmd" "System tool" "ON")
    fi
done

if [ ${#EXISTING_COMMANDS[@]} -gt 0 ]; then
    echo "💡 Found ${#EXISTING_COMMANDS[@]} monitoring tools."
    
    SELECTED_COMMANDS=$(whiptail --title "Command Whitelist Configuration" \
        --checklist "Select commands to allow the Server Sentinel to execute:" 20 60 12 \
        "${CHECKLIST_ITEMS[@]}" 3>&1 1>&2 2>&3)

    if [ $? -eq 0 ]; then
        # whiptail returns "df" "free" - we need to strip ALL quotes
        CLEAN_LIST=$(echo "$SELECTED_COMMANDS" | tr -d '"')
        
        echo "📝 Updating config.toml with selected commands..."
        # Clear existing list and append new ones using tomato
        tomato rm autonomy.allowed_commands config.toml &>/dev/null || true
        for cmd in $CLEAN_LIST; do
            # tomato handles string quoting automatically for the TOML file
            tomato append autonomy.allowed_commands "$cmd" config.toml &>/dev/null
        done
        echo "✅ Command whitelist updated."
    else
        echo "⚠️ Selection cancelled. Keeping fallback commands."
    fi
else
    echo "⚠️ No monitoring tools detected. Keeping fallback commands."
fi

# 5. Security: Prevent leaking local config
echo "🛡️ Configuring Git to ignore local changes to config.toml..."
git update-index --assume-unchanged config.toml 2>/dev/null || true

# 6. Service Installation
if whiptail --title "Service Installation" --yesno "Would you like to install the Server Sentinel as a system service (auto-start)?" 10 60; then
    echo "⚙️ Installing ZeroClaw service..."
    # Ensure cargo path is available for the service
    zeroclaw service install --config-dir "$(pwd)"
    
    if whiptail --title "Service Start" --yesno "Service installed successfully. Would you like to start it now?" 10 60; then
        echo "🚀 Starting Server Sentinel service..."
        zeroclaw service start --config-dir "$(pwd)"
        echo "✅ Service started."
    fi
else
    echo "ℹ️ Skipping service installation. You can run 'zeroclaw service install' manually later."
fi

echo ""
echo "🎉 Bootstrap complete!"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Your Server Sentinel is ready."
echo "You can now start it with: zeroclaw agent --config-dir \$(pwd)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
