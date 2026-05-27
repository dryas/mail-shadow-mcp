#!/bin/sh
# MAIL-SHADOW-MCP — Docker entrypoint
#
# If /config/config.yaml does not exist yet, copy the example config as a
# starting point so the container gives a helpful message instead of crashing.

CONFIG=/config/config.yaml
EXAMPLE=/etc/mail-shadow-mcp/config.example.yaml

if [ ! -f "$CONFIG" ]; then
    echo "mail-shadow-mcp: no config.yaml found in /config — copying example config"
    echo "mail-shadow-mcp: edit /config/config.yaml and restart the container"
    cp "$EXAMPLE" "$CONFIG"
    exit 1
fi

exec mail-shadow-mcp serve "$@"
