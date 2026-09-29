#!/bin/bash
# One case of the install.sh test suite, run inside a throwaway container
# without network by scripts/test-install.sh. The repository is mounted at
# /src, read only.
#
# The release the installer downloads is built here from deploy/, with a
# checksums file and an OpenSSH signature made with a key generated for the
# run; Cloudflare's lists and trace endpoint are files too. Docker is a stub
# that logs its calls, since the container runs no Docker daemon. Names
# resolve through /etc/hosts. Everything else is the real script.
set -euo pipefail

readonly RELEASE_VERSION="1.2.3"
readonly INSTALLER="/work/install.sh"
readonly DOCKER_LOG="/work/docker.log"
readonly STATE="/work/state"
readonly OUT="/work/out"
readonly DIR="/opt/kerge"
readonly LIVE_V4="104.16.0.0/13 172.64.0.0/13"
readonly LIVE_V6="2606:4700::/32"

fail() {
	echo "FAIL: $*" >&2
	if [ -f "$OUT" ]; then
		echo "--- installer output:" >&2
		cat "$OUT" >&2
	fi
	exit 1
}

pass() {
	echo "ok: $*"
}

# stub_docker stands in for Docker and Compose. It answers the calls the
# installer makes and logs every one.
stub_docker() {
	mkdir -p "$STATE"
	cat >/usr/local/bin/docker <<-STUB
		#!/bin/bash
		echo "\$*" >>$DOCKER_LOG
		case "\$*" in
		"compose version") echo "Docker Compose version v5.0.0" ;;
		"info") ;;
		*"-f compose.yml up -d") touch $STATE/caddy ;;
		*"-f compose.external-proxy.yml up -d") rm -f $STATE/caddy ;;
		*" down") rm -f $STATE/caddy ;;
		*" restart caddy") ;;
		"ps -q --filter name=^kerge-caddy"*) [ ! -f $STATE/caddy ] || echo 0123456789ab ;;
		"inspect -f {{.State.Health.Status}} kerge") echo healthy ;;
		"inspect -f {{.State.StartedAt}} kerge") echo 2026-01-01T00:00:00.000000000Z ;;
		"logs --since "*)
			if [ ! -f $STATE/initialized ]; then
				echo "Kerge is not set up yet. Open the panel in a browser and enter this code:"
				echo "Setup code: TEST-SETUP-CODE"
			fi
			;;
		*)
			echo "docker stub: unexpected call: \$*" >&2
			exit 1
			;;
		esac
	STUB
	chmod 0755 /usr/local/bin/docker
	: >"$DOCKER_LOG"
}

# build_release publishes a release of the deployment files, signed with
# the key the prepared installer trusts, or with sign_key when given.
build_release() {
	local version="$1" sign_key="${2:-/work/release_key}"
	local dir="/release/v$version"
	mkdir -p "$dir"
	cp /src/deploy/compose.yml /src/deploy/compose.external-proxy.yml /src/deploy/Caddyfile \
		/src/deploy/Caddyfile.cloudflare /src/deploy/cloudflare-ips.txt "$dir/"
	echo "# release $version" >>"$dir/compose.yml"
	(cd "$dir" && sha256sum compose.yml compose.external-proxy.yml Caddyfile Caddyfile.cloudflare cloudflare-ips.txt >checksums.txt)
	if [ "$sign_key" != /work/release_key ] && [ ! -f "$sign_key" ]; then
		ssh-keygen -q -t ed25519 -N '' -C attacker -f "$sign_key"
	fi
	ssh-keygen -Y sign -q -f "$sign_key" -n kerge-release "$dir/checksums.txt"
}

# serve_cloudflare writes the lists and the trace answer the installer
# fetches from Cloudflare.
serve_cloudflare() {
	mkdir -p /cf
	tr ' ' '\n' <<<"$LIVE_V4" >/cf/ips-v4
	# Cloudflare's lists end without a newline.
	printf '%s' "$LIVE_V6" >/cf/ips-v6
	echo "ip=127.0.0.1" >/cf/trace
}

