#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# 0. Check for Rust/Cargo
export PATH="$HOME/.cargo/bin:$PATH"
if which cargo &> /dev/null || [ -f "$HOME/.cargo/bin/cargo" ]; then
  [ -f "$HOME/.cargo/env" ] && source "$HOME/.cargo/env"
  echo "✅ Rust/Cargo already installed ($(cargo --version 2>/dev/null || echo 'detected via rustup'))."
else
  echo "🦀 Rust/Cargo not found. Installing Rustup..."
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
  source "$HOME/.cargo/env"
  echo "✅ Rust installed."
fi

# 0.2 Install cargo-binstall for faster installs
if ! command -v cargo-binstall &> /dev/null; then
  echo "🚀 Installing cargo-binstall..."
  curl -L --proto '=https' --tlsv1.2 -sSf https://raw.githubusercontent.com/cargo-bins/cargo-binstall/main/install-from-binstall-release.sh | bash
  echo "✅ cargo-binstall installed."
else
  echo "✅ cargo-binstall already installed."
fi

# 0.5 Install TOML editor (tomato) and template engine (minijinja)
if ! command -v tomato &> /dev/null; then
  echo "🍅 Installing tomato-toml editor..."
  cargo binstall -y tomato-toml
  echo "✅ tomato-toml installed."
else
  echo "✅ tomato-toml already installed."
fi

if ! command -v minijinja-cli &> /dev/null; then
  echo "Templates: Installing minijinja-cli..."
  cargo binstall -y minijinja-cli
  echo "✅ minijinja-cli installed."
else
  echo "✅ minijinja-cli already installed."
fi

# 1. Install ZeroClaw
if ! command -v zeroclaw &> /dev/null; then
  echo "📥 ZeroClaw not found. Installing via cargo-binstall..."
  cargo binstall -y https://github.com/zeroclaw-labs/zeroclaw --locked
  echo "✅ ZeroClaw installed."
else
  echo "✅ ZeroClaw is already installed ($(zeroclaw --version))."
fi

# 2. Setup project structure
echo "📂 Setting up local project workspace..."
mkdir -p workspace/SOPs workspace/sessions workspace/memory workspace/state

HOSTNAME=$(hostname)
IP_ADDR=$(ip route get 1 2>/dev/null | awk '{print $7;exit}')
AGENT_NAME="${HOSTNAME}[${IP_ADDR:-127.0.0.1}]"
echo "🤖 Auto-detected Agent Name: $AGENT_NAME"

# Set name cleanly via tomato
tomato set agent.name "\"$AGENT_NAME\"" config.toml

# 3. Interactive Onboarding
echo "🛠️ Starting ZeroClaw Onboarding..."
echo "This will help you configure your AI provider and channels."
zeroclaw onboard --config-dir "$(pwd)"

# 4. AI & Manual Command Detection
CANDIDATES=("df" "free" "uptime" "cat" "head" "tail" "grep" "wc" "ls" "find" "journalctl" "ss" "ip" "ping" "dig" "git" "ps")
if whiptail --title "AI System Scan" --yesno "Would you like to let the ZeroClaw agent autonomously scan your system to find relevant monitoring commands?" 10 60; then
    echo "🔍 AI is scanning the system for monitoring tools..."
    echo "----------------------------------------------------------------"
    # Render the template with current config data using minijinja-cli
    RENDERED_PROMPT=$(minijinja-cli prompts/tool-autodetection.prompt config.toml)

    # Create a temporary file to capture the output
    AI_OUT=$(mktemp)
    # Run the agent in YOLO mode so it can autonomously probe the system
    # We stream output to terminal (tee) while saving to file
    zeroclaw agent --config-dir "$(pwd)" --approval-mode yolo -m "$RENDERED_PROMPT" 2>/dev/null | tee "$AI_OUT"

    # Parse the commands from the captured output
    AI_LIST=$(grep -oE '[a-z0-9_-]+(,[a-z0-9_-]+)*' "$AI_OUT" | tail -n 1 || echo "")
    rm -f "$AI_OUT"
    echo "----------------------------------------------------------------"

    if [ -n "$AI_LIST" ]; then
        IFS=',' read -ra ADDR <<< "$AI_LIST"
        for i in "${ADDR[@]}"; do
            # Add AI found tools to our candidate list
            CANDIDATES+=("$(echo $i | xargs)")
        done
        echo "✅ AI suggested additional tools: $AI_LIST"
    fi
fi


# Filter candidates to only those actually installed
CHECKLIST_ITEMS=()
UNIQUE_CANDIDATES=$(echo "${CANDIDATES[@]}" | tr ' ' '\n' | sort -u)
for cmd in $UNIQUE_CANDIDATES; do
    if which "$cmd" &> /dev/null; then
        CHECKLIST_ITEMS+=("$cmd" "System tool" "ON")
    fi
done

if [ ${#CHECKLIST_ITEMS[@]} -gt 0 ]; then
    echo "💡 Found ${#CHECKLIST_ITEMS[@]} total monitoring tools."
    SELECTED=$(whiptail --title "Whitelist Configuration" --checklist "Select commands to allow the Server Sentinel to execute:" 20 60 12 "${CHECKLIST_ITEMS[@]}" 3>&1 1>&2 2>&3)
    
    if [ $? -eq 0 ]; then
        echo "📝 Updating config.toml with approved whitelist..."
        # Clear existing list and append approved ones
        tomato rm autonomy.allowed_commands config.toml &>/dev/null || true
        for cmd in $(echo "$SELECTED" | tr -d '"'); do
            tomato append autonomy.allowed_commands "\"$cmd\"" config.toml &>/dev/null
        done
        echo "✅ Whitelist updated in config.toml."
    else
        echo "⚠️ Selection cancelled. Keeping default commands."
    fi
else
    echo "⚠️ No monitoring tools detected. Keeping defaults."
fi

# 5. Security & Service
echo "🛡️ Configuring Git security..."
git update-index --assume-unchanged config.toml 2>/dev/null || true

if whiptail --title "Service Installation" --yesno "Would you like to install the Server Sentinel as a system service (auto-start)?" 10 60; then
    echo "⚙️ Installing ZeroClaw service..."
    zeroclaw service install --config-dir "$(pwd)"
    
    if whiptail --title "Service Start" --yesno "Service installed. Start it now?" 10 60; then
        echo "🚀 Starting service..."
        zeroclaw service start --config-dir "$(pwd)"
        echo "✅ Service is running."
    fi
fi

echo ""
echo "🎉 Bootstrap complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Your Server Sentinel is ready."
echo "You can now run: zeroclaw agent --config-dir \$(pwd)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
