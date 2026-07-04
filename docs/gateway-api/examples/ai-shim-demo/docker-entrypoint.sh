#!/bin/sh
# Render nginx.conf from env (openresty-alpine has no envsubst) and exec.
set -e
: "${RESOLVER:=127.0.0.11}"
: "${MODEL_BACKEND:=mock:8000}"
: "${CACHE_BACKEND:=cache-service:9200}"
: "${CLASSIFIER_BACKEND:=127.0.0.1:9999}"

sed -e "s|\${RESOLVER}|${RESOLVER}|g" \
    -e "s|\${MODEL_BACKEND}|${MODEL_BACKEND}|g" \
    -e "s|\${CACHE_BACKEND}|${CACHE_BACKEND}|g" \
    -e "s|\${CLASSIFIER_BACKEND}|${CLASSIFIER_BACKEND}|g" \
    /conf/nginx.conf > /tmp/nginx.conf

exec /usr/local/openresty/bin/openresty \
    -c /tmp/nginx.conf -p /usr/local/openresty/nginx -g 'daemon off;'