# prepare_installer copies the installer with the values a release carries
# and the test locations of everything it downloads.
prepare_installer() {
	local pubkey
	pubkey="$(cat /work/release_key.pub)"
	sed -e "s|^VERSION=.*|VERSION=\"$RELEASE_VERSION\"|" \
		-e "s|^BASE_URL=.*|BASE_URL=\"file:///release\"|" \
		-e "s|^RELEASE_PUBKEY=.*|RELEASE_PUBKEY=\"$pubkey\"|" \
		-e "s|^CF_IPS_V4_URL=.*|CF_IPS_V4_URL=\"file:///cf/ips-v4\"|" \
		-e "s|^CF_IPS_V6_URL=.*|CF_IPS_V6_URL=\"file:///cf/ips-v6\"|" \
		-e "s|^TRACE_URL=.*|TRACE_URL=\"file:///cf/trace\"|" \
		-e "s|^SELFCHECK_SECONDS=.*|SELFCHECK_SECONDS=0|" \
		/src/install.sh >"$INSTALLER"
	chmod 0755 "$INSTALLER"
}

# give_address gives the container an address besides loopback. Without
# one, name lookups (getaddrinfo with AI_ADDRCONFIG, as getent uses it)
# return nothing. The interface routes nowhere, so the container stays
# offline.
give_address() {
	ip link add dummy0 type dummy
	ip link set dummy0 up
	ip addr add 192.0.2.1/24 dev dummy0
	ip -6 addr add 2001:db8::1/64 dev dummy0 nodad
}

setup() {
	mkdir -p /work
	give_address
	ssh-keygen -q -t ed25519 -N '' -C kerge-release -f /work/release_key
	stub_docker
	build_release "$RELEASE_VERSION" "$@"
	serve_cloudflare
	prepare_installer
	# panel.test points at this host; cf.test is proxied by Cloudflare.
	{
		echo "127.0.0.1 panel.test"
		echo "104.16.0.1 cf.test"
		echo "2606:4700::1 cf.test"
	} >>/etc/hosts
}

# install runs the installer with the given options and records its output.
install() {
	"$INSTALLER" "$@" >"$OUT" 2>&1 || fail "the installer failed: $*"
}

# install_fails runs the installer and expects it to stop.
install_fails() {
	if "$INSTALLER" "$@" >"$OUT" 2>&1; then
		fail "the installer succeeded: $*"
	fi
}

output_has() {
	grep -qF -- "$1" "$OUT" || fail "the output does not mention: $1"
}

output_lacks() {
	! grep -qF -- "$1" "$OUT" || fail "the output mentions: $1"
}

docker_called() {
	grep -qF -- "$1" "$DOCKER_LOG" || fail "docker was not called with: $1 (calls: $(tr '\n' ';' <"$DOCKER_LOG"))"
}

docker_not_called() {
	! grep -qF -- "$1" "$DOCKER_LOG" || fail "docker was called with: $1"
}

env_is() {
	grep -qx "$1=$2" "$DIR/.env" || fail ".env does not set $1=$2: $(cat "$DIR/.env")"
}

same_file() {
	cmp -s "$1" "$2" || fail "$2 differs from $1"
}

# assert_nothing_changed is what every refused run must leave behind.
assert_nothing_changed() {
	[ ! -e "$DIR" ] || fail "$DIR was created: $(ls -A "$DIR")"
	docker_not_called " up -d"
	docker_not_called " down"
}

assert_integrated() {
	local caddyfile="$1"
	same_file "/release/v$RELEASE_VERSION/compose.yml" "$DIR/compose.yml"
	same_file "/src/deploy/$caddyfile" "$DIR/Caddyfile"
	[ ! -e "$DIR/compose.external-proxy.yml" ] || fail "the other mode's compose file is there"
	[ -d "$DIR/caddy/conf.d" ] || fail "caddy/conf.d was not created"
	docker_called "compose --project-directory $DIR -f compose.yml up -d"
}

assert_data_dir() {
	[ "$(stat -c '%u:%g' "$DIR/data")" = "65532:65532" ] || fail "data is owned by $(stat -c '%u:%g' "$DIR/data")"
}

cloudflare_line() {
	echo "trusted_proxies static $*"
}

case_install() {
	setup
	install --domain panel.test
	assert_integrated Caddyfile
	[ -z "$(ls -A "$DIR/caddy/conf.d")" ] || fail "caddy/conf.d is not empty without Cloudflare"
	env_is KERGE_DOMAIN panel.test
	env_is KERGE_CLOUDFLARE no
	env_is KERGE_EXTERNAL_PROXY no
	assert_data_dir
	output_has "TEST-SETUP-CODE"
	output_has "https://panel.test"
	output_lacks "resolves to"
	docker_not_called "restart caddy"
	pass "a fresh install sets up the panel with its own Caddy"
}

