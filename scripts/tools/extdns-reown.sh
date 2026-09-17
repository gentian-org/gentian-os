#!/usr/bin/env bash
# Repair external-dns ownership records: owner-id changes, the apex, and the
# registry prefix they both depend on.
#
# WHY THIS EXISTS
#
# external-dns records ownership in a TXT record beside each managed record, and
# in appendTakenDNSNameChanges() it "only add[s] creates if the external dns has
# ownership claim on the domain". Once a NAME carries a record this owner does
# not own, every create at that name is dropped in silence, and the log still
# says "All records are already up to date". Two things produced such names:
#
# 1. txtOwnerId changed from the product name to the cluster name, and nothing
#    rewrote the ownership records written under the old id.
#
# 2. The zone apex could never be owned at all. The marker name is built by
#    registry/mapper ToTXTName(), which without a record-type template glues
#    "<type>-" onto the FIRST LABEL of the name:
#
#        prefix "_extdns."  +  apex "example.com"  ->  "_extdns.a-example.com"
#
#    That is a name under a-example.com — a different domain, outside the zone.
#    Nothing can write it, so no apex record is ever owned, so SPF, MX and the
#    apex A itself are skipped forever. Subdomains are unaffected because they
#    have a label of their own to glue onto. With "%{record_type}" in the prefix
#    the type moves into the prefix and the name stays intact:
#
#        prefix "_extdns-%{record_type}."  ->  "_extdns-a.example.com"
#
# Subcommands, all idempotent and all a DRY RUN unless DRY_RUN=0:
#
#   reown            owner=<OLD_OWNER> -> owner=<NEW_OWNER> in every registry TXT
#   migrate-prefix   write each old-format marker again under TXT_PREFIX, so
#                    ownership survives switching external-dns to the new prefix
#   adopt T N R      create the marker that gives NEW_OWNER record T at name N,
#                    attributed to source R — for a record created outside
#                    external-dns that it is supposed to manage
#   prune-malformed  delete markers whose name repeats the zone, which is what a
#                    marker for an apex written under the old prefix turns into
#   cleanup-old      delete old-format markers once external-dns reads the new
#                    prefix and no longer consults them
#
# ORDER for an existing cluster: reown, migrate-prefix, THEN deploy the new
# txtPrefix, then adopt what external-dns did not create, then cleanup-old.
# Deploying the prefix before migrate-prefix leaves external-dns reading no
# ownership at all for a while — nothing is deleted, but nothing it manages is
# updated either.
#
# Re-owning deletes nothing by itself, but it grants external-dns authority to
# delete records no source asks for any more. Read the dry-run list first.
set -euo pipefail

ZONE_NAME="${ZONE_NAME:-}"
OLD_OWNER="${OLD_OWNER:-}"
NEW_OWNER="${NEW_OWNER:-}"
DRY_RUN="${DRY_RUN:-1}"
# The prefix external-dns is (or will be) configured with, and the one it used
# before. Must match kernel/values/external-dns.yaml.
TXT_PREFIX="${TXT_PREFIX:-_extdns-%{record_type\}.}"
OLD_TXT_PREFIX="${OLD_TXT_PREFIX:-_extdns.}"
# Where the token lives when it is not already in the environment: the Secret
# external-dns itself reads, so this needs no second credential.
TOKEN_NS="${TOKEN_NS:-external-dns}"
TOKEN_SECRET="${TOKEN_SECRET:-cloudflare-api-token}"
TOKEN_KEY="${TOKEN_KEY:-cloudflare_api_token}"

API=https://api.cloudflare.com/client/v4
RECORD_TEMPLATE='%{record_type}'
# The types extractRecordTypeDefaultPosition() recognises, so an old-format name
# is split where external-dns would split it.
KNOWN_TYPES=" a aaaa cname txt mx ns srv caa ptr naptr "

die() {
	echo "ERROR: $*" >&2
	exit 1
}

usage() {
	sed -n '/^# Subcommands/,/^# ORDER/p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 64
}

require() {
	local var
	for var in "$@"; do
		if [ -z "${!var:-}" ]; then
			echo "ERROR: $var is required" >&2
			usage
		fi
	done
}

