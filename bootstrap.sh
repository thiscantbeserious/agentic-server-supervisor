#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# 1. Check for Rust/Cargo
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

# 2. Install cargo-binstall for faster installs
if ! command -v cargo-binstall &> /dev/null; then
  echo "🚀 Installing cargo-binstall..."
  curl -L --proto '=https' --tlsv1.2 -sSf https://raw.githubusercontent.com/cargo-bins/cargo-binstall/main/install-from-binstall-release.sh | bash
  echo "✅ cargo-binstall installed."
else
  echo "✅ cargo-binstall already installed."
fi

# 3. Install TOML editor (tomato) and template engine (minijinja)
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

# 4. Install ZeroClaw (with Persistent Build Cache)
echo "📥 Preparing ZeroClaw from PR-branch (Gemini Fix #5106)..."
BUILD_DIR="$(pwd)/.build/zeroclaw"
mkdir -p "$BUILD_DIR"

if [ ! -d "$BUILD_DIR/.git" ]; then
  echo "⏳ Cloning ZeroClaw repository..."
  git clone https://github.com/rareba/zeroclaw.git "$BUILD_DIR" --branch fix/4879-gemini-oauth
else
  echo "🔄 Updating ZeroClaw source..."
  (cd "$BUILD_DIR" && git pull origin fix/4879-gemini-oauth)
fi

echo "🏗️ Building ZeroClaw (this uses incremental caching)..."
(cd "$BUILD_DIR" && cargo build --release -p zeroclawlabs)

echo "📦 Installing ZeroClaw binary..."
cp "$BUILD_DIR/target/release/zeroclaw" "$HOME/.cargo/bin/zeroclaw"
echo "✅ ZeroClaw (PR-5106) installed and cached."

# 5. Install Local Management CLI (sentinel-cli)
echo "🛠️ Building local management CLI..."
(cd cli && cargo install --path .)
echo "✅ Local management CLI 'sentinel-cli' installed."

# 6. Setup project structure
echo "📂 Setting up local project workspace..."
mkdir -p workspace/SOPs workspace/sessions workspace/memory workspace/state

# 7. Auto-detect unique Agent Name (hostname[ip])
HOSTNAME=$(hostname)
IP_ADDR=$(ip route get 1 2>/dev/null | awk '{print $7;exit}')
AGENT_NAME="${HOSTNAME}[${IP_ADDR:-127.0.0.1}]"
echo "🤖 Auto-detected Agent Name: $AGENT_NAME"

# Set name cleanly via tomato
tomato set agent.name "\"$AGENT_NAME\"" config.toml

# 8. Interactive Onboarding
echo "🛠️ Starting ZeroClaw Onboarding..."
echo "This will help you configure your AI provider and channels."
zeroclaw onboard --config-dir "$(pwd)"

# 9. AI & Manual Command Detection
CANDIDATES=("df" "free" "uptime" "cat" "head" "tail" "grep" "wc" "ls" "find" "journalctl" "ss" "ip" "ping" "dig" "git" "ps")
if whiptail --title "AI System Scan" --yesno "Would you like to let the ZeroClaw agent autonomously scan your system to find relevant monitoring commands?" 10 60; then
    echo "🔍 AI is scanning the system for monitoring tools..."
    echo "----------------------------------------------------------------"
    # Render the template with current config data using minijinja-cli
    RENDERED_PROMPT=$(minijinja-cli prompts/tool-autodetection.prompt config.toml)

    # Small sleep to prevent burst rate limiting after rendering
    sleep 2

    # Create a temporary file to capture the output
    AI_OUT=$(mktemp)
    # Run the agent with increased reliability for the scan
    # We stream output to terminal (tee) while saving to file
    zeroclaw agent --config-dir "$(pwd)" -m "$RENDERED_PROMPT" 2>/dev/null | tee "$AI_OUT"

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
            # tomato handles string quoting automatically for the TOML file
            tomato append autonomy.allowed_commands "$cmd" config.toml &>/dev/null
        done
        echo "✅ Whitelist updated in config.toml."
    else
        echo "⚠️ Selection cancelled. Keeping default commands."
    fi
