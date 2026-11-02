#!/bin/sh
# Install as: /etc/letsencrypt/renewal-hooks/deploy/clever-campus-copy.sh  (chmod 755)
# Runs after every successful renewal: copies the new wildcard cert into the
# folder Clever Campus's nginx mounts, then reloads nginx (no downtime).
set -eu
LINEAGE="${RENEWED_LINEAGE:-/etc/letsencrypt/live/clevercampus.cloud-0001}"
DEST=/root/school-erp-saas/deploy/nginx/ssl

case "$LINEAGE" in
  */clevercampus.cloud*) ;;
  *) exit 0 ;;  # some other certificate
esac

install -m 644 "$LINEAGE/fullchain.pem" "$DEST/fullchain.pem.new"
install -m 600 "$LINEAGE/privkey.pem"   "$DEST/privkey.pem.new"
mv "$DEST/fullchain.pem.new" "$DEST/fullchain.pem"
mv "$DEST/privkey.pem.new"   "$DEST/privkey.pem"

docker exec clever-campus-nginx-1 nginx -t && docker exec clever-campus-nginx-1 nginx -s reload
