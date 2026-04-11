#!/usr/bin/env bash
# start.sh — start the ubersdr_wefax service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/wefax"

cd "${INSTALL_DIR}"
echo "Starting ubersdr_wefax..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6094"