dry_run_note() {
	if [ "$DRY_RUN" != "0" ]; then
		echo "DRY_RUN: nothing was written (set DRY_RUN=0 to apply)"
	fi
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
	local args=(-sS -X "$method" -H "Authorization: Bearer $CF_API_TOKEN")
	if [ -n "$data" ]; then
		args+=(-H 'Content-Type: application/json' --data "$data")
	fi
	curl "${args[@]}" "$API$path"
}

ok() {
	[ "$(printf '%s' "$1" | jq -r '.success')" = "true" ]
}

zone_id() {
	local id
	id=$(cf GET "/zones?name=$ZONE_NAME" | jq -r '.result[0].id // empty')
	[ -n "$id" ] || die "no zone id for $ZONE_NAME (is the token scoped to it?)"
	printf '%s' "$id"
}

lower() {
	printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# A port of registry/mapper AffixNameMapper.ToTXTName for a prefix-only mapper,
# so the names written here are the names external-dns looks up.
marker_name() {
	local prefix="$1" type name="$3" first rest
	type=$(lower "$2")
	first=${name%%.*}
	if [[ "$prefix" == *"$RECORD_TEMPLATE"* ]]; then
		prefix=${prefix//"$RECORD_TEMPLATE"/$type}
	else
		first="$type-$first"
	fi
	if [ "$first" = "$name" ] || [[ "$name" != *.* ]]; then
		printf '%s%s' "$prefix" "$first"
		return
	fi
	rest=${name#*.}
	printf '%s%s.%s' "$prefix" "$first" "$rest"
}

# Every registry TXT in the zone, as tab-separated id, name, content.
list_markers() {
	local zid="$1" resp
	# per_page is explicit: the default page size silently truncates a larger
	# zone, and a partial migration is exactly the failure this prevents.
	resp=$(cf GET "/zones/$zid/dns_records?type=TXT&per_page=500")
	ok "$resp" || die "listing TXT records failed: $(printf '%s' "$resp" | jq -c '.errors')"
	printf '%s' "$resp" | jq -r '.result[]
		| select(.content | contains("heritage=external-dns"))
		| [.id, .name, .content] | @tsv'
}

marker_exists() {
	local zid="$1" name="$2" resp
	resp=$(cf GET "/zones/$zid/dns_records?type=TXT&name=$name")
	ok "$resp" || die "looking up $name failed: $(printf '%s' "$resp" | jq -c '.errors')"
	[ "$(printf '%s' "$resp" | jq -r '.result | length')" != "0" ]
}

create_marker() {
	local zid="$1" name="$2" content="$3" resp
	resp=$(cf POST "/zones/$zid/dns_records" \
		"$(jq -n --arg n "$name" --arg c "$content" '{type: "TXT", name: $n, content: $c, ttl: 300}')")
	ok "$resp" || die "creating $name failed: $(printf '%s' "$resp" | jq -c '.errors')"
}

delete_marker() {
	local zid="$1" id="$2" name="$3" resp
	resp=$(cf DELETE "/zones/$zid/dns_records/$id")
	ok "$resp" || die "deleting $name failed: $(printf '%s' "$resp" | jq -c '.errors')"
}

# A marker for an apex record written under a prefix without the record-type
# template names a host outside the zone, and Cloudflare stores such a name
# relative to the zone — so it comes back with the zone appended twice.
is_malformed() {
	# No dot required before the first copy: the type is glued onto it with a
	# hyphen, so the stored name reads _extdns.a-example.com.example.com.
	[[ "$1" == *"$ZONE_NAME.$ZONE_NAME" ]]
}

do_reown() {
	require ZONE_NAME OLD_OWNER NEW_OWNER
	local zid id name content updated total=0 changed=0
	zid=$(zone_id)
	while IFS=$'\t' read -r id name content; do
		[ -n "$id" ] || continue
		total=$((total + 1))
		updated=${content//external-dns\/owner=$OLD_OWNER,/external-dns\/owner=$NEW_OWNER,}
		[ "$updated" != "$content" ] || continue
		changed=$((changed + 1))
		echo "reown  $name"
		if [ "$DRY_RUN" = "0" ]; then
			local resp
			resp=$(cf PATCH "/zones/$zid/dns_records/$id" "$(jq -n --arg c "$updated" '{content: $c}')")
			ok "$resp" || die "updating $name failed: $(printf '%s' "$resp" | jq -c '.errors')"
		fi
	done < <(list_markers "$zid")
	echo "registry records: $total   owner=$OLD_OWNER: $changed"
	dry_run_note
}

do_migrate_prefix() {
	require ZONE_NAME
	[ "$TXT_PREFIX" != "$OLD_TXT_PREFIX" ] || die "TXT_PREFIX and OLD_TXT_PREFIX are the same"
	local zid id name content rest type endpoint target created=0 present=0 skipped=0
	zid=$(zone_id)
	while IFS=$'\t' read -r id name content; do
		[ -n "$id" ] || continue
		[[ "$name" == "$OLD_TXT_PREFIX"* ]] || continue
		if is_malformed "$name"; then
			echo "skip   $name (malformed; see prune-malformed)"
			skipped=$((skipped + 1))
			continue
		fi
		rest=${name#"$OLD_TXT_PREFIX"}
		type=${rest%%-*}
		if [[ "$KNOWN_TYPES" != *" $type "* ]] || [ "$type" = "$rest" ]; then
			echo "skip   $name (no record type in the name)"
			skipped=$((skipped + 1))
			continue
		fi
		endpoint=${rest#"$type"-}
		target=$(marker_name "$TXT_PREFIX" "$type" "$endpoint")
		if marker_exists "$zid" "$target"; then
			present=$((present + 1))
			continue
		fi
		echo "copy   $name -> $target"
		created=$((created + 1))
		if [ "$DRY_RUN" = "0" ]; then
			create_marker "$zid" "$target" "$content"
		fi
	done < <(list_markers "$zid")
	echo "to create: $created   already present: $present   skipped: $skipped"
	dry_run_note
}

do_adopt() {
	require ZONE_NAME NEW_OWNER
	local type="${1:-}" name="${2:-}" resource="${3:-}" zid target
	if [ -z "$type" ] || [ -z "$name" ] || [ -z "$resource" ]; then
		usage
	fi
	target=$(marker_name "$TXT_PREFIX" "$type" "$name")
	# A marker outside the zone cannot be written, and trying produces the
	# malformed record prune-malformed exists to remove.
	if [ "$target" != "$ZONE_NAME" ] && [[ "$target" != *".$ZONE_NAME" ]]; then
		die "$target is outside $ZONE_NAME — TXT_PREFIX needs $RECORD_TEMPLATE to own the apex"
	fi
	zid=$(zone_id)
	if marker_exists "$zid" "$target"; then
		echo "adopt  $target already exists — nothing to do"
		return
	fi
	echo "adopt  $target -> owner=$NEW_OWNER resource=$resource"
	if [ "$DRY_RUN" = "0" ]; then
		create_marker "$zid" "$target" \
			"heritage=external-dns,external-dns/owner=$NEW_OWNER,external-dns/resource=$resource"
	fi
	dry_run_note
}

do_prune_malformed() {
	require ZONE_NAME
	local zid id name content removed=0
	zid=$(zone_id)
	while IFS=$'\t' read -r id name content; do
		[ -n "$id" ] || continue
		is_malformed "$name" || continue
		echo "delete $name"
		removed=$((removed + 1))
		if [ "$DRY_RUN" = "0" ]; then
			delete_marker "$zid" "$id" "$name"
		fi
	done < <(list_markers "$zid")
	echo "malformed: $removed"
	dry_run_note
}

do_cleanup_old() {
	require ZONE_NAME
	local zid id name content removed=0
	zid=$(zone_id)
	while IFS=$'\t' read -r id name content; do
		[ -n "$id" ] || continue
		[[ "$name" == "$OLD_TXT_PREFIX"* ]] || continue
		echo "delete $name"
		removed=$((removed + 1))
		if [ "$DRY_RUN" = "0" ]; then
			delete_marker "$zid" "$id" "$name"
		fi
	done < <(list_markers "$zid")
	echo "old-format markers: $removed"
	dry_run_note
}

main() {
	command -v jq >/dev/null 2>&1 || die "jq is required"
	command -v curl >/dev/null 2>&1 || die "curl is required"
	local cmd="${1:-}"
	shift || true
	case "$cmd" in
	reown | migrate-prefix | adopt | prune-malformed | cleanup-old) require_token ;;
	*) usage ;;
	esac
	case "$cmd" in
	reown) do_reown ;;
	migrate-prefix) do_migrate_prefix ;;
	adopt) do_adopt "$@" ;;
	prune-malformed) do_prune_malformed ;;
	cleanup-old) do_cleanup_old ;;
	esac
}

main "$@"