# A domain that resolves only into Cloudflare's ranges is set up for
# Cloudflare, with the ranges fetched just now.
case_cloudflare_detect() {
	setup
	install --domain cf.test
	output_has "resolves to Cloudflare"
	output_has "Full (strict)"
	env_is KERGE_CLOUDFLARE yes
	assert_integrated Caddyfile.cloudflare
	[ "$(cat "$DIR/caddy/conf.d/cloudflare-ips.caddy")" = "$(cloudflare_line "$LIVE_V4" "$LIVE_V6")" ] ||
		fail "unexpected ranges file: $(cat "$DIR/caddy/conf.d/cloudflare-ips.caddy")"
	pass "a domain behind Cloudflare is detected"
}

snapshot_line() {
	# shellcheck disable=SC2046 # one word per range
	cloudflare_line $(sed -e 's/#.*//' /src/deploy/cloudflare-ips.txt | grep -v '^[[:space:]]*$')
}

# When Cloudflare's lists cannot be fetched, the signed snapshot is used
# and the operator is told how old it is.
case_cloudflare_fallback() {
	setup
	rm /cf/ips-v6
	install --domain cf.test --cloudflare
	output_has "using the snapshot in release v$RELEASE_VERSION, taken on 2026-09-28"
	[ "$(cat "$DIR/caddy/conf.d/cloudflare-ips.caddy")" = "$(snapshot_line)" ] ||
		fail "the snapshot was not used: $(cat "$DIR/caddy/conf.d/cloudflare-ips.caddy")"
	pass "without Cloudflare's lists the release snapshot is used"
}

# A list that does not look like Cloudflare's is not trusted.
case_cloudflare_invalid() {
	setup
	local bad
	for bad in 0.0.0.0/0 10.0.0.0/8 192.168.1.0/24 104.16.0.1/13 104.0.0.0/6 not-a-range 104.16.0.0/33; do
		serve_cloudflare
		echo "$bad" >>/cf/ips-v4
		install --domain cf.test --cloudflare
		output_has "using the snapshot"
	done
	for bad in ::/0 fd00::/8 fd12:3456::/32 fe80::/64 ff02::/32 2606:4700::1/32 2606::/16 2606:4700:::/32; do
		serve_cloudflare
		printf '\n%s' "$bad" >>/cf/ips-v6
		install --domain cf.test --cloudflare
		output_has "using the snapshot"
	done
	serve_cloudflare
	for _ in $(seq 1 101); do echo "104.16.0.0/13"; done >/cf/ips-v4
	install --domain cf.test --cloudflare
	output_has "using the snapshot"
	serve_cloudflare
	install --domain cf.test --cloudflare
	output_lacks "using the snapshot"
	pass "lists with unsafe or malformed entries are replaced by the snapshot"
}

# Running again keeps the domain, the mode, the data and whatever the
# operator added to .env; --version upgrades the deployment files.
case_rerun_upgrade() {
	setup
	install --domain panel.test
	echo "KERGE_NET_SUBNET=10.99.0.0/24" >>"$DIR/.env"
	echo "database" >"$DIR/data/kerge.db"
	touch "$STATE/initialized"
	build_release 1.2.4
	: >"$DOCKER_LOG"

	install --version 1.2.4
	grep -qx '# release 1.2.4' "$DIR/compose.yml" || fail "compose.yml was not upgraded"
	env_is KERGE_DOMAIN panel.test
	env_is KERGE_CLOUDFLARE no
	env_is KERGE_NET_SUBNET 10.99.0.0/24
	[ "$(cat "$DIR/data/kerge.db")" = database ] || fail "the data was touched"
	output_has "already set up"
	output_lacks "TEST-SETUP-CODE"
	docker_not_called "restart caddy"
	pass "running again upgrades and keeps domain, mode, settings and data"
}

# Switching Cloudflare off replaces the Caddyfile, empties conf.d and
# restarts the running Caddy; the next run without options keeps it off.
case_switch_cloudflare() {
	setup
	install --domain cf.test
	env_is KERGE_CLOUDFLARE yes
	: >"$DOCKER_LOG"

	install --no-cloudflare
	env_is KERGE_CLOUDFLARE no
	assert_integrated Caddyfile
	[ ! -e "$DIR/caddy/conf.d/cloudflare-ips.caddy" ] || fail "the Cloudflare ranges were left in place"
	docker_called "restart caddy"

	: >"$DOCKER_LOG"
	install
	env_is KERGE_CLOUDFLARE no
	docker_not_called "restart caddy"

	install --cloudflare
	env_is KERGE_CLOUDFLARE yes
	same_file /src/deploy/Caddyfile.cloudflare "$DIR/Caddyfile"
	docker_called "restart caddy"

	# Detection is for first installs only: an installation whose .env
	# lost the mode falls back to the default.
	sed -i '/^KERGE_CLOUDFLARE=/d' "$DIR/.env"
	install
	env_is KERGE_CLOUDFLARE no
	output_lacks "resolves to Cloudflare"
	pass "switching Cloudflare mode on and off reconfigures Caddy"
}

case_external_proxy() {
	setup
	install --domain panel.test --external-proxy
	env_is KERGE_EXTERNAL_PROXY yes
	same_file "/release/v$RELEASE_VERSION/compose.external-proxy.yml" "$DIR/compose.external-proxy.yml"
	for f in compose.yml Caddyfile; do
		[ ! -e "$DIR/$f" ] || fail "$f is there in external proxy mode"
	done
	docker_called "compose --project-directory $DIR -f compose.external-proxy.yml up -d"
	output_has "127.0.0.1:3000"
	output_has "TEST-SETUP-CODE"
	assert_data_dir

	: >"$DOCKER_LOG"
	install --no-external-proxy
	env_is KERGE_EXTERNAL_PROXY no
	docker_called "compose --project-directory $DIR -f compose.external-proxy.yml down"
	assert_integrated Caddyfile
	pass "external proxy mode runs the panel alone and can be switched back"
}

# The ports check stops before anything is written when a web server
# holds 80 or 443, and points at external proxy mode.
case_ports_busy() {
	setup
	cat >/usr/local/bin/ss <<-'STUB'
		#!/bin/sh
		case "$*" in
		*-Hltn*) echo "LISTEN 0 511 0.0.0.0:80 0.0.0.0:*" ;;
		esac
	STUB
	chmod 0755 /usr/local/bin/ss
	install_fails --domain panel.test
	output_has "0.0.0.0:80"
	output_has "--external-proxy"
	assert_nothing_changed
	install --domain panel.test --external-proxy
	pass "busy ports stop the install and suggest external proxy mode"
}

# A domain that points elsewhere is reported; without a terminal the
# install goes on.
case_dns_mismatch() {
	setup
	echo "ip=198.51.100.7" >/cf/trace
	install --domain panel.test
	output_has "panel.test resolves to 127.0.0.1, but this host's public address is 198.51.100.7"
	assert_integrated Caddyfile

	install --domain panel.test --cloudflare
	output_has "not to Cloudflare"
	pass "a domain that does not point here is reported"
}

case_bad_signature() {
	setup /work/attacker_key
	install_fails --domain panel.test
	output_has "signature"
	assert_nothing_changed
	pass "a release signed with another key is refused"
}

case_bad_checksum() {
	setup
	echo "tampered" >>"/release/v$RELEASE_VERSION/Caddyfile"
	install_fails --domain panel.test
	output_has "checksum of Caddyfile"
	assert_nothing_changed
	pass "a tampered deployment file is refused"
}

case_no_docker() {
	setup
	rm /usr/local/bin/docker
	install_fails --domain panel.test
	output_has "https://get.docker.com"
	[ ! -e "$DIR" ] || fail "$DIR was created"
	pass "without Docker the installer explains how to get it"
}

case_no_domain() {
	setup
	install_fails
	output_has "--domain"
	assert_nothing_changed
	install_fails --domain 203.0.113.9
	output_has "not a domain name"
	assert_nothing_changed
	pass "without a terminal the domain has to be passed, and must be a name"
}

case_domain_change() {
	setup
	install --domain panel.test
	cp "$DIR/.env" /work/env.before
	install_fails --domain other.test
	output_has "change KERGE_DOMAIN"
	same_file /work/env.before "$DIR/.env"
	install --domain PANEL.test
	pass "an installation keeps its domain"
}

