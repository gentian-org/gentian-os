#!/usr/bin/env bash
# Re-own external-dns registry records after txtOwnerId changed, and adopt the
# records external-dns wants to manage but did not create.
#
# WHY THIS EXISTS
#
# external-dns records ownership in a TXT record beside each managed record
# (_extdns.<type>-<name>, "heritage=external-dns,external-dns/owner=<id>,...").
# Changing txtOwnerId does NOT rewrite those, and the mismatch is not reported
# anywhere: in appendTakenDNSNameChanges() external-dns "only add[s] creates if
# the external dns has ownership claim on the domain", so once a NAME carries a
# record it does not own, every create at that name is skipped in silence. The
# log still says "All records are already up to date".
#
# That is how the kernel domain ended up with a DKIM record and no SPF and no MX.
# gentian.cloud already had an unowned A record — created before external-dns, so
# with no ownership TXT at all — and the SPF, MX and the apex A that the
# HTTPRoute asks for were dropped on every cycle for as long as anyone looked.
# _dmarc.gentian.cloud published fine, because that name was free.
#
# Two operations, both idempotent:
#
#   reown  rewrite owner=<old> to owner=<new> in existing registry TXTs
#   adopt  create a registry TXT for a record external-dns wants but does not own
#
# Re-owning does not delete anything by itself, but it does hand external-dns
# authority to delete: with --policy=sync it removes records it owns that no
# source asks for any more. Run with DRY_RUN=1 first and read the list.
set -euo pipefail

ZONE_NAME="${ZONE_NAME:-}"
OLD_OWNER="${OLD_OWNER:-}"
NEW_OWNER="${NEW_OWNER:-}"
DRY_RUN="${DRY_RUN:-1}"
# Where the token lives when it is not already in the environment. The same
# Secret external-dns itself reads, so this needs no second credential.
TOKEN_NS="${TOKEN_NS:-external-dns}"
TOKEN_SECRET="${TOKEN_SECRET:-cloudflare-api-token}"
TOKEN_KEY="${TOKEN_KEY:-cloudflare_api_token}"

API=https://api.cloudflare.com/client/v4

die() {
	echo "ERROR: $*" >&2
	exit 1
}

usage() {
	cat >&2 <<'EOF'
usage:
  ZONE_NAME=example.com OLD_OWNER=old NEW_OWNER=new [DRY_RUN=0] extdns-reown.sh reown
  ZONE_NAME=example.com NEW_OWNER=new extdns-reown.sh adopt <type> <name> <resource>

  reown   rewrite every _extdns.* registry TXT in the zone from OLD_OWNER to NEW_OWNER
  adopt   create the registry TXT that makes NEW_OWNER the owner of one record,
          so external-dns may add other record types at that name

  <type>      record type of the record being adopted, e.g. A
  <name>      the record's name, e.g. example.com
  <resource>  the source external-dns would attribute it to, e.g.
              httproute/platform-kernel/kernel-apex-redirect

  DRY_RUN=1 (the default) prints what would change and writes nothing.
EOF
	exit 64
}

require_token() {
	if [ -n "${CF_API_TOKEN:-}" ]; then
		return
	fi
	command -v kubectl >/dev/null 2>&1 ||
		die "CF_API_TOKEN is unset and kubectl is not available to read it"
	CF_API_TOKEN=$(kubectl -n "$TOKEN_NS" get secret "$TOKEN_SECRET" \
		-o "go-template={{index .data \"$TOKEN_KEY\" | base64decode}}") ||
		die "could not read $TOKEN_NS/$TOKEN_SECRET"
	[ -n "$CF_API_TOKEN" ] || die "$TOKEN_NS/$TOKEN_SECRET/$TOKEN_KEY is empty"
	export CF_API_TOKEN
}

cf() {
	local method="$1" path="$2" data="${3:-}"
	if [ -n "$data" ]; then
		curl -sS -X "$method" \
			-H "Authorization: Bearer $CF_API_TOKEN" \
			-H 'Content-Type: application/json' \
			--data "$data" "$API$path"
	else
		curl -sS -X "$method" \
			-H "Authorization: Bearer $CF_API_TOKEN" "$API$path"
	fi
}

