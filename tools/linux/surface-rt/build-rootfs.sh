#!/bin/sh
# build-rootfs.sh: TECHO5's Alpine root filesystem for the Surface RT, as a tarball for
# install-slot.sh. Runs where Docker can run an armv7 Alpine container (the Mac with OrbStack, or
# Linux with qemu-user binfmt): mkrootfs.sh runs inside it with Alpine's own apk, so packages and
# their triggers install as on a device, and the files are root's without any user namespace.
#
#   sh tools/linux/surface-rt/build-rootfs.sh [-o out.tar.gz]
#
# Environment (defaults in brackets):
#   SRT_REPO     the surface_rt project: slotctl, srt-healthy, srt-audio-defaults, the modules
#                [~/personal_repos/surface_rt]
#   DAEMON       the daemon, armv7 [bin/echod-arm; build it with
#                (cd echod && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o ../bin/echod-arm ./cmd/echod)]
#   MODULES      the kernel's modules tarball [$SRT_REPO/images/modules-7.2.13-surface-18.tar.gz]
#   INPUTS       alpine-minirootfs-*-armv7.tar.gz and models/ [inputs]
#   ALPINE_IMAGE the build container [alpine:3.24]
#   TZ_NAME      the default zone [Europe/Lisbon]
#
# The tarball carries no firmware and no secrets: no Wi-Fi credentials, keys or password hashes.
# install-slot.sh adds the tablet's own on the tablet.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
SRT_REPO=${SRT_REPO:-$HOME/personal_repos/surface_rt}
OUT=$SRT_REPO/images/alpine-surface-rt-rootfs.tar.gz
while [ $# -gt 0 ]; do
	case $1 in
	-o) OUT=$2; shift 2 ;;
	*) echo "usage: build-rootfs.sh [-o out.tar.gz]" >&2; exit 1 ;;
	esac
done
DAEMON=${DAEMON:-$ROOT/bin/echod-arm}
MODULES=${MODULES:-$SRT_REPO/images/modules-7.2.13-surface-18.tar.gz}
INPUTS=${INPUTS:-$ROOT/inputs}
ALPINE_IMAGE=${ALPINE_IMAGE:-alpine:3.24}
TZ_NAME=${TZ_NAME:-Europe/Lisbon}

for f in "$DAEMON" "$MODULES" "$SRT_REPO/tools/slotctl" "$SRT_REPO/tools/srt-healthy" "$SRT_REPO/tools/srt-audio-defaults"; do
	[ -f "$f" ] || { echo "build-rootfs: missing $f" >&2; exit 1; }
done
file "$DAEMON" 2>/dev/null | grep -q 'ARM, EABI5' || { echo "build-rootfs: $DAEMON is not an armv7 binary" >&2; exit 1; }
# The newest minirootfs only: mkrootfs.sh takes exactly one.
MINI=$(ls "$INPUTS"/alpine-minirootfs-*-armv7.tar.gz 2>/dev/null | sort -t- -k3 -V | tail -1)
[ -n "$MINI" ] || { echo "build-rootfs: no $INPUTS/alpine-minirootfs-*-armv7.tar.gz" >&2; exit 1; }

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/srt-rootfs-stage.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/bin" "$STAGE/tools" "$STAGE/overlay" "$STAGE/inputs" "$STAGE/out"
cp "$DAEMON" "$STAGE/bin/techo5"
L=$ROOT/tools/linux
cp "$L/slotctl" "$L/bootctl" "$L/techo5-lib.sh" "$L/mkrootfs.sh" "$STAGE/tools/"
{ cat "$L/packages-rootfs.txt"; echo; cat "$HERE/alpine/packages.txt"; } > "$STAGE/tools/packages-rootfs.txt"
cp "$HERE/alpine/rootfs-post.sh" "$STAGE/tools/rootfs-post.sh"
# The Echos' overlay, the Surface RT's on top, and the surface_rt project's tools (its slotctl
# replaces TECHO5's, which mkrootfs.sh installs first).
cp -R "$L/rootfs/." "$STAGE/overlay/"
cp -R "$HERE/alpine/overlay/." "$STAGE/overlay/"
mkdir -p "$STAGE/overlay/usr/local/sbin"
for t in slotctl srt-healthy srt-audio-defaults; do
	cp "$SRT_REPO/tools/$t" "$STAGE/overlay/usr/local/sbin/$t"
done
if ls "$INPUTS"/models/*.tflite >/dev/null 2>&1; then
	mkdir -p "$STAGE/overlay/usr/share/techo5/models"
	cp "$INPUTS"/models/*.tflite "$INPUTS"/models/*.json "$STAGE/overlay/usr/share/techo5/models/"
else
	echo "build-rootfs: no wake word models in $INPUTS/models; the tablet keeps the ones in /data" >&2
fi
cp "$MINI" "$STAGE/inputs/"
cp "$MODULES" "$STAGE/inputs/kernel.tar.gz"
# macOS's extended attributes would land in the tarball as ._ files.
find "$STAGE" -name '._*' -delete 2>/dev/null || true

VERSION="surface-rt-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)$(git -C "$ROOT" diff --quiet 2>/dev/null || echo -dirty)-$(date -u +%Y%m%d%H%M)"
echo "build-rootfs: $VERSION from $(basename "$MINI") in $ALPINE_IMAGE (linux/arm/v7)"
docker run --rm --platform linux/arm/v7 -v "$STAGE:/stage" "$ALPINE_IMAGE" \
	sh /stage/tools/mkrootfs.sh -i /stage -o /stage/out/rootfs.tar.gz -w /work -V "$VERSION" -z "$TZ_NAME"
mkdir -p "$(dirname "$OUT")"
cp "$STAGE/out/rootfs.tar.gz" "$OUT"
echo "build-rootfs: $OUT ($(du -h "$OUT" | cut -f1))"
