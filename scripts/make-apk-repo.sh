#!/bin/sh
# Build APKINDEX.tar.gz for every directory under DIR that contains *.apk
# files. Optionally sign the index with APK_SIGN_KEY (path to a private PEM
# key).
#
# Usage: scripts/make-apk-repo.sh <dir-with-apk-files>
#
# The apk binary is looked up in PATH first, then under ./staging_dir (the
# OpenWrt SDK layout, so run this from the SDK root or pass an explicit path
# via APK_BIN=<path>).
set -eu

DIR="${1:-}"
[ -n "$DIR" ] || {
	echo "usage: $0 <dir-with-apk-files>" >&2
	exit 2
}
[ -d "$DIR" ] || {
	echo "error: not a directory: $DIR" >&2
	exit 2
}

find_apk_bin() {
	if command -v apk >/dev/null 2>&1; then
		command -v apk
		return 0
	fi
	if [ -d ./staging_dir ]; then
		# head -n1 instead of find -quit for busybox find compatibility
		found="$(find ./staging_dir -type f -name apk 2>/dev/null | head -n 1 || true)"
		if [ -n "$found" ]; then
			echo "$found"
			return 0
		fi
	fi
	return 1
}

if [ -n "${APK_BIN:-}" ]; then
	[ -x "$APK_BIN" ] || {
		echo "error: APK_BIN is not executable: $APK_BIN" >&2
		exit 2
	}
	APK="$APK_BIN"
else
	APK="$(find_apk_bin)" || {
		echo "error: apk binary not found in PATH or ./staging_dir;" \
			"set APK_BIN=<path> (it may live in staging_dir/host/bin" \
			"or staging_dir/hostpkg/bin of an OpenWrt SDK)" >&2
		exit 1
	}
fi
echo "using apk: $APK"

# One index per directory containing .apk files: OpenWrt places built
# packages into bin/packages/<arch>/<feed>/..., and an apk repository is one
# index per such directory.
dirs="$(find "$DIR" -type f -name '*.apk' -exec dirname {} \; | sort -u)"
[ -n "$dirs" ] || {
	echo "error: no *.apk files under $DIR" >&2
	exit 1
}

for d in $dirs; do
	set -- "$d"/*.apk
	if [ ! -f "$1" ]; then
		echo "error: glob did not match .apk files in $d" >&2
		exit 1
	fi

	idx="$d/APKINDEX.tar.gz"
	rm -f "$idx"

	# apk-tools 3 (OpenWrt 25.12): packages are v3, the repository index is
	# built with `mkndx` and --allow-untrusted is a global option. Fall back
	# to the v2 `index` applet for older toolchains.
	if "$APK" --allow-untrusted mkndx --output "$idx" "$@" 2>/dev/null; then
		:
	else
		echo "note: 'apk mkndx' failed, retrying with v2 'apk index'" >&2
		"$APK" index --allow-untrusted -o "$idx" "$@" ||
			"$APK" index -o "$idx" "$@"
	fi

	if [ -n "${APK_SIGN_KEY:-}" ]; then
		# mkndx embeds the signature into the index when given a key
		"$APK" --allow-untrusted mkndx --output "$idx" --sign-key "$APK_SIGN_KEY" "$@" ||
			openssl dgst -sha256 -sign "$APK_SIGN_KEY" -out "$idx.sig" "$idx"
	fi

	echo "index: $idx"
done