zone_id() {
	local id
	id=$(cf GET "/zones?name=$ZONE_NAME" | jq -r '.result[0].id // empty')
	[ -n "$id" ] || die "no zone id for $ZONE_NAME (is the token scoped to it?)"
	printf '%s' "$id"
}

# The marker name external-dns uses for one record, matching --txt-prefix and the
# per-type naming it writes: _extdns.<lowercased type>-<name>.
marker_name() {
	local type="$1" name="$2"
	printf '_extdns.%s-%s' "$(printf '%s' "$type" | tr '[:upper:]' '[:lower:]')" "$name"
}

do_reown() {
	[ -n "$ZONE_NAME" ] && [ -n "$OLD_OWNER" ] && [ -n "$NEW_OWNER" ] || usage
	local zid records count=0 changed=0
	zid=$(zone_id)
	# per_page is explicit: the default page size silently truncates a zone with
	# more records than it, and a partial migration is the failure this script
	# exists to prevent.
	records=$(cf GET "/zones/$zid/dns_records?type=TXT&per_page=500")
	[ "$(printf '%s' "$records" | jq -r '.success')" = "true" ] ||
		die "listing TXT records failed: $(printf '%s' "$records" | jq -c '.errors')"

	while IFS=$'\t' read -r id name content; do
		[ -n "$id" ] || continue
		count=$((count + 1))
		local updated
		updated=${content//external-dns\/owner=$OLD_OWNER/external-dns\/owner=$NEW_OWNER}
		if [ "$updated" = "$content" ]; then
			continue
		fi
		changed=$((changed + 1))
		echo "reown  $name"
		if [ "$DRY_RUN" != "0" ]; then
			continue
		fi
		local resp
		resp=$(cf PATCH "/zones/$zid/dns_records/$id" \
			"$(jq -n --arg c "$updated" '{content: $c}')")
		[ "$(printf '%s' "$resp" | jq -r '.success')" = "true" ] ||
			die "updating $name failed: $(printf '%s' "$resp" | jq -c '.errors')"
	done < <(printf '%s' "$records" |
		jq -r '.result[] | select(.name | startswith("_extdns.")) | [.id, .name, .content] | @tsv')

	echo "registry records: $count   owner=$OLD_OWNER: $changed"
	[ "$DRY_RUN" = "0" ] || echo "DRY_RUN: nothing was written (set DRY_RUN=0 to apply)"
}

do_adopt() {
	local type="${1:-}" name="${2:-}" resource="${3:-}"
	[ -n "$ZONE_NAME" ] && [ -n "$NEW_OWNER" ] || usage
	[ -n "$type" ] && [ -n "$name" ] && [ -n "$resource" ] || usage
	local zid marker existing value resp
	zid=$(zone_id)
	marker=$(marker_name "$type" "$name")
	existing=$(cf GET "/zones/$zid/dns_records?type=TXT&name=$marker")
	if [ "$(printf '%s' "$existing" | jq -r '.result | length')" != "0" ]; then
		echo "adopt  $marker already exists — nothing to do"
		return
	fi
	value="heritage=external-dns,external-dns/owner=$NEW_OWNER,external-dns/resource=$resource"
	echo "adopt  $marker -> $value"
	if [ "$DRY_RUN" != "0" ]; then
		echo "DRY_RUN: nothing was written (set DRY_RUN=0 to apply)"
		return
	fi
	resp=$(cf POST "/zones/$zid/dns_records" \
		"$(jq -n --arg n "$marker" --arg c "$value" \
			'{type: "TXT", name: $n, content: $c, ttl: 300}')")
	[ "$(printf '%s' "$resp" | jq -r '.success')" = "true" ] ||
		die "creating $marker failed: $(printf '%s' "$resp" | jq -c '.errors')"
	echo "created"
}

main() {
	command -v jq >/dev/null 2>&1 || die "jq is required"
	command -v curl >/dev/null 2>&1 || die "curl is required"
	local cmd="${1:-}"
	shift || true
	case "$cmd" in
	reown)
		require_token
		do_reown
		;;
	adopt)
		require_token
		do_adopt "$@"
		;;
	*) usage ;;
	esac
}

main "$@"
