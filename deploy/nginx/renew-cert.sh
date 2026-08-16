#!/usr/bin/env bash
# Requests/renews the TLS cert for the mavenrepo nginx vhost from the internal
# CA's REST API, then reloads nginx.
#
# Generates a fresh private key + CSR locally on every run (so the private key
# never leaves this host -- only the CSR is sent to the CA) and rotates the
# key on each renewal. If your CA prefers a stable key across renewals, move
# the `openssl req` key generation outside of this script's renewal path.
#
# TODO once the CA's actual API contract is known, fix:
#   - CA_ENDPOINT, HTTP method, and how the caller authenticates
#   - request body shape (this assumes {"csr": "<PEM>", "common_name": "..."})
#   - response field names for the signed cert / chain (this assumes
#     {"certificate": "<PEM>", "ca_chain": "<PEM>"})
#   - whether the CA wants the CSR as raw PEM, base64, or DER
#
# DOMAIN / CA_ENDPOINT / AUTH_TOKEN_FILE can all be overridden via environment
# (or DOMAIN via the first positional arg) so this script stays identical
# across hosts in a fleet -- per-host values come from the caller (a systemd
# Environment= line, an Ansible-templated unit, etc.) instead of hand-edits.
set -euo pipefail

DOMAIN="${1:-${DOMAIN:-maven.internal.example.com}}"
CERT_DIR="/etc/nginx/certs/$DOMAIN"
CA_ENDPOINT="${CA_ENDPOINT:-https://ca.internal.example.com/api/v1/sign-csr}"
AUTH_TOKEN_FILE="${AUTH_TOKEN_FILE:-/etc/nginx/certs/ca-api-token}"

mkdir -p "$CERT_DIR"
umask 077

# 1. Generate a fresh key + CSR.
openssl req -new -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/privkey.pem.new" \
    -out "$CERT_DIR/req.csr" \
    -subj "/CN=$DOMAIN"

# 2. Submit the CSR to the CA's REST API.
response=$(curl -sf -X POST "$CA_ENDPOINT" \
    -H "Authorization: Bearer $(cat "$AUTH_TOKEN_FILE")" \
    -H "Content-Type: application/json" \
    -d "{\"csr\": $(jq -Rs . < "$CERT_DIR/req.csr"), \"common_name\": \"$DOMAIN\"}")

echo "$response" | jq -r '.certificate' > "$CERT_DIR/cert.pem.new"
echo "$response" | jq -r '.ca_chain'    > "$CERT_DIR/chain.pem.new"
cat "$CERT_DIR/cert.pem.new" "$CERT_DIR/chain.pem.new" > "$CERT_DIR/fullchain.pem.new"

# 3. Atomic swap into place, then reload nginx.
mv "$CERT_DIR/privkey.pem.new"   "$CERT_DIR/privkey.pem"
mv "$CERT_DIR/fullchain.pem.new" "$CERT_DIR/fullchain.pem"
rm -f "$CERT_DIR/cert.pem.new" "$CERT_DIR/chain.pem.new" "$CERT_DIR/req.csr"
chmod 600 "$CERT_DIR/privkey.pem"

nginx -t && systemctl reload nginx