else
    echo "⚠️ No monitoring tools detected. Keeping defaults."
fi

# 10. AI Filesystem Policy Detection
if whiptail --title "AI Filesystem Scan" --yesno "Would you like to let the ZeroClaw agent autonomously suggest additional allowed and forbidden directories?" 10 60; then
    echo "🔍 AI is scanning the filesystem for policy recommendations..."
    
    # 1. Allowed Roots
    echo "--- Detecting Allowed Roots ---"
    RENDERED_ALLOWED=$(minijinja-cli prompts/allowed-folders.prompt config.toml)
    AI_ALLOWED_OUT=$(mktemp)
    zeroclaw agent --config-dir "$(pwd)" -m "$RENDERED_ALLOWED" 2>/dev/null | tee "$AI_ALLOWED_OUT"
    AI_ALLOWED_LIST=$(grep -oE '[a-zA-Z0-9_/.-]+(,[a-zA-Z0-9_/.-]+)*' "$AI_ALLOWED_OUT" | tail -n 1 || echo "")
    rm -f "$AI_ALLOWED_OUT"
    
    if [ -n "$AI_ALLOWED_LIST" ]; then
        IFS=',' read -ra ADDR <<< "$AI_ALLOWED_LIST"
        ALLOWED_CHECKLIST=()
        for path in "${ADDR[@]}"; do
            path=$(echo $path | xargs)
            [ -d "$path" ] && ALLOWED_CHECKLIST+=("$path" "System Path" "ON")
        done
        
        if [ ${#ALLOWED_CHECKLIST[@]} -gt 0 ]; then
            SELECTED_ALLOWED=$(whiptail --title "Allowed Roots Configuration" --checklist "Approve additional read-only system roots:" 20 60 12 "${ALLOWED_CHECKLIST[@]}" 3>&1 1>&2 2>&3)
            if [ $? -eq 0 ]; then
                for path in $(echo "$SELECTED_ALLOWED" | tr -d '"'); do
                    tomato append autonomy.allowed_roots "$path" config.toml &>/dev/null
                done
                echo "✅ Allowed roots updated."
            fi
        fi
    fi

    # 2. Forbidden Paths
    echo "--- Detecting Forbidden Paths ---"
    RENDERED_FORBIDDEN=$(minijinja-cli prompts/forbidden-folders.prompt config.toml)
    AI_FORBIDDEN_OUT=$(mktemp)
    zeroclaw agent --config-dir "$(pwd)" -m "$RENDERED_FORBIDDEN" 2>/dev/null | tee "$AI_FORBIDDEN_OUT"
    AI_FORBIDDEN_LIST=$(grep -oE '[a-zA-Z0-9_/.-]+(,[a-zA-Z0-9_/.-]+)*' "$AI_FORBIDDEN_OUT" | tail -n 1 || echo "")
    rm -f "$AI_FORBIDDEN_OUT"

    if [ -n "$AI_FORBIDDEN_LIST" ]; then
        IFS=',' read -ra ADDR <<< "$AI_FORBIDDEN_LIST"
        FORBIDDEN_CHECKLIST=()
        for path in "${ADDR[@]}"; do
            path=$(echo $path | xargs)
            [ -e "$path" ] && FORBIDDEN_CHECKLIST+=("$path" "Sensitive Path" "ON")
        done
        
        if [ ${#FORBIDDEN_CHECKLIST[@]} -gt 0 ]; then
            SELECTED_FORBIDDEN=$(whiptail --title "Forbidden Paths Configuration" --checklist "Approve additional security blocks:" 20 60 12 "${FORBIDDEN_CHECKLIST[@]}" 3>&1 1>&2 2>&3)
            if [ $? -eq 0 ]; then
                for path in $(echo "$SELECTED_FORBIDDEN" | tr -d '"'); do
                    tomato append autonomy.forbidden_paths "$path" config.toml &>/dev/null
                done
                echo "✅ Forbidden paths updated."
            fi
        fi
    fi
fi

# 11. Security & Service
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
