#!/bin/sh
# Installs veans and marshal on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/EPYCD/veans/main/install.sh | sh
#
# A token is optional while the repository is public; any of GH_TOKEN,
# GITHUB_TOKEN or an authenticated `gh` CLI will do; a fine-grained token
# needs only "Contents: read" on this one repository.
#
#   GH_TOKEN=ghp_xxx sh install.sh
#
# Environment:
#   VEANS_VERSION  tag to install (default: the latest release)
#   VEANS_BINDIR   install directory (default: /usr/local/bin, or
#                  ~/.local/bin when that is not writable)

set -eu

REPO="${VEANS_REPO:-EPYCD/veans}"
API="https://api.github.com/repos/${REPO}"

die() { printf '\033[31merror\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[36m==\033[0m %s\n' "$*" >&2; }

# ---------------------------------------------------------------- platform
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux) ;;
  darwin) ;;
  msys*|mingw*|cygwin*) die "on Windows use install.ps1, or run this inside WSL2" ;;
  *) die "unsupported OS: $os" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
info "platform ${os}/${arch}"

# ------------------------------------------------------------------- auth
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
if [ -z "$TOKEN" ] && command -v gh >/dev/null 2>&1; then
  TOKEN=$(gh auth token 2>/dev/null || true)
fi
# No token is not an error: the repository is public, so the release API and
# its assets are readable anonymously — just rate-limited by IP. A token is
# still used when present, which raises that limit and keeps this working if
# the repository ever goes private again.

command -v curl >/dev/null 2>&1 || die "curl is required"

# Only send Authorization when there is something to send: an empty Bearer
# is rejected outright, which would be a worse failure than anonymous access.
api() {
  if [ -n "$TOKEN" ]; then
    curl -fsSL -H "Authorization: Bearer ${TOKEN}" \
         -H "X-GitHub-Api-Version: 2022-11-28" \
         -H "Accept: application/vnd.github+json" "$@"
  else
    curl -fsSL -H "X-GitHub-Api-Version: 2022-11-28" \
         -H "Accept: application/vnd.github+json" "$@"
  fi
}

# dl <asset-id> <output-path>
dl() {
  if [ -n "$TOKEN" ]; then
    curl -fsSL -H "Authorization: Bearer ${TOKEN}" -H "Accept: application/octet-stream" \
         "${API}/releases/assets/$1" -o "$2"
  else
    curl -fsSL -H "Accept: application/octet-stream" \
         "${API}/releases/assets/$1" -o "$2"
  fi
}

# --------------------------------------------------------------- resolve
if [ -n "${VEANS_VERSION:-}" ]; then
  rel=$(api "${API}/releases/tags/${VEANS_VERSION}") \
    || die "no release tagged ${VEANS_VERSION}"
else
  rel=$(api "${API}/releases/latest") \
    || die "cannot read releases from ${REPO}"
fi

version=$(printf '%s' "$rel" | sed -n 's/.*"tag_name"[ ]*:[ ]*"\([^"]*\)".*/\1/p' | head -1)
[ -n "$version" ] || die "could not determine the release tag"
asset="veans_${version}_${os}_${arch}.tar.gz"
info "release ${version}"

# GitHub returns PRETTY-PRINTED JSON, so an asset's "id" and "name" sit on
# different lines. Splitting on '{' alone therefore leaves grep holding a line
# that has the name and no id, and the sed below finds nothing — asset_id comes
# back empty and every install dies with "has no asset named ...".
#
# Collapse the newlines, and the spaces around each colon, so that splitting on
# '{' puts one asset object on one line with both fields on it. Handles compact
# JSON too, which is what this originally assumed.
json=$(printf '%s' "$rel" | tr -d '\r\n' | sed 's/"[[:space:]]*:[[:space:]]*/":/g')

# asset_id_of NAME -> the numeric id of the release asset called NAME.
asset_id_of() {
  printf '%s' "$json" \
    | tr '{' '\n' \
    | grep -F "\"name\":\"$1\"" \
    | sed -n 's/.*"id":\([0-9]*\).*/\1/p' \
    | head -1
}

# Assets are fetched by id with an octet-stream Accept header. That path works
# for public and private repositories alike, unlike browser_download_url.
asset_id=$(asset_id_of "$asset")
[ -n "$asset_id" ] || die "release ${version} has no asset named ${asset}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading ${asset}"
dl "$asset_id" "${tmp}/${asset}" || die "download failed"

# -------------------------------------------------------------- checksum
sum_id=$(asset_id_of checksums.txt)
if [ -n "$sum_id" ]; then
  dl "$sum_id" "${tmp}/checksums.txt" || true
fi
if [ -s "${tmp}/checksums.txt" ]; then
  want=$(grep -F " ${asset}" "${tmp}/checksums.txt" | awk '{print $1}' | head -1)
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
  else
    got=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
  fi
  [ -n "$want" ] || die "checksums.txt has no entry for ${asset}"
  [ "$want" = "$got" ] || die "checksum mismatch for ${asset} — refusing to install"
  info "checksum verified"
else
  info "no checksums.txt in the release; skipping verification"
fi

# --------------------------------------------------------------- install
tar -xzf "${tmp}/${asset}" -C "$tmp"

if [ -n "${VEANS_BINDIR:-}" ]; then
  bindir="$VEANS_BINDIR"
elif [ -w /usr/local/bin ]; then
  bindir=/usr/local/bin
elif command -v sudo >/dev/null 2>&1 && [ -d /usr/local/bin ]; then
  bindir=/usr/local/bin
  SUDO=sudo
else
  bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir" 2>/dev/null || ${SUDO:-} mkdir -p "$bindir"

for c in veans marshal; do
  ${SUDO:-} install -m 755 "${tmp}/${c}" "${bindir}/${c}"
done

# macOS refuses to run a quarantined binary; these are ad-hoc signed by the
# Go linker but not notarised, so strip the flag if the download set one.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  ${SUDO:-} xattr -d com.apple.quarantine "${bindir}/veans" "${bindir}/marshal" 2>/dev/null || true
fi

info "installed veans and marshal to ${bindir}"
case ":${PATH}:" in
  *":${bindir}:"*) ;;
  *) info "note: ${bindir} is not on PATH — add it to your shell profile" ;;
esac

"${bindir}/veans" version || true
"${bindir}/marshal" --version || true

cat >&2 <<'NEXT'

Next: from inside the repository you want coordinated, run

  veans onboard --server https://your-board.example.com

That creates the project, the bots and every file the repo needs.
NEXT
