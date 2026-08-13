#!/bin/bash
set -e

SSH_KEY="$HOME/.ssh/id_ed25519_rentmanager"
SERVER="root@45.11.92.171"
REMOTE_DIR="/opt/rentmanager"

# === Version from git (client repo + server repo) ===
CLIENT_REPO="/c/Users/dench/AndroidStudioProjects/Rentmanager"
CLIENT_COMMITS=$(git -C "$CLIENT_REPO" rev-list --count HEAD 2>/dev/null || echo 0)
VERSION_CODE=$CLIENT_COMMITS
# Fallback if 0 (should never happen)
if [ "$VERSION_CODE" -eq 0 ]; then VERSION_CODE=1; fi
VERSION_NAME="1.0.$VERSION_CODE"

# === Configurable settings ===
MIN_CLIENT_VERSION=1
FORCE_UPDATE=false
APK_FILE="app-release.apk"

# === Read release notes ===
if [ -f "release_notes.txt" ]; then
    RELEASE_NOTES=$(cat release_notes.txt | tr '\n' ' ' | sed 's/"/\\"/g')
else
    RELEASE_NOTES=""
fi

echo "=== Version: $VERSION_NAME (code=$VERSION_CODE) ==="

echo "=== Building for Linux ==="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o server_linux ./cmd/server
echo "Build done."

echo "=== Uploading to VPS ==="
scp -i "$SSH_KEY" server_linux "$SERVER:$REMOTE_DIR/server_new"
scp -i "$SSH_KEY" docker-compose.yml "$SERVER:$REMOTE_DIR/"
scp -i "$SSH_KEY" .env "$SERVER:$REMOTE_DIR/"
echo "Upload done."

echo "=== Ensuring downloads directory ==="
ssh -i "$SSH_KEY" "$SERVER" "mkdir -p $REMOTE_DIR/downloads"

# Check if client version is newer than server version
SERVER_VERSION=$(curl -s "http://45.11.92.171:8080/api/v1/version" 2>/dev/null | grep -o '"version_code":[0-9]*' | grep -o '[0-9]*')
SERVER_VERSION=${SERVER_VERSION:-0}
APK_PATH="$CLIENT_REPO/app/build/outputs/apk/release/$APK_FILE"

if [ "$VERSION_CODE" -gt "$SERVER_VERSION" ] && [ -f "$APK_PATH" ]; then
    echo "=== Client version $VERSION_CODE > server version $SERVER_VERSION — full deploy ==="

    echo "=== Generating version.json ==="
    cat > /tmp/version.json <<EOF
{
  "version_code": $VERSION_CODE,
  "version_name": "$VERSION_NAME",
  "min_client_version": $MIN_CLIENT_VERSION,
  "apk_url": "http://45.11.92.171:8080/downloads/$APK_FILE",
  "force_update": $FORCE_UPDATE,
  "release_notes": "$RELEASE_NOTES"
}
EOF

    echo "=== Uploading version.json ==="
    scp -i "$SSH_KEY" /tmp/version.json "$SERVER:$REMOTE_DIR/downloads/version.json"
    rm -f /tmp/version.json

    echo "=== Uploading APK ==="
    scp -i "$SSH_KEY" "$APK_PATH" "$SERVER:$REMOTE_DIR/downloads/$APK_FILE"
    echo "APK uploaded."
else
    echo "=== Client version $VERSION_CODE <= server version $SERVER_VERSION — server-only deploy ==="
    echo "=== Skipping APK and version.json ==="
fi

echo "=== Ensuring Docker containers are running ==="
ssh -i "$SSH_KEY" "$SERVER" "cd $REMOTE_DIR && docker compose up -d postgres redis"
echo "Waiting for containers to be healthy..."
sleep 5

echo "=== Killing old server ==="
ssh -i "$SSH_KEY" "$SERVER" "pkill -f './server' 2>/dev/null; sleep 1; true"

echo "=== Starting new server ==="
ssh -i "$SSH_KEY" -f "$SERVER" "cd $REMOTE_DIR && mv -f server_new server && chmod +x server && nohup ./server > server.log 2>&1 < /dev/null &"
sleep 2

echo "=== Checking server is up ==="
ssh -i "$SSH_KEY" "$SERVER" "ps aux | grep './server' | grep -v grep > /dev/null && echo 'OK: server process running' || echo 'WARN: no server process'; ss -tlnp | grep 8080 > /dev/null && echo 'OK: port 8080 listening' || echo 'WARN: port 8080 not listening'"
echo "=== Deploy complete! Server is at http://45.11.92.171:8080 ==="
echo "=== Version info at http://45.11.92.171:8080/api/v1/version ==="