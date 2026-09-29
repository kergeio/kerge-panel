#!/bin/bash
# Installs, upgrades or reconfigures the Kerge panel with Docker Compose.
#
#   curl -fsSL https://get.kerge.io | sudo bash
#   curl -fsSL https://get.kerge.io | sudo bash -s -- --domain panel.example.com
#
# Every file it downloads is checked before anything is written: the
# release's checksums.txt carries an OpenSSH signature made with the
# release key, and each file must match its checksum. The whole script is
# made of functions and only the last line runs anything, so a download cut
# short does nothing.
#
# VERSION is written in when the release workflow publishes this script, so
# the script from release vX installs vX unless --version says otherwise;
# the script in the repository carries no version and installs nothing
# without --version. BASE_URL, the Cloudflare URLs and SELFCHECK_SECONDS
# are rewritten only by the tests.
set -euo pipefail

VERSION="dev"
BASE_URL="https://github.com/kergeio/kerge-panel/releases/download"
RELEASE_PUBKEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMMWMoTwuKCmpyWran5GZp5KiZuOIt6N/vzcfZsrdfpH kerge-release"
CF_IPS_V4_URL="https://www.cloudflare.com/ips-v4"
CF_IPS_V6_URL="https://www.cloudflare.com/ips-v6"
TRACE_URL="https://www.cloudflare.com/cdn-cgi/trace"
SELFCHECK_SECONDS=180

readonly SIGNER="releases@kerge.io"
readonly NAMESPACE="kerge-release"
readonly PANEL_CONTAINER="kerge"
readonly CADDY_CONTAINER="kerge-caddy"
readonly PANEL_UID="65532"
readonly COMPOSE_INTEGRATED="compose.yml"
readonly COMPOSE_EXTERNAL="compose.external-proxy.yml"
# What the agent sends: it uses the Go HTTP client's default.
readonly AGENT_USER_AGENT="Go-http-client/1.1"
readonly AGENT_PROTOCOL="kerge.v1"
# A Cloudflare list outside these bounds is treated as a failed download.
# Cloudflare's widest ranges are far narrower (/13 and /29 in 2026).
readonly MIN_PREFIX_V4=10
readonly MIN_PREFIX_V6=24
readonly MAX_RANGES=100

opt_domain=""
opt_version=""
opt_cloudflare=""
opt_external=""
dir="/opt/kerge"

has_tty=false
workdir=""
version=""
domain=""
env_exists=false
env_domain=""
env_cloudflare=""
env_external=""
cloudflare=""
external=""
compose_file=""
caddy_changed=false
cf_v4=()
cf_v6=()

main() {
	parse_args "$@"
	need_root
	require_tools
	detect_tty
	choose_version

	workdir="$(mktemp -d)"
	trap cleanup EXIT

	load_env
	choose_domain
	fetch_release_index
	fetch_cloudflare_ranges
	choose_mode
	preflight
	download_files

	install_files
	write_env
	start_panel
	show_setup_code
	self_check
}

info() { echo "kerge: $*"; }

warn() { echo "kerge: warning: $*" >&2; }

die() {
	echo "kerge: $*" >&2
	exit 1
}

cleanup() {
	if [ -n "$workdir" ]; then
		rm -rf "$workdir"
	fi
}

