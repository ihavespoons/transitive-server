#!/bin/bash
# Claudette hook script - POSTs hook events to the local Claudette server.
# Claude Code pipes JSON to stdin; we forward it and return any response.
INPUT=$(cat)
RESPONSE=$(echo "$INPUT" | curl -s -X POST --max-time 300 \
    -H "Content-Type: application/json" \
    "http://localhost:${CLAUDETTE_PORT:-7865}/hook" --data-binary @-)
[ -n "$RESPONSE" ] && echo "$RESPONSE"
exit 0
