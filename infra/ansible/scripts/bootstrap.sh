#!/bin/bash
# Bootstrap script for new hosts
# Installs Python and sets up SSH access
#
# Usage:
#   ./scripts/bootstrap.sh root@new-host

set -euo pipefail

if [ $# -eq 0 ]; then
	echo "Usage: $0 user@host"
	exit 1
fi

TARGET="$1"

echo "Bootstrapping $TARGET..."

# Install Python (required for Ansible)
ssh "$TARGET" "apt-get update && apt-get install -y python3 python3-apt"

echo "Bootstrap complete. You can now run Ansible against $TARGET"