usage() {
	cat <<'USAGE'
Usage: install.sh [options]

  --domain <name>         the panel's domain; asked for when not given
  --version <v>           install this release instead of the script's own
  --cloudflare            the domain is proxied by Cloudflare
  --no-cloudflare         the domain is not proxied by Cloudflare
  --external-proxy        use a reverse proxy already running on this host
  --no-external-proxy     run the panel's own Caddy on ports 80 and 443
  --dir <path>            installation directory (default /opt/kerge)

Run it again to upgrade or to switch modes; data and certificates are kept.
The mode options are remembered in .env, so a later run without them keeps
the current mode.
USAGE
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--domain | --version | --dir)
			[ $# -ge 2 ] || die "$1 needs a value"
			case "$1" in
			--domain) opt_domain="$2" ;;
			--version) opt_version="${2#v}" ;;
			--dir) dir="$2" ;;
			esac
			shift 2
			;;
		--cloudflare) opt_cloudflare=yes && shift ;;
		--no-cloudflare) opt_cloudflare=no && shift ;;
		--external-proxy) opt_external=yes && shift ;;
		--no-external-proxy) opt_external=no && shift ;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown option $1"
			;;
		esac
	done
	case "$dir" in
	/*) ;;
	*) die "--dir must be an absolute path, not $dir" ;;
	esac
}

need_root() {
	[ "$(id -u)" = "0" ] || die "run this as root, for example with sudo"
}

require_tools() {
	if ! command -v docker >/dev/null 2>&1; then
		die "Docker is not installed. Install it first (step 1 of the README):
  curl -fsSL https://get.docker.com | sudo sh
then run this again."
	fi
	docker compose version >/dev/null 2>&1 ||
		die "the Docker Compose plugin is missing. Install Docker with its Compose plugin (step 1 of the README):
  curl -fsSL https://get.docker.com | sudo sh"
	docker info >/dev/null 2>&1 || die "Docker is installed but not running; start it (systemctl start docker) and run this again"

	local missing=()
	local tool
	for tool in curl sha256sum getent install sed grep awk; do
		command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
	done
	[ ${#missing[@]} -eq 0 ] || die "these commands are missing: ${missing[*]}"

	# The signature check is mandatory, so a host that cannot verify one
	# does not get a panel.
	command -v ssh-keygen >/dev/null 2>&1 ||
		die "ssh-keygen is missing; install openssh-client (OpenSSH 8.1 or newer) and run this again"
	if ssh-keygen -Y verify 2>&1 | grep -qi 'unknown option'; then
		die "this ssh-keygen cannot verify signatures; install OpenSSH 8.1 or newer and run this again"
	fi
}

# detect_tty finds out whether questions can be asked. Piped into bash, the
# script's own input is the download, so questions go to the terminal.
detect_tty() {
	if (exec </dev/tty) 2>/dev/null; then
		has_tty=true
	fi
}

# ask prints a question on the terminal and reads the answer from it.
ask() {
	local prompt="$1" answer
	printf '%s' "$prompt" >/dev/tty
	IFS= read -r answer </dev/tty || answer=""
	printf '%s' "$answer"
}

# confirm asks a yes/no question; default is the answer to an empty reply.
confirm() {
	local prompt="$1" default="$2" answer
	answer="$(ask "$prompt")"
	answer="$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')"
	case "${answer:-$default}" in
	y | yes) return 0 ;;
	*) return 1 ;;
	esac
}

choose_version() {
	version="${opt_version:-$VERSION}"
	[ "$version" != "dev" ] ||
		die "this script carries no release version; run it with --version <v> or use the command in the README"
	[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] ||
		die "$version is not a version such as 1.2.3"
}

# load_env reads what an earlier run recorded. .env is parsed, never
# sourced.
load_env() {
	local file="$dir/.env"
	[ -f "$file" ] || return 0
	env_exists=true
	env_domain="$(env_value "$file" KERGE_DOMAIN)"
	env_cloudflare="$(env_value "$file" KERGE_CLOUDFLARE)"
	env_external="$(env_value "$file" KERGE_EXTERNAL_PROXY)"
	local key
	for key in env_cloudflare env_external; do
		case "${!key}" in
		"" | yes | no) ;;
		*) die "$file has ${key#env_} set to '${!key}'; use yes or no" ;;
		esac
	done
}

# env_value prints the last value a .env file gives a key.
env_value() {
	local file="$1" key="$2"
	sed -n "s/^[[:space:]]*$key=//p" "$file" | tail -n 1 | tr -d '\r' | sed 's/[[:space:]]*$//'
}

choose_domain() {
	if [ -n "$env_domain" ]; then
		domain="$env_domain"
		if [ -n "$opt_domain" ] && [ "$(lower "$opt_domain")" != "$domain" ]; then
			die "the panel in $dir runs on $domain; to move it to $opt_domain, change KERGE_DOMAIN in $dir/.env and run this again"
		fi
		return
	fi
	if [ -n "$opt_domain" ]; then
		domain="$(lower "$opt_domain")"
		valid_domain "$domain" || die "$opt_domain is not a domain name; the panel needs one, for example panel.example.com"
		return
	fi
	[ "$has_tty" = true ] || die "no terminal to ask for the domain on; pass it with --domain, for example:
  curl -fsSL https://get.kerge.io | sudo bash -s -- --domain panel.example.com"
	while :; do
		domain="$(lower "$(ask "Domain of the panel (for example panel.example.com): ")")"
		valid_domain "$domain" && return
		echo "That is not a domain name." >/dev/tty
	done
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# valid_domain accepts a DNS name with at least two labels. An IP address
# is not one: v1 needs a domain for its certificate.
valid_domain() {
	local name="$1"
	[ ${#name} -le 253 ] || return 1
	[[ $name =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || return 1
	[[ ! $name =~ ^[0-9.]+$ ]]
}

fetch_release_index() {
	local url="$BASE_URL/v$version" name
	for name in checksums.txt checksums.txt.sig; do
		curl -fsSL -o "$workdir/$name" "$url/$name" ||
			die "could not download $url/$name; is $version a released version?"
	done
	printf '%s namespaces="%s" %s\n' "$SIGNER" "$NAMESPACE" "$RELEASE_PUBKEY" >"$workdir/allowed_signers"
	ssh-keygen -Y verify -f "$workdir/allowed_signers" -I "$SIGNER" -n "$NAMESPACE" \
		-s "$workdir/checksums.txt.sig" <"$workdir/checksums.txt" >/dev/null 2>&1 ||
		die "the signature of checksums.txt does not match the release key; nothing was changed"
	info "the signature of release v$version is valid"
}

# download_verified fetches one release file into the work directory and
# checks it against the signed checksums.
download_verified() {
	local name="$1" url="$BASE_URL/v$version/$1"
	curl -fsSL -o "$workdir/$name" "$url" || die "could not download $url"
	grep -E "^[0-9a-f]{64} [ *]$name\$" "$workdir/checksums.txt" >"$workdir/$name.sum" ||
		die "checksums.txt has no entry for $name; nothing was changed"
	(cd "$workdir" && sha256sum -c --quiet "$name.sum" >/dev/null 2>&1) ||
		die "the checksum of $name does not match; nothing was changed"
}

# fetch_cloudflare_ranges gets the ranges Cloudflare publishes today. When
# that fails, or a list does not look like Cloudflare's, the snapshot in
# the release is used instead.
fetch_cloudflare_ranges() {
	if fetch_range_list "$CF_IPS_V4_URL" "$workdir/ips-v4" &&
		fetch_range_list "$CF_IPS_V6_URL" "$workdir/ips-v6" &&
		valid_range_list v4 "$workdir/ips-v4" &&
		valid_range_list v6 "$workdir/ips-v6"; then
		mapfile -t cf_v4 <"$workdir/ips-v4"
		mapfile -t cf_v6 <"$workdir/ips-v6"
		return
	fi
	download_verified cloudflare-ips.txt
	local snapshot="$workdir/cloudflare-ips.txt" taken
	mapfile -t cf_v4 < <(range_lines "$snapshot" | grep -v ':' || true)
	mapfile -t cf_v6 < <(range_lines "$snapshot" | grep ':' || true)
	taken="$(sed -n 's/.*taken on \([0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}\).*/\1/p' "$snapshot" | head -n 1)"
	warn "could not get Cloudflare's current IP ranges; using the snapshot in release v$version, taken on ${taken:-an unknown date}"
}