# The script in the repository carries no version and installs nothing.
case_unreleased_script() {
	setup
	if /src/install.sh --domain panel.test >"$OUT" 2>&1; then
		fail "the unreleased script installed something"
	fi
	output_has "no release version"
	assert_nothing_changed
	pass "the unreleased script refuses to install"
}

# A download cut short anywhere runs nothing: only the last line calls
# main.
case_truncated() {
	setup
	local lines n
	lines="$(wc -l <"$INSTALLER")"
	for n in 1 20 60 $((lines / 4)) $((lines / 2)) $((lines * 3 / 4)) $((lines - 1)); do
		head -n "$n" "$INSTALLER" >/work/truncated.sh
		bash /work/truncated.sh --domain panel.test >"$OUT" 2>&1 || true
		assert_nothing_changed
		[ ! -s "$DOCKER_LOG" ] || fail "the first $n lines called docker: $(cat "$DOCKER_LOG")"
	done
	[ "$(tail -n 1 /src/install.sh)" = 'main "$@"' ] || fail "the last line does not call main"
	pass "a truncated script does nothing"
}

# The key in the repository is the release key, so it rejects a signature
# made with any other key.
case_release_key() {
	setup /work/attacker_key
	grep -q '^RELEASE_PUBKEY="ssh-ed25519 AAAA' /src/install.sh || fail "the script carries no release key"
	sed -e "s|^VERSION=.*|VERSION=\"$RELEASE_VERSION\"|" \
		-e "s|^BASE_URL=.*|BASE_URL=\"file:///release\"|" \
		/src/install.sh >/work/shipped.sh
	if bash /work/shipped.sh --domain panel.test >"$OUT" 2>&1; then
		fail "the shipped key verified a signature it did not make"
	fi
	output_has "signature"
	assert_nothing_changed
	pass "the shipped release key rejects foreign signatures"
}

# The address helpers, loaded from the script without running it.
case_helpers() {
	# shellcheck source=/dev/null
	source <(sed '$d' /src/install.sh)
	local range
	while IFS= read -r range; do
		if [[ $range == *:* ]]; then
			valid_cidr6 "$range" || fail "the snapshot range $range was rejected"
		else
			valid_cidr4 "$range" || fail "the snapshot range $range was rejected"
		fi
	done < <(sed -e 's/#.*//' /src/deploy/cloudflare-ips.txt | grep -v '^[[:space:]]*$')

	[ "$(ipv6_hex ::)" = "00000000000000000000000000000000" ] || fail "ipv6_hex ::"
	[ "$(ipv6_hex 2606:4700::1)" = "26064700000000000000000000000001" ] || fail "ipv6_hex 2606:4700::1"
	[ "$(ipv6_hex 1:2:3:4:5:6:7:8)" = "00010002000300040005000600070008" ] || fail "ipv6_hex full"
	for range in 1::2::3 1:2:3:4:5:6:7:8:9 12345:: ::ffff:1.2.3.4 g::; do
		! ipv6_hex "$range" >/dev/null || fail "ipv6_hex accepted $range"
	done

	# shellcheck disable=SC2034 # read by in_ranges
	cf_v4=(104.16.0.0/13 131.0.72.0/22)
	# shellcheck disable=SC2034 # read by in_ranges
	cf_v6=(2606:4700::/32 2a06:98c0::/29)
	for range in 104.16.0.1 104.23.255.255 131.0.75.1 2606:4700::1 2a06:98c7:ffff::1; do
		in_ranges "$range" || fail "$range is in the ranges"
	done
	for range in 104.24.0.0 131.0.76.0 8.8.8.8 2606:4701::1 2a06:98c8::1 2001:db8::1; do
		! in_ranges "$range" || fail "$range is not in the ranges"
	done

	for range in panel.example.com a.b xn--bcher-kva.example PANEL.example.com; do
		valid_domain "$(lower "$range")" || fail "valid_domain rejected $range"
	done
	for range in localhost 1.2.3.4 -a.example a-.example a_b.example "" "a..example" "a.example."; do
		! valid_domain "$range" || fail "valid_domain accepted '$range'"
	done
	pass "the address and domain helpers"
}

[ $# -eq 1 ] || {
	echo "usage: install-cases.sh <case>" >&2
	exit 2
}
"case_${1//-/_}"
