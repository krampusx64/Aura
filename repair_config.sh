#!/bin/bash
# AuraGo config.yaml repair script (no Python yaml module needed)
# Fixes corrupted budget.models entries (removes '[object ...' strings)
# Run this on the server: bash repair_config.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="$SCRIPT_DIR/config.yaml"
BACKUP="$SCRIPT_DIR/config.yaml.bak.$(date +%s)"

if [ ! -f "$CONFIG" ]; then
    echo "ERROR: config.yaml not found at $CONFIG"
    exit 1
fi

echo "Backing up config.yaml to $BACKUP"
cp "$CONFIG" "$BACKUP"

# Check if the file is corrupted
if ! grep -q "\[object" "$CONFIG"; then
    echo "No corruption detected in config.yaml. Nothing to fix."
    exit 0
fi

echo "Corruption detected! Fixing budget.models section..."

# Remove lines that contain '[object' (corrupted JS-stringified values)
# These will look like:  - '[object Object]'  or  - - '[object Object]'
sed -i '/\[object/d' "$CONFIG"

# Check if models section is now empty / missing entries
# Restore the default model entries if the models: block is empty
python3 -c "
import sys, re

with open('$CONFIG', 'r') as f:
    content = f.read()

# Find the models: block and check if it has any entries
models_match = re.search(r'(    models:\s*\n)((?:        .*\n)*)', content)
if models_match and not models_match.group(2).strip():
    # Empty models block - restore defaults
    replacement = models_match.group(1) + \
        '        - input_per_million: 0\n' + \
        '          name: arcee-ai/trinity-large-preview:free\n' + \
        '          output_per_million: 0\n' + \
        '        - input_per_million: 0.075\n' + \
        '          name: google/gemini-2.5-flash-lite-preview-09-2025\n' + \
        '          output_per_million: 0.3\n'
    content = content[:models_match.start()] + replacement + content[models_match.end():]
    with open('$CONFIG', 'w') as f:
        f.write(content)
    print('Restored default model entries.')
else:
    print('Models section looks OK after cleanup.')
" 2>/dev/null || echo "(Python not available for model restore - check models: section manually)"

echo ""
echo "config.yaml repaired! Now restart AuraGo:"
echo "  sudo systemctl start aurago   OR   ./start.sh"
