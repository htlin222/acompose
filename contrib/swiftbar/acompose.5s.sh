#!/bin/bash
# SwiftBar/xbar plugin for acompose — https://github.com/htlin222/acompose
# Install: brew install swiftbar; copy this file into your SwiftBar plugin
# folder; edit PROJECT_DIR. The 5s in the filename is the refresh interval.
PROJECT_DIR="$HOME/change-me-to-your-compose-project"
ACOMPOSE="$(command -v acompose || echo /opt/homebrew/bin/acompose)"
cd "$PROJECT_DIR" 2>/dev/null || { echo "⛴ ?"; echo "---"; echo "PROJECT_DIR not found — edit $0 | color=red"; exit 0; }
exec "$ACOMPOSE" menubar
