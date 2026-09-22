#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
runtime_dir="$script_dir/.runtime"
umask 077
mkdir -p "$runtime_dir"

openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 2 \
  -subj "/CN=SAOAF test trust domain" \
  -keyout "$runtime_dir/ca-key.pem" -out "$runtime_dir/ca.pem"

openssl req -newkey rsa:3072 -sha256 -nodes \
  -subj "/CN=resource-resolver" \
  -addext "subjectAltName=URI:spiffe://saoaf.test/ns/default/sa/resource-resolver" \
  -keyout "$runtime_dir/workload-key.pem" -out "$runtime_dir/workload.csr"

cat >"$runtime_dir/extensions.cnf" <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth,serverAuth
subjectAltName=URI:spiffe://saoaf.test/ns/default/sa/resource-resolver
EOF

openssl x509 -req -sha256 -days 1 \
  -in "$runtime_dir/workload.csr" \
  -CA "$runtime_dir/ca.pem" -CAkey "$runtime_dir/ca-key.pem" -CAcreateserial \
  -extfile "$runtime_dir/extensions.cnf" \
  -out "$runtime_dir/workload.pem"

openssl verify -CAfile "$runtime_dir/ca.pem" "$runtime_dir/workload.pem"
openssl x509 -in "$runtime_dir/workload.pem" -noout -subject -issuer -dates
openssl x509 -in "$runtime_dir/workload.pem" -noout -text | grep -A 1 "Subject Alternative Name"
