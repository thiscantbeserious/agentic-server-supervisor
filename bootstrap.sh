#!/bin/bash
set -e

echo "🚀 Bootstrapping ZeroClaw for Agentic Server Supervisor..."

# 1. Install ZeroClaw if not present
if ! command -v zeroclaw &> /dev/null; then
  echo "📥 ZeroClaw not found. Installing via Option A (Clone + Local script)..."
  TMP_DIR=$(mktemp -d)
  git clone https://github.com/zeroclaw-labs/zeroclaw.git "$TMP_DIR/zeroclaw"
  cd "$TMP_DIR/zeroclaw"
  
  # Run the interactive installer
  ./install.sh
  
  cd - > /dev/null
  rm -rf "$TMP_DIR"
  echo "✅ ZeroClaw installed."
  echo "⚠️ Please ensure ~/.cargo/bin is in your PATH. You may need to run: source ~/.cargo/env"
else
  echo "✅ ZeroClaw is already installed."
fi

# 2. Setup project-local structure
echo "📂 Setting up local project workspace..."

# Ensure necessary directories exist within the project
mkdir -p workspace/SOPs
mkdir -p workspace/sessions
mkdir -p workspace/memory
mkdir -p workspace/state

# config.toml is already provided in the repo.
if [ ! -f "config.toml" ]; then
    echo "⚠️ Warning: config.toml not found. You may need to run 'zeroclaw onboard'."
fi

echo ""
echo "🎉 Bootstrap complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Next Steps:"
echo "1. Run the interactive onboarding for this project:"
echo "   zeroclaw onboard --config-dir \$(pwd)"
echo ""
echo "2. Once configured, you can start the server sentinel:"
echo "   zeroclaw agent --config-dir \$(pwd)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
