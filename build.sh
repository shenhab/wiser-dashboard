#!/bin/sh
# Builds wiser-dashboard; see tools/build for the options (-all, -target, -package, -matter).
cd "$(dirname "$0")" && exec go run ./tools/build "$@"
