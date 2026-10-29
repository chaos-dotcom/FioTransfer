#!/bin/sh
# Render frontend/dist at 1180x780 under the egui harness's fixture.
#   ./render.sh              -> /tmp/web-shots/{transfers,watch,history,settings}.png
#   ./render.sh --out DIR    -> somewhere else
set -eu
exec node "$(dirname "$0")/render.js" "$@"
