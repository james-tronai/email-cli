#!/bin/sh
# Install the `email` CLI from GitHub Releases.
#
#   curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
#   curl -fsSL …/install.sh | sh -s -- --version v0.7.0 --dest ~/.local/bin
#
# Options:
#   --version vX.Y.Z   release to install (default: the latest)
#   --dest DIR         where to put `email` (default: ~/.local/bin; /usr/local/bin as root)
#   --verify           also check the cosign signature of checksums.txt (needs cosign)
#   --force            reinstall even if that version is already there
#
# Picks email_<os>_<arch> for linux/darwin on amd64/arm64 and checks it
# against the release's checksums.txt before installing. Running it again
# with the same version changes nothing. No GitHub token is needed.
set -eu

REPO="james-tronai/email-cli"
API="${EMAIL_INSTALL_API:-https://api.github.com}"
# Testing or a mirror: assets are fetched from $EMAIL_INSTALL_BASE_URL/<tag>/<name>.
BASE_URL="${EMAIL_INSTALL_BASE_URL:-}"

version=latest
dest=""
verify=0
force=0

say() { printf 'email-install: %s\n' "$*" >&2; }
die() {
	say "error: $*"
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) version="${2:?--version needs a value}"; shift 2 ;;
	--version=*) version="${1#*=}"; shift ;;
	--dest) dest="${2:?--dest needs a value}"; shift 2 ;;
	--dest=*) dest="${1#*=}"; shift ;;
	--verify) verify=1; shift ;;
	--force) force=1; shift ;;
	-h | --help)
		cat <<'USAGE'
usage: install.sh [--version vX.Y.Z] [--dest DIR] [--verify] [--force]
  --version   release to install (default: the latest)
  --dest      where to put `email` (default: ~/.local/bin; /usr/local/bin as root)
  --verify    also check the cosign signature of checksums.txt (needs cosign)
  --force     reinstall even if that version is already there
USAGE
		exit 0
		;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done

case "$version" in
latest | v[0-9]*) ;;
[0-9]*) version="v$version" ;;
*) die "--version must look like v1.2.3" ;;
esac

if [ -z "$dest" ]; then
	if [ "$(id -u)" = 0 ]; then dest=/usr/local/bin; else dest="${HOME:?HOME is not set}/.local/bin"; fi
fi

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported OS $(uname -s) (linux and darwin only)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported CPU $(uname -m) (amd64 and arm64 only)" ;;
esac
asset="email_${os}_${arch}"

if command -v curl >/dev/null 2>&1; then
	dl=curl
elif command -v wget >/dev/null 2>&1; then
	dl=wget
else
	die "curl or wget is required"
fi
# Retry resets and other transient failures too, where curl can (7.71+).
retry_all=""
if [ "$dl" = curl ] && curl --help all 2>/dev/null | grep -q -- --retry-all-errors; then
	retry_all="--retry-all-errors"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t email-install)"
trap 'rm -rf "$tmp"' EXIT INT TERM

# fetch URL OUT
fetch() {
	if [ "$dl" = curl ]; then
		curl -fsSL --proto '=https,http' --retry 3 $retry_all -A email-cli-install -o "$2" "$1"
	else
		wget -q -T 30 -U email-cli-install -O "$2" "$1" || { sleep 2 && wget -q -T 30 -U email-cli-install -O "$2" "$1"; }
	fi
}

# json_field NAME FILE: the first "NAME": "value" in a GitHub API reply
# (each member on its own line first; no jq needed).
json_field() {
	awk '{ gsub(/[,{}]/, "\n"); print }' <"$2" | sed -n "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n 1
}

if [ "$version" = latest ]; then
	[ -z "$BASE_URL" ] || die "--version is required with EMAIL_INSTALL_BASE_URL"
	fetch "$API/repos/$REPO/releases/latest" "$tmp/release.json" ||
		die "cannot read the latest release from $API"
	version="$(json_field tag_name "$tmp/release.json")"
	[ -n "$version" ] || die "no tag_name in the latest release"
fi

target="$dest/email"
if [ "$force" = 0 ] && [ -x "$target" ] &&
	"$target" version 2>/dev/null | grep -q "\"version\": *\"$version\""; then
	say "email $version is already installed at $target"
	exit 0
fi

get_asset() {
	if [ -n "$BASE_URL" ]; then
		url="$BASE_URL/$version/$1"
	else
		url="https://github.com/$REPO/releases/download/$version/$1"
	fi
	fetch "$url" "$tmp/$1" || die "download of $1 ($version) failed"
}

say "installing email $version ($os/$arch) into $dest"
get_asset "$asset"
get_asset checksums.txt

want="$(awk -v n="$asset" '$2 == n || $2 == "*" n { print $1; exit }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $asset"
if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	got="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
else
	got="$(openssl dgst -sha256 "$tmp/$asset" | awk '{print $NF}')"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $asset: expected $want, got $got"

if [ "$verify" = 1 ]; then
	command -v cosign >/dev/null 2>&1 || die "--verify needs cosign (https://docs.sigstore.dev)"
	get_asset checksums.txt.sigstore.json
	cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "^https://github.com/$REPO/\\.github/workflows/release\\.yml@refs/tags/v" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com \
		"$tmp/checksums.txt" >/dev/null 2>&1 || die "signature of checksums.txt does not verify"
	say "signature verified (keyless, release workflow of $REPO)"
fi

mkdir -p "$dest"
cp "$tmp/$asset" "$dest/.email.tmp.$$"
chmod 0755 "$dest/.email.tmp.$$"
mv -f "$dest/.email.tmp.$$" "$target"
say "installed $target ($("$target" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p'))"
case ":$PATH:" in
*":$dest:"*) ;;
*) say "note: $dest is not on PATH; add it, e.g. export PATH=\"$dest:\$PATH\"" ;;
esac