# fetch_range_list downloads one list and normalizes it to one entry per
# line.
fetch_range_list() {
	local url="$1" out="$2"
	curl -fsSL --max-time 15 -o "$out.raw" "$url" 2>/dev/null || return 1
	range_lines "$out.raw" >"$out"
}

# range_lines prints the entries of a range file: comments, blank lines and
# carriage returns removed.
range_lines() {
	sed -e 's/#.*//' -e 's/\r//g' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$1" | grep -v '^$' || true
}

# valid_range_list checks every entry of a v4 or v6 list and its length.
valid_range_list() {
	local family="$1" file="$2" count line
	count="$(wc -l <"$file")"
	[ "$count" -ge 1 ] && [ "$count" -le "$MAX_RANGES" ] || return 1
	while IFS= read -r line; do
		if [ "$family" = v4 ]; then
			valid_cidr4 "$line" || return 1
		else
			valid_cidr6 "$line" || return 1
		fi
	done <"$file"
}

# ipv4_int prints a dotted IPv4 address as an integer.
ipv4_int() {
	local a b c d
	[[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
	a=$((10#${BASH_REMATCH[1]})) b=$((10#${BASH_REMATCH[2]})) c=$((10#${BASH_REMATCH[3]})) d=$((10#${BASH_REMATCH[4]}))
	[ "$a" -le 255 ] && [ "$b" -le 255 ] && [ "$c" -le 255 ] && [ "$d" -le 255 ] || return 1
	echo $(((a << 24) | (b << 16) | (c << 8) | d))
}

# Address space that no public service lives in: private, loopback,
# link-local, shared, documentation, benchmarking, multicast, reserved.
readonly RESERVED_V4="0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/3"

# valid_cidr4 accepts a public IPv4 range of at least /MIN_PREFIX_V4 with
# no host bits set.
valid_cidr4() {
	[[ $1 =~ ^([0-9.]+)/([0-9]{1,2})$ ]] || return 1
	local prefix start size reserved rstart rsize
	prefix=$((10#${BASH_REMATCH[2]}))
	[ "$prefix" -ge "$MIN_PREFIX_V4" ] && [ "$prefix" -le 32 ] || return 1
	start="$(ipv4_int "${BASH_REMATCH[1]}")" || return 1
	size=$((1 << (32 - prefix)))
	[ $((start % size)) -eq 0 ] || return 1
	for reserved in $RESERVED_V4; do
		rstart="$(ipv4_int "${reserved%/*}")"
		rsize=$((1 << (32 - ${reserved#*/})))
		if [ "$start" -lt $((rstart + rsize)) ] && [ $((start + size)) -gt "$rstart" ]; then
			return 1
		fi
	done
}

# ipv6_hex prints an IPv6 address as 32 lowercase hex digits.
ipv6_hex() {
	local addr
	addr="$(lower "$1")"
	[[ $addr =~ ^[0-9a-f:]+$ ]] || return 1
	local head tail groups=() tail_groups=() out="" group
	if [[ $addr == *::* ]]; then
		[[ ${addr#*::} != *::* ]] || return 1
		head="${addr%%::*}"
		tail="${addr#*::}"
		[ -z "$head" ] || IFS=: read -r -a groups <<<"$head"
		[ -z "$tail" ] || IFS=: read -r -a tail_groups <<<"$tail"
		[ $((${#groups[@]} + ${#tail_groups[@]})) -le 7 ] || return 1
		while [ $((${#groups[@]} + ${#tail_groups[@]})) -lt 8 ]; do
			groups+=(0)
		done
		groups+=(${tail_groups[@]+"${tail_groups[@]}"})
	else
		IFS=: read -r -a groups <<<"$addr"
		[ ${#groups[@]} -eq 8 ] || return 1
	fi
	for group in "${groups[@]}"; do
		[[ $group =~ ^[0-9a-f]{1,4}$ ]] || return 1
		out+="$(printf '%04x' "$((16#$group))")"
	done
	echo "$out"
}

# valid_cidr6 accepts a global unicast (2000::/3) IPv6 range of at least
# /MIN_PREFIX_V6 with no host bits set.
valid_cidr6() {
	[[ $1 =~ ^([0-9a-fA-F:]+)/([0-9]{1,3})$ ]] || return 1
	local hex prefix first
	prefix=$((10#${BASH_REMATCH[2]}))
	[ "$prefix" -ge "$MIN_PREFIX_V6" ] && [ "$prefix" -le 128 ] || return 1
	hex="$(ipv6_hex "${BASH_REMATCH[1]}")" || return 1
	first=$((16#${hex:0:4}))
	[ "$first" -ge $((16#2000)) ] && [ "$first" -le $((16#3fff)) ] || return 1
	[ "$(masked_hex "$hex" "$prefix")" = "$hex" ]
}

# masked_hex keeps the first prefix bits of a 32-digit hex address and
# zeroes the rest.
masked_hex() {
	local hex="$1" prefix="$2" full rest nibble out
	full=$((prefix / 4))
	rest=$((prefix % 4))
	out="${hex:0:full}"
	if [ "$full" -lt 32 ]; then
		if [ "$rest" -gt 0 ]; then
			nibble=$((16#${hex:full:1} & (0xf << (4 - rest)) & 0xf))
			out+="$(printf '%x' "$nibble")"
			full=$((full + 1))
		fi
		while [ "${#out}" -lt 32 ]; do
			out+="0"
		done
	fi
	echo "$out"
}

# in_ranges reports whether an address lies in one of the Cloudflare
# ranges of its family.
in_ranges() {
	local addr="$1" range
	if [[ $addr == *:* ]]; then
		local hex
		hex="$(ipv6_hex "$addr")" || return 1
		for range in ${cf_v6[@]+"${cf_v6[@]}"}; do
			local net
			net="$(ipv6_hex "${range%/*}")" || continue
			[ "$(masked_hex "$hex" "${range#*/}")" = "$(masked_hex "$net" "${range#*/}")" ] && return 0
		done
	else
		local ip
		ip="$(ipv4_int "$addr")" || return 1
		for range in ${cf_v4[@]+"${cf_v4[@]}"}; do
			local start size
			start="$(ipv4_int "${range%/*}")" || continue
			size=$((1 << (32 - ${range#*/})))
			[ "$ip" -ge "$start" ] && [ "$ip" -lt $((start + size)) ] && return 0
		done
	fi
	return 1
}

# resolve prints the domain's IPv4 and IPv6 addresses, one per line.
resolve() {
	{
		getent ahostsv4 "$1" 2>/dev/null | awk '{ print $1 }'
		getent ahostsv6 "$1" 2>/dev/null | awk '{ print $1 }' | grep -v -i '^::ffff:'
	} | sort -u || true
}

# public_addresses prints the addresses this host reaches the internet
# from, as Cloudflare's trace endpoint sees them; nothing when offline.
public_addresses() {
	local family
	for family in -4 -6; do
		curl "$family" -fsS --max-time 5 "$TRACE_URL" 2>/dev/null | sed -n 's/^ip=//p' || true
	done | sort -u
}

all_in_cloudflare() {
	local addrs="$1" addr
	[ -n "$addrs" ] || return 1
	while IFS= read -r addr; do
		in_ranges "$addr" || return 1
	done <<<"$addrs"
}

# choose_mode settles both choices, each from the first of: the command
# line, the mode recorded in .env, detection on a first install, the
# default.
choose_mode() {
	external="${opt_external:-$env_external}"
	external="${external:-no}"

	cloudflare="${opt_cloudflare:-$env_cloudflare}"
	if [ -z "$cloudflare" ]; then
		cloudflare=no
		if [ "$env_exists" = false ] && all_in_cloudflare "$(resolve "$domain")"; then
			if [ "$has_tty" = true ]; then
				echo "$domain resolves to Cloudflare: the domain is proxied by Cloudflare." >/dev/tty
				echo "Its SSL/TLS encryption mode must be Full (strict)." >/dev/tty
				if confirm "Set up the panel for Cloudflare? [Y/n] " y; then
					cloudflare=yes
				fi
			else
				cloudflare=yes
				info "$domain resolves to Cloudflare, so the panel is set up for Cloudflare (pass --no-cloudflare to change that)."
				info "Cloudflare's SSL/TLS encryption mode for the domain must be Full (strict)."
			fi
		fi
	fi

	if [ "$external" = yes ]; then
		compose_file="$COMPOSE_EXTERNAL"
	else
		compose_file="$COMPOSE_INTEGRATED"
	fi
	info "mode: $(mode_name)"
}

mode_name() {
	local proxy="the panel's own Caddy" cdn="DNS only"
	[ "$external" = no ] || proxy="an existing reverse proxy"
	[ "$cloudflare" = no ] || cdn="Cloudflare proxy"
	echo "$cdn, $proxy"
}

preflight() {
	if [ "$external" = no ]; then
		check_ports
	fi
	check_dns
}

# check_ports stops before Caddy fails to bind: the panel's own Caddy needs
# ports 80 and 443. Ports held by this installation's Caddy are fine.
check_ports() {
	command -v ss >/dev/null 2>&1 || {
		warn "cannot check whether ports 80 and 443 are free (ss is missing)"
		return 0
	}
	if [ -n "$(docker ps -q --filter "name=^${CADDY_CONTAINER}\$" 2>/dev/null)" ]; then
		return 0
	fi
	local busy
	busy="$({
		ss -Hltn '( sport = :80 or sport = :443 )'
		ss -Hlun '( sport = :443 )'
	} 2>/dev/null | awk '{ print $4 }' | sort -u | tr '\n' ' ')"
	[ -z "$busy" ] || die "ports 80 and 443 must be free for the panel's own Caddy, but these are in use: $busy
If a web server already runs on this host, put the panel behind it instead:
  curl -fsSL https://get.kerge.io | sudo bash -s -- --external-proxy
Nothing was changed."
}

# check_dns warns when the domain does not point where it has to.
check_dns() {
	local addrs listed
	addrs="$(resolve "$domain")"
	listed="$(tr '\n' ' ' <<<"$addrs")"
	if [ -z "$addrs" ]; then
		dns_warning "$domain does not resolve to any address yet"
		return
	fi
	if [ "$cloudflare" = yes ]; then
		all_in_cloudflare "$addrs" ||
			dns_warning "$domain resolves to $listed, not to Cloudflare; is the domain's proxy (orange cloud) on?"
		return
	fi
	local mine
	mine="$(public_addresses)"
	if [ -z "$mine" ]; then
		info "could not find out this host's public address; not checking where $domain points"
		return
	fi
	if [ -z "$(comm -12 <(echo "$addrs") <(echo "$mine"))" ]; then
		dns_warning "$domain resolves to $listed, but this host's public address is $(tr '\n' ' ' <<<"$mine")"
	fi
}

# dns_warning lets the operator stop; without a terminal it only warns.
dns_warning() {
	warn "$1"
	if [ "$has_tty" = true ]; then
		confirm "Continue anyway? [y/N] " n || die "nothing was changed"
	else
		warn "continuing; the certificate cannot be issued until this is fixed"
	fi
}

# download_files fetches and verifies everything this mode installs before
# anything on the host changes.
download_files() {
	if [ "$external" = yes ]; then
		download_verified "$COMPOSE_EXTERNAL"
	else
		download_verified "$COMPOSE_INTEGRATED"
		if [ "$cloudflare" = yes ]; then
			download_verified Caddyfile.cloudflare
		else
			download_verified Caddyfile
		fi
	fi
}

install_files() {
	mkdir -p "$dir"
	cd "$dir"

	# Switching modes: stop what the other compose file started, so that
	# its containers and network do not linger. Data and certificates stay.
	local other="$COMPOSE_INTEGRATED"
	[ "$external" = no ] && other="$COMPOSE_EXTERNAL"
	if [ -f "$other" ]; then
		info "stopping the containers of the previous mode"
		docker compose --project-directory "$dir" -f "$other" down
		rm -f "$other"
	fi

	install -m 0644 "$workdir/$compose_file" "$compose_file"

	if [ "$external" = yes ]; then
		rm -f Caddyfile caddy/conf.d/cloudflare-ips.caddy
	else
		local caddyfile=Caddyfile
		[ "$cloudflare" = no ] || caddyfile=Caddyfile.cloudflare
		replace_file "$workdir/$caddyfile" Caddyfile
		mkdir -p caddy/conf.d
		if [ "$cloudflare" = yes ]; then
			local ranges=(${cf_v4[@]+"${cf_v4[@]}"} ${cf_v6[@]+"${cf_v6[@]}"})
			printf 'trusted_proxies static %s\n' "${ranges[*]}" >"$workdir/cloudflare-ips.caddy"
			replace_file "$workdir/cloudflare-ips.caddy" caddy/conf.d/cloudflare-ips.caddy
		elif [ -e caddy/conf.d/cloudflare-ips.caddy ]; then
			rm -f caddy/conf.d/cloudflare-ips.caddy
			caddy_changed=true
		fi
	fi

	mkdir -p data
	chown "$PANEL_UID:$PANEL_UID" data
}

# replace_file installs a file and notes whether Caddy has to reload.
replace_file() {
	local src="$1" dst="$2"
	if [ -f "$dst" ] && cmp -s "$src" "$dst"; then
		return
	fi
	install -m 0644 "$src" "$dst"
	caddy_changed=true
}

# write_env creates .env on the first run and afterwards only updates the
# recorded mode.
write_env() {
	if [ "$env_exists" = false ]; then
		cat >.env <<-ENV
			# Written by install.sh. Every variable is described in .env.example
			# in the panel repository. Never put a password, key or token here.
			KERGE_DOMAIN=$domain

			# Deployment mode. Running install.sh again without --cloudflare,
			# --no-cloudflare, --external-proxy or --no-external-proxy keeps it.
			KERGE_CLOUDFLARE=$cloudflare
			KERGE_EXTERNAL_PROXY=$external
		ENV
		chmod 0644 .env
		return
	fi
	set_env_key KERGE_CLOUDFLARE "$cloudflare"
	set_env_key KERGE_EXTERNAL_PROXY "$external"
}

set_env_key() {
	local key="$1" value="$2"
	if grep -q "^$key=" .env; then
		sed -i "s/^$key=.*/$key=$value/" .env
	else
		printf '%s=%s\n' "$key" "$value" >>.env
	fi
}

start_panel() {
	local caddy_was_running=false
	if [ -n "$(docker ps -q --filter "name=^${CADDY_CONTAINER}\$")" ]; then
		caddy_was_running=true
	fi
	info "starting the panel (release v$version)"
	docker compose --project-directory "$dir" -f "$compose_file" up -d
	# Caddy reads its configuration when it starts; a running Caddy keeps
	# the old one until restarted.
	if [ "$external" = no ] && [ "$caddy_changed" = true ] && [ "$caddy_was_running" = true ]; then
		info "restarting Caddy for its new configuration"
		docker compose --project-directory "$dir" -f "$compose_file" restart caddy
	fi

	local status="" waited=0
	while [ "$waited" -lt 120 ]; do
		status="$(docker inspect -f '{{.State.Health.Status}}' "$PANEL_CONTAINER" 2>/dev/null || true)"
		[ "$status" = healthy ] && return
		sleep 2
		waited=$((waited + 2))
	done
	die "the panel did not become healthy (status: ${status:-unknown}); see: docker logs $PANEL_CONTAINER"
}

# show_setup_code prints the code a panel that is not set up yet logs when
# it starts. Only the log since the current start counts.
show_setup_code() {
	local started code
	started="$(docker inspect -f '{{.State.StartedAt}}' "$PANEL_CONTAINER")"
	code="$(docker logs --since "$started" "$PANEL_CONTAINER" 2>&1 | sed -n 's/^Setup code: //p' | tail -n 1)"
	echo
	if [ -n "$code" ]; then
		echo "  Kerge is running. Open https://$domain in a browser"
		echo "  and enter this setup code:"
		echo
		echo "    $code"
	else
		echo "  Kerge is running and already set up: https://$domain"
	fi
	echo
}

# self_check reaches the panel the way browsers and agents will, through
# the public address, and explains what it finds instead of failing.
self_check() {
	if [ "$external" = yes ]; then
		local code
		code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:3000/healthz || true)"
		if [ "$code" != 200 ]; then
			warn "the panel does not answer on 127.0.0.1:3000 (status $code); see: docker logs $PANEL_CONTAINER"
			return
		fi
		info "the panel answers on 127.0.0.1:3000; point your reverse proxy at it"
	fi

	info "checking https://$domain from the internet (this can take a minute while the certificate is issued)"
	local deadline=$((SECONDS + SELFCHECK_SECONDS)) rc code headers="$workdir/headers"
	while :; do
		rc=0
		rm -f "$headers"
		code="$(curl -sS -o /dev/null -D "$headers" -w '%{http_code}' --max-time 10 -L --max-redirs 5 \
			"https://$domain/healthz" 2>/dev/null)" || rc=$?
		if [ "$rc" -eq 0 ] && [ "$code" = 200 ]; then
			break
		fi
		if [ "$rc" -eq 47 ] || challenged "$headers" || [ "$SECONDS" -ge "$deadline" ]; then
			explain_failure "https://$domain/healthz" "$rc" "$code" "$headers"
			return
		fi
		sleep 5
	done
	info "https://$domain answers"
	check_agent_endpoint
}

challenged() {
	[ -f "$1" ] && grep -qi '^cf-mitigated:[[:space:]]*challenge' "$1"
}

explain_failure() {
	local url="$1" rc="$2" code="$3" headers="$4"
	if [ "$rc" -eq 47 ]; then
		warn "$url redirects in a loop: set Cloudflare's SSL/TLS encryption mode for the domain to Full (strict)"
	elif challenged "$headers"; then
		warn "Cloudflare answers $url with a challenge: turn off Bot Fight Mode, Under Attack mode or the rule that challenges it"
	elif [ "$external" = yes ]; then
		warn "$url cannot be reached yet (status ${code:-none}): configure your reverse proxy to forward it to 127.0.0.1:3000; the README and its examples show how"
	elif [ "$code" = 525 ] || [ "$code" = 526 ] || [ "$rc" -eq 35 ] || [ "$rc" -eq 51 ] || [ "$rc" -eq 60 ]; then
		warn "$url has no valid certificate yet: either it has not been issued (see: docker logs $CADDY_CONTAINER), or Cloudflare's SSL/TLS encryption mode is not Full (strict)"
	else
		warn "$url cannot be reached (status ${code:-none}); check that $domain points to this host and that ports 80 and 443 are open (see: docker logs $CADDY_CONTAINER)"
	fi
}

# check_agent_endpoint makes the handshake request an agent makes, without
# credentials: reaching the panel is enough (it refuses the request).
check_agent_endpoint() {
	local url="https://$domain/api/agent/ws" headers="$workdir/ws-headers" code rc=0 key
	key="$(head -c 16 /dev/urandom | base64)"
	code="$(curl -sS -o /dev/null -D "$headers" -w '%{http_code}' --max-time 10 --http1.1 \
		-A "$AGENT_USER_AGENT" -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $key" \
		-H "Sec-WebSocket-Protocol: $AGENT_PROTOCOL" "$url" 2>/dev/null)" || rc=$?
	case "$code" in
	400 | 401 | 503)
		info "agents can reach $url"
		return
		;;
	esac
	if challenged "$headers"; then
		warn "Cloudflare answers agents with a challenge at $url, so agents cannot connect: turn off Bot Fight Mode, Under Attack mode or the rule that challenges it"
	else
		warn "agents may not be able to reach $url (status ${code:-none}, curl exit $rc)"
	fi
}

main "$@"
