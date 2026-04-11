#!/usr/bin/env bash
# stop.sh — stop the ubersdr_wefax service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/wefax"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_wefax..."
docker compose down
echo "Done."
