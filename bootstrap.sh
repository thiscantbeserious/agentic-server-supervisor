#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# Ensure ~/.cargo/bin is in PATH for this session
export PATH="$HOME/.cargo/bin:$PATH"

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
IP_ADDR=$(ip route get 1 | awk '{print $7;exit}')
AGENT_NAME="${HOSTNAME}[${IP_ADDR}]"
echo "🤖 Auto-detected Agent Name: $AGENT_NAME"

# Update config.toml with the unique name
if [ -f "config.toml" ]; then
    sed -i "s/^name = .*/name = \"$AGENT_NAME\"/" config.toml
fi

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
    if command -v "$cmd" &> /dev/null; then
        EXISTING_COMMANDS+=("$cmd")
        # By default, we mark them as ON
        CHECKLIST_ITEMS+=("$cmd" "System tool" "ON")
    fi
done

if [ ${#EXISTING_COMMANDS[@]} -gt 0 ]; then
    echo "💡 Found ${#EXISTING_COMMANDS[@]} monitoring tools."
    
    # Use whiptail for the checkbox interface
    SELECTED_COMMANDS=$(whiptail --title "Command Whitelist Configuration" \
        --checklist "Select commands to allow the Server Sentinel to execute:" 20 60 12 \
        "${CHECKLIST_ITEMS[@]}" 3>&1 1>&2 2>&3)

    if [ $? -eq 0 ]; then
        # Clean up the output (remove quotes)
        CLEAN_LIST=$(echo "$SELECTED_COMMANDS" | tr -d '"')
        
        # Convert space-separated list to TOML array format
        TOML_ARRAY="["
        for cmd in $CLEAN_LIST; do
            TOML_ARRAY+="\"$cmd\", "
        done
        TOML_ARRAY="${TOML_ARRAY%, }] " # Remove trailing comma and close
        
        echo "📝 Updating config.toml with selected commands..."
        # Robustly update the allowed_commands array in config.toml using python
        python3 -c "import sys, re; c=open('config.toml').read(); n=re.sub(r'allowed_commands = \[.*?\]', 'allowed_commands = $TOML_ARRAY', c, flags=re.DOTALL); open('config.toml', 'w').write(n)"
        echo "✅ Command whitelist updated."
    else
        echo "⚠️ Selection cancelled. Keeping fallback commands."
    fi
else
    echo "⚠️ No monitoring tools detected. Keeping fallback commands."
fi

# 5. Service Installation
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
