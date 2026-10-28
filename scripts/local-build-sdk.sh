#!/bin/sh
# Build fencebreaker OpenWrt packages locally using the official SDK docker
# image. The openwrt/sdk image is a builder environment: the SDK itself is
# downloaded inside the container by /builder/setup.sh (env-driven). Built
# .apk files and the apk repository index are copied to out/apk/.
#
# Usage: scripts/local-build-sdk.sh [package ...]
#   package defaults to: fencebreaker fencebreaker-coredns luci-app-fencebreaker
#   Override with SDK_IMAGE / SDK_VERSION_PATH / SDK_TARGET.
set -eu

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${SDK_IMAGE:-openwrt/sdk:x86-64-openwrt-25.12}"
VERSION_PATH="${SDK_VERSION_PATH:-releases/25.12.0}"
TARGET="${SDK_TARGET:-x86/64}"

if [ "$#" -gt 0 ]; then
	FB_PACKAGES="$*"
else
	FB_PACKAGES="fencebreaker fencebreaker-coredns luci-app-fencebreaker"
fi

echo "image:    $IMAGE"
echo "sdk:      $VERSION_PATH / $TARGET"
echo "packages: $FB_PACKAGES"
echo "output:   $REPO_DIR/out/apk"

mkdir -p "$REPO_DIR/out/apk"

# entry.sh chowns /builder and drops to user buildbot for the given command.
# setup.sh fetches and unpacks the SDK tarball into /builder. The feed root
# (/fb) is this repository: it already has the net/ and luci/ layout that
# scripts/feeds expects. Produced files are chown'ed back to the invoking
# user at the end. A named docker volume persists /builder across runs so the
# SDK and its toolchain downloads are not repeated (SDK_CACHE=0 disables).
# shellcheck disable=SC2016 # the inner script is expanded inside the container
VOL_ARGS=""
if [ "${SDK_CACHE:-1}" = 1 ]; then
	VOL_ARGS="-v fencebreaker-sdk-builder:/builder"
fi

# shellcheck disable=SC2086 # $VOL_ARGS carries 0..2 already-split docker args
docker run --rm \
	$VOL_ARGS \
	-e TARGET="$TARGET" \
	-e VERSION_PATH="$VERSION_PATH" \
	-e DOWNLOAD_FILE='sdk-.*x86_64.tar.[xz|zst]' \
	-e FB_PACKAGES="$FB_PACKAGES" \
	-e HOST_UID="$(id -u)" \
	-e HOST_GID="$(id -g)" \
	-v "$REPO_DIR":/fb \
	"$IMAGE" /bin/bash -eu -c '
		cd /builder
		if [ ! -f rules.mk ]; then
			./setup.sh
		fi
		test -f rules.mk

		# feeds.conf must keep the feeds.conf.default entries (packages/luci
		# feeds) — writing only our line would disable them; keep exactly one
		# fencebreaker line (runs share a persistent SDK volume)
		[ -f feeds.conf ] || cp feeds.conf.default feeds.conf
		sed -i "/^src-link fencebreaker /d" feeds.conf
		echo "src-link fencebreaker /fb" >> feeds.conf
		./scripts/feeds update -a
		./scripts/feeds install -a

		# shellcheck disable=SC2086 # intentional word splitting
		for pkg in $FB_PACKAGES; do
			echo "CONFIG_PACKAGE_${pkg}=m" >> .config
		done
		make defconfig

		TARGETS=""
		# shellcheck disable=SC2086 # intentional word splitting
		for pkg in $FB_PACKAGES; do
			TARGETS="$TARGETS package/$pkg/compile"
		done
		# shellcheck disable=SC2086 # intentional word splitting
		make $TARGETS -j"$(nproc)" V=s

		sh /fb/scripts/make-apk-repo.sh /builder/bin/packages ||
			echo "WARNING: apk index creation failed, copying raw .apk files only" >&2

		# copy our feed packages plus the small APKINDEX files of every
		# built directory, not the whole dependency firmware ballast
		mkdir -p /fb/out/apk
		find bin/packages -type f -path "*/fencebreaker/*.apk" \
			-exec cp --parents {} /fb/out/apk/ \;
		find bin/packages -name "APKINDEX.tar.gz" -exec cp --parents {} /fb/out/apk/ \;
		chown -R "$HOST_UID:$HOST_GID" /fb/out 2>/dev/null ||
			chmod -R a+rwX /fb/out
	'

echo
echo "Build finished. Artifacts:"
find "$REPO_DIR/out/apk" -type f | sort
