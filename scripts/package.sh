#!/usr/bin/env bash
# Build release archives on Linux. Prerequisites: Go, musl C toolchain,
# GNU tar/coreutils, readelf and flock. For cross builds, for example:
# ARCHES='amd64 arm64' CC_AMD64=musl-gcc CC_ARM64=aarch64-linux-musl-gcc \
#   ./scripts/package.sh 0.1.0
set -euo pipefail
export LC_ALL=C
umask 022

die() { echo "package: $*" >&2; exit 1; }
if [[ $# -lt 1 || $# -gt 2 ]]; then die "usage: $0 VERSION [OUTDIR]"; fi
version=$1
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z][0-9A-Za-z.+-]*)?$ ]] ||
    die "use a numeric version, for example 0.1.0 or 0.2.0-rc.1"
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p -- "${2:-$root/dist}"
out=$(cd -- "${2:-$root/dist}" && pwd)
# Creating both archives from the same clean source tree makes the published
# build inputs match the binaries and keeps local embedded assets out.
stage=$(mktemp -d)
publish=$(mktemp -d "$out/.release-XXXXXXXX")
trap 'rm -rf -- "$stage" "$publish"' EXIT
stem="emailrelay-$version"
source_dir="$stage/$stem-source"
go=${GO:-go}
epoch=${SOURCE_DATE_EPOCH:-0}
[[ $epoch =~ ^[0-9]+$ ]] || die "invalid SOURCE_DATE_EPOCH"
host=$("$go" env GOHOSTARCH)
read -r -a arches <<< "${ARCHES:-$host}"
[[ ${#arches[@]} -gt 0 ]] || die "ARCHES is empty"
echo "Packaging EmailRelay $version for Linux (${arches[*]})"

copy_source() {
    local path=$1
    [[ -f $root/$path && ! -L $root/$path ]] || die "missing or non-regular source file: $path"
    install -D -m "${2:-0644}" -- "$root/$path" "$source_dir/$path"
}
for path in README.md README.ko.md LICENSE go.mod go.sum .gitignore Dockerfile .dockerignore \
    compose.yaml compose.https.yaml .env.example deploy/portable/Caddyfile \
    deploy/emailrelay.service internal/webui/assets/{index.html,app.js,app.css,NanumGothic-Regular.woff2,NanumGothic-OFL.txt}; do
    copy_source "$path"
done
copy_source scripts/package.sh 0755
cd "$root"
while IFS= read -r -d '' path; do copy_source "$path"; done \
    < <(find cmd internal integration -type f -name '*.go' -print0)
for path in deploy/licenses/*.txt; do copy_source "$path"; done

archive() {
    local name=$1
    tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
        -C "$stage" -cf - "$name" | gzip -n > "$publish/$name.tar.gz"
}
archive "$stem-source"
cd "$source_dir"
for arch in "${arches[@]}"; do
    case $arch in
        amd64) cc=${CC_AMD64:-}; expected_machine='X86-64' ;;
        arm64) cc=${CC_ARM64:-}; expected_machine='AArch64' ;;
        *) die "unsupported architecture: $arch" ;;
    esac
    if [[ -z $cc && $arch == "$host" ]]; then
        cc=${CC:-$(command -v musl-gcc || command -v cc)}
    fi
    [[ -n $cc ]] || die "set CC_${arch^^} to the target musl C compiler"
    # Check the compiler target independently of the builder host. This also
    # catches an accidentally selected glibc toolchain before a costly build.
    printf 'int main(void) { return 0; }\n' | "$cc" -x c - -o "$stage/cc-probe"
    readelf -lW "$stage/cc-probe" | grep 'interpreter: .*ld-musl-' >/dev/null ||
        die "$cc does not target musl"

    name="$stem-linux-$arch"
    dest="$stage/$name"
    mkdir -p "$dest/bin" "$dest/deploy"
    CGO_ENABLED=1 GOOS=linux GOARCH="$arch" CC="$cc" "$go" build \
        -mod=readonly -buildvcs=false -trimpath -tags 'netgo osusergo sqlite_omit_load_extension' \
        -ldflags "-s -w -X=main.version=$version -linkmode=external -extldflags=-static" \
        -o "$dest/bin/emailrelay" ./cmd/emailrelay
    readelf -hW "$dest/bin/emailrelay" | grep "Machine:.*$expected_machine" >/dev/null ||
        die "wrong machine type for $arch"
    if readelf -lWd "$dest/bin/emailrelay" | grep -E 'INTERP|\(NEEDED\)' >/dev/null; then
        die "$arch executable has shared-library dependencies"
    fi
    cp README.md README.ko.md LICENSE "$dest/"
    cp deploy/emailrelay.service "$dest/deploy/"
    cp -R deploy/licenses "$dest/THIRD_PARTY"
    archive "$name"
done

cd "$publish"
sha256sum ./*.tar.gz > SHA256SUMS
sha256sum -c SHA256SUMS
# Publish only after all target builds and verification succeeded. The checksum
# file is installed last so consumers can verify the entire set.
exec 9<"$out"
flock 9
for file in *.tar.gz SHA256SUMS; do mv -fT -- "$file" "$out/$file"; done
echo "Archives and SHA256SUMS written to $out"
