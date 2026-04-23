#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# 0. Check for Rust/Cargo
export PATH="$HOME/.cargo/bin:$PATH"
if which cargo &> /dev/null || [ -f "$HOME/.cargo/bin/cargo" ]; then
  [ -f "$HOME/.cargo/env" ] && source "$HOME/.cargo/env"
  echo "✅ Rust/Cargo detected."
else
  echo "🦀 Rust/Cargo not found. Installing Rustup..."
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
  source "$HOME/.cargo/env"
fi

# 0.5 Install TOML editor (tomato)
if ! command -v tomato &> /dev/null; then
  echo "🍅 Installing tomato-toml editor..."
  cargo install tomato-toml
fi

# 1. Install ZeroClaw
if ! command -v zeroclaw &> /dev/null; then
  echo "📥 Installing ZeroClaw..."
  TMP_DIR=$(mktemp -d)
  git clone https://github.com/zeroclaw-labs/zeroclaw.git "$TMP_DIR/zeroclaw"
  cd "$TMP_DIR/zeroclaw" && ./install.sh
  cd - > /dev/null && rm -rf "$TMP_DIR"
fi

# 2. Setup project structure
echo "📂 Setting up local project workspace..."
mkdir -p workspace/SOPs workspace/sessions workspace/memory workspace/state

HOSTNAME=$(hostname)
IP_ADDR=$(ip route get 1 2>/dev/null | awk '{print $7;exit}')
AGENT_NAME="${HOSTNAME}[${IP_ADDR:-127.0.0.1}]"
echo "🤖 Auto-detected Agent Name: $AGENT_NAME"
tomato set agent.name "\"$AGENT_NAME\"" config.toml

# 3. Interactive Onboarding
zeroclaw onboard --config-dir "$(pwd)"

# 4. AI & Manual Command Detection
CANDIDATES=("df" "free" "uptime" "cat" "head" "tail" "grep" "wc" "ls" "find" "journalctl" "ss" "ip" "ping" "dig" "git" "ps")

if whiptail --title "AI System Scan" --yesno "Would you like to let the ZeroClaw agent autonomously scan your system to find relevant monitoring commands?" 10 60; then
    echo "🔍 AI is scanning the system..."
    AI_LIST=$(zeroclaw agent --config-dir "$(pwd)" -m "Scan the system for installed read-only monitoring and diagnostic commands. Return ONLY a comma-separated list of command names." 2>/dev/null | grep -oE '[a-z0-9_-]+(,[a-z0-9_-]+)*' || echo "")
    if [ -n "$AI_LIST" ]; then
        IFS=',' read -ra ADDR <<< "$AI_LIST"
        for i in "${ADDR[@]}"; do CANDIDATES+=("$(echo $i | xargs)"); done
    fi
fi

CHECKLIST_ITEMS=()
UNIQUE_CANDIDATES=$(echo "${CANDIDATES[@]}" | tr ' ' '\n' | sort -u)
for cmd in $UNIQUE_CANDIDATES; do
    if which "$cmd" &> /dev/null; then
        CHECKLIST_ITEMS+=("$cmd" "System tool" "ON")
    fi
done

if [ ${#CHECKLIST_ITEMS[@]} -gt 0 ]; then
    SELECTED=$(whiptail --title "Whitelist Configuration" --checklist "Approve monitoring commands:" 20 60 12 "${CHECKLIST_ITEMS[@]}" 3>&1 1>&2 2>&3)
    if [ $? -eq 0 ]; then
        echo "📝 Updating config.toml..."
        tomato rm autonomy.allowed_commands config.toml &>/dev/null || true
        for cmd in $(echo "$SELECTED" | tr -d '"'); do
            tomato append autonomy.allowed_commands "\"$cmd\"" config.toml &>/dev/null
        done
        echo "✅ Whitelist updated."
    fi
fi

# 5. Security & Service
git update-index --assume-unchanged config.toml 2>/dev/null || true
if whiptail --title "Service" --yesno "Install as a system service?" 10 60; then
    zeroclaw service install --config-dir "$(pwd)"
    whiptail --yesno "Start service now?" 10 60 && zeroclaw service start --config-dir "$(pwd)"
fi

echo "🎉 Bootstrap complete! Start with: zeroclaw agent --config-dir \$(pwd)"
