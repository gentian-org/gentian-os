# Tools image for Crossplane app init Jobs (ldap/db/s3). Built at CI time so
# tenant namespaces never need outbound access to Alpine package mirrors at runtime.
FROM alpine:3.20
RUN apk add --no-cache curl jq openssl minio-client postgresql-client \
 && ln -sf /usr/bin/mcli /usr/bin/mc
