#!/bin/bash

# Setup needed configuration. 
# Fill in missing sensitive parts before executing.

export PORT="8080"
export OAUTH_REDIRECT_URI="http://localhost:8080/callback"

# Replace these with your actual values:
export GOOGLE_API_KEY="PASTE_YOUR_API_KEY_HERE"
export OAUTH_CLIENT_ID="PASTE_YOUR_CLIENT_ID_HERE"
export OAUTH_CLIENT_SECRET="PASTE_YOUR_CLIENT_SECRET_HERE"

# Defaults mapped from specification
export OAUTH_AUTHORIZE_URL="https://34.54.8.132.nip.io/v1/oauth20/authorize"
export OAUTH_TOKEN_URL="https://34.54.8.132.nip.io/v1/oauth20/token"
export LEGO_MCP_ENDPOINT="https://34.54.8.132.nip.io/mcp/lego"
export ROBOT_MCP_ENDPOINT="https://34.54.8.132.nip.io/mcp/robot"

echo "Compiling Binary..."
go build -o demo-agent ./cmd/demo

if [ $? -eq 0 ]; then
    echo "--------------------------------------------------"
    echo "Starting Local Agent Server on http://localhost:$PORT"
    echo "--------------------------------------------------"
    ./demo-agent
else
    echo "Build failed!"
fi
