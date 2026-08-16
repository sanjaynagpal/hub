#!/usr/bin/env bash
# Generates a self-signed cert for local/testing use, dropped in the same
# location and filenames renew-cert.sh would use -- so mavenrepo.conf works
# unmodified whether the cert came from the internal CA or from here.
#
# NOT for production: nothing trusts this cert. Clients will need -k/--insecure
# (curl), <server><configuration><httpConfiguration><param>...</param> insecure
# TLS settings or a trust-store import (mvn), or a manual "always trust" click
# (browsers) to talk to it.
#
# Usage: ./gen-self-signed-cert.sh [domain] [days]
#   domain defaults to "localhost", days defaults to 365.
set -euo pipefail

DOMAIN="${1:-localhost}"
DAYS="${2:-365}"
CERT_DIR="/etc/nginx/certs/$DOMAIN"

mkdir -p "$CERT_DIR"
umask 077

openssl req -x509 -nodes -newkey rsa:2048 -days "$DAYS" \
    -keyout "$CERT_DIR/privkey.pem" \
    -out "$CERT_DIR/fullchain.pem" \
    -subj "/CN=$DOMAIN" \
    -addext "subjectAltName=DNS:$DOMAIN,DNS:localhost,IP:127.0.0.1"

chmod 600 "$CERT_DIR/privkey.pem"

echo "Self-signed cert for '$DOMAIN' written to $CERT_DIR (valid $DAYS days)."
echo "Run: nginx -t && systemctl reload nginx  # (or just start nginx) to pick it up."
echo "This cert is untrusted by design -- clients need -k/--insecure or an explicit trust override."
