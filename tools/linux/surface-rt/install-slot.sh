#!/bin/sh
# install-slot.sh: put TECHO5's Alpine root filesystem (build-rootfs.sh) into a Surface RT slot and
# put that slot on trial. Run ON the tablet, as root, from the running (good) slot:
#
#   sh install-slot.sh <a|b> [options]
#     -t <rootfs.tar.gz>  the image [alpine-surface-rt-rootfs.tar.gz next to this script, else /tmp]
#     -k <vmlinuz>        kernel   [the booted slot's, /boot/<slot>/vmlinuz]
#     -d <dtb>            dtb      [the booted slot's, /boot/<slot>/dtb]
#     -n                  dry run: check everything and say what would be done; change nothing
#     --wifi              write /data/techo5-linux/wpa_supplicant.conf again even if it has a network
#     --no-trial          stop after set-kernel (the slot is installed but not booted)
#
# What it changes:
#   - the target slot's partition (/dev/mmcblk0p2 for a, p3 for b), and only that one: a new ext4
#     (label SRT_ROOT_<X>, the partition UUID stays), the image unpacked into it, then the unit's own
#     files: kernel modules if the image has none for the kernel, /lib/firmware (with the private
#     BT_UART firmware) from the running system, admin's authorized_keys and password hash;
#   - /data/techo5-linux (shared by both slots): the Wi-Fi configuration from NetworkManager's active
#     connection (written only on the tablet, mode 600, the key never printed), the MAC NetworkManager
#     uses (so the DHCP lease stays), dropbear host keys converted from /etc/ssh (so the host key stays);
#   - /boot/<X> (the slot's kernel, no initramfs) and the loader entries, through slotctl;
#   - /usr/local/sbin/slotctl and bootctl of the running system: the image's, which know slots without
#     an initramfs and keep the slots' state as TECHO5's boot record (bootctl's systemd-boot backend;
#     the old slotctl is kept as slotctl.pre-alpine). The running system's slot-commit runs them.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

die() { echo "install-slot: $*" >&2; exit 1; }
say() { echo "install-slot: $*"; }
DRY=; WIFI_FORCE=; TRIAL=1; TARBALL=; KERNEL=; DTB=
S=${1:-}; [ $# -gt 0 ] && shift
while [ $# -gt 0 ]; do
	case $1 in
	-n|--dry-run) DRY=1; shift ;;
	-t) TARBALL=$2; shift 2 ;;
	-k) KERNEL=$2; shift 2 ;;
	-d) DTB=$2; shift 2 ;;
	--wifi) WIFI_FORCE=1; shift ;;
	--no-trial) TRIAL=; shift ;;
	*) die "unknown option $1" ;;
	esac
done
# run: do it, or in a dry run say it
run() { if [ -n "$DRY" ]; then echo "  would: $*"; else "$@"; fi; }

case $S in a) DEV=/dev/mmcblk0p2 ;; b) DEV=/dev/mmcblk0p3 ;; *) die "usage: install-slot.sh <a|b> [-t rootfs.tar.gz] [-k vmlinuz] [-d dtb] [-n] [--wifi] [--no-trial]" ;; esac
X=$(echo "$S" | tr ab AB)
[ "$(id -u)" = 0 ] || die "run as root"

# --- Checks (all of them before anything changes).
d=$(mountpoint -d / 2>/dev/null || true)
BOOTED=
[ "$d" = "$(cat /sys/class/block/mmcblk0p2/dev 2>/dev/null)" ] && BOOTED=a
[ "$d" = "$(cat /sys/class/block/mmcblk0p3/dev 2>/dev/null)" ] && BOOTED=b
# check-install.sh runs all of this in a container, on a loop device, as if slot $SRT_TEST_BOOTED ran
[ -n "${SRT_TEST_DEV:-}" ] && { DEV=$SRT_TEST_DEV; BOOTED=$SRT_TEST_BOOTED; }
[ -n "$BOOTED" ] || die "not running from an eMMC slot"
[ "$BOOTED" != "$S" ] || die "slot $S is the running one; install into the other"
B=$(echo "$BOOTED" | tr ab AB)
[ -b "$DEV" ] || die "no $DEV"
grep -q "^$DEV " /proc/mounts && die "$DEV is mounted"
grep -q "^$DEV " /proc/swaps 2>/dev/null && die "$DEV is swap"
mountpoint -q /boot && [ -d /boot/loader/entries ] || die "/boot (the ESP) is not mounted"
mountpoint -q /data || die "/data is not mounted"
here=$(cd "$(dirname "$0")" && pwd)
if [ -z "$TARBALL" ]; then
	for t in "$here/alpine-surface-rt-rootfs.tar.gz" /tmp/alpine-surface-rt-rootfs.tar.gz; do
		[ -f "$t" ] && { TARBALL=$t; break; }
	done
fi
[ -n "$TARBALL" ] && [ -f "$TARBALL" ] || die "no rootfs tarball (-t)"
KERNEL=${KERNEL:-/boot/$B/vmlinuz}
DTB=${DTB:-/boot/$B/dtb}
[ -f "$KERNEL" ] || die "no kernel $KERNEL"
[ -f "$DTB" ] || die "no dtb $DTB"
say "checking $TARBALL"
gzip -t "$TARBALL" || die "$TARBALL is damaged"
TMP=$(mktemp -d /tmp/install-slot.XXXXXX)
MNT=/run/install-slot-$S
cleanup() {
	mountpoint -q "$MNT/dev" 2>/dev/null && umount "$MNT/dev"
	mountpoint -q "$MNT" 2>/dev/null && umount "$MNT"
	rm -rf "$TMP"
}
trap cleanup EXIT
# The image's slotctl: it writes the entry of a slot without an initramfs.
tar -xzOf "$TARBALL" ./usr/local/sbin/slotctl > "$TMP/slotctl" || die "the image has no usr/local/sbin/slotctl"
grep -q 'rootwait' "$TMP/slotctl" || die "the image's slotctl does not know slots without an initramfs"
chmod 755 "$TMP/slotctl"
# Its bootctl, beside it: the only writer of the slots' state (TECHO5's boot contract).
tar -xzOf "$TARBALL" ./usr/local/sbin/bootctl > "$TMP/bootctl" || die "the image has no usr/local/sbin/bootctl"
grep -q 'ab.slot=' "$TMP/slotctl" && grep -q 'systemd-boot' "$TMP/bootctl" || die "the image's slotctl or bootctl does not keep TECHO5's boot record"
chmod 755 "$TMP/bootctl"
kmods=$(tar -tzf "$TARBALL" | sed -n 's#^\./lib/modules/\([^/]*\)/modules\.dep$#\1#p' | head -1)
krel=$(uname -r)
if [ "$KERNEL" = "/boot/$B/vmlinuz" ] && [ "$kmods" != "$krel" ]; then
	say "the image's modules are for '${kmods:-none}', the kernel is $krel: the running system's go in instead"
fi
fw_bt=/lib/firmware/updates/mrvl/sd8797_uapsta.bin
[ -f $fw_bt ] || say "warning: no $fw_bt (BT_UART firmware): Bluetooth will not come up"
# The other slot must be good: it is what a failed trial falls back to.
"$TMP/slotctl" status | grep -q "^slot $BOOTED: good" || die "slot $BOOTED (running) is not good: nothing to fall back to"

# Wi-Fi from NetworkManager's active connection, unless /data already has a network.
WPA=/data/techo5-linux/wpa_supplicant.conf
WIFI=
if [ -n "$WIFI_FORCE" ] || ! grep -q '^network={' "$WPA" 2>/dev/null; then
	command -v nmcli >/dev/null || die "no nmcli to read the Wi-Fi connection from (or write $WPA by hand)"
	con=$(nmcli -t -e no -f NAME,TYPE con show --active | sed -n 's/^\(.*\):802-11-wireless$/\1/p' | head -1)
	[ -n "$con" ] || die "no active Wi-Fi connection in NetworkManager"
	km=$(nmcli -e no -g 802-11-wireless-security.key-mgmt con show "$con")
	[ "$km" = wpa-psk ] || die "Wi-Fi connection '$con' uses '$km'; only wpa-psk is handled (write $WPA by hand)"
	ssid=$(nmcli -e no -g 802-11-wireless.ssid con show "$con")
	hidden=$(nmcli -e no -g 802-11-wireless.hidden con show "$con")
	# WPA's PSK is PBKDF2-HMAC-SHA1(passphrase, SSID, 4096, 32), what wpa_passphrase prints. The
	# passphrase goes from nmcli to python through a pipe: never on a command line, in a file or on
	# the screen (wpa_passphrase reads stdin only from a tty).
	psk=$(nmcli -e no -s -g 802-11-wireless-security.psk con show "$con" 2>/dev/null | SSID=$ssid python3 -I -c 'import hashlib, os, re, sys
p = sys.stdin.buffer.read().rstrip(b"\n")
if re.fullmatch(rb"[0-9a-fA-F]{64}", p): print(p.decode().lower())
elif 8 <= len(p) <= 63: print(hashlib.pbkdf2_hmac("sha1", p, os.environ["SSID"].encode(), 4096, 32).hex())') || true
	[ ${#psk} -eq 64 ] || die "no usable PSK for '$con' from NetworkManager"
	umask 077
	{
		echo "ctrl_interface=/run/wpa"
		echo "update_config=0"
		echo "network={"
		echo "	ssid=$(printf %s "$ssid" | od -An -tx1 -v | tr -d ' \n')"
		[ "$hidden" = yes ] && echo "	scan_ssid=1"
		echo "	key_mgmt=WPA-PSK"
		echo "	psk=$psk"
		echo "}"
	} > "$TMP/wpa.conf"
	umask 022
	unset psk
	WIFI=$con
	say "Wi-Fi: '$con' ($km), configuration made; the key stays on the tablet"
else
	say "Wi-Fi: $WPA already has a network; kept (--wifi to write it again)"
fi
say "slot $S ($DEV) from slot $BOOTED; kernel $KERNEL, dtb $DTB${DRY:+ — dry run, nothing changes}"

# --- The slot's file system.
# Bad before its files change, so it is never booted half written (TECHO5's slotctl install).
run "$TMP/slotctl" mark-bad "$S"
run mkfs.ext4 -q -F -L "SRT_ROOT_$X" "$DEV"
if [ -n "$DRY" ]; then
	say "dry run: the image would be unpacked into $DEV; the unit's files added:"
	echo "  modules: $([ "$kmods" = "$krel" ] && echo "the image's ($kmods)" || echo "/lib/modules/$krel")"
	echo "  firmware: /lib/firmware ($(du -sh /lib/firmware | cut -f1))"
	echo "  admin: $(wc -l < /home/admin/.ssh/authorized_keys) authorized key(s), password hash from /etc/shadow"
	echo "  dropbear host keys: $(ls /data/techo5-linux/dropbear/dropbear_*_host_key 2>/dev/null | wc -l) on /data; from /etc/ssh: $(ls /etc/ssh/ssh_host_*_key 2>/dev/null | tr '\n' ' ')"
	echo "  wifi-mac: $(cat /sys/class/net/mlan0/address 2>/dev/null || echo '?')"
	if [ -n "$WIFI" ]; then
		echo "  wifi: $WPA from '$WIFI' ($(grep -c . "$TMP/wpa.conf") lines, $(grep -Ec '^	psk=[0-9a-f]{64}$' "$TMP/wpa.conf") hashed PSK)"
	fi
	run "$TMP/slotctl" set-kernel "$S" "$KERNEL" - "$DTB"
	run install -m 755 "$TMP/slotctl" "$TMP/bootctl" /usr/local/sbin/
	[ -n "$TRIAL" ] && run slotctl trial "$S"
	sh -n "$TMP/slotctl" && say "dry run done: all checks passed"
	exit 0
fi
mkdir -p "$MNT"
mount -t ext4 "$DEV" "$MNT"
say "unpacking"
tar --numeric-owner -xzpf "$TARBALL" -C "$MNT"
[ -x "$MNT/sbin/init" ] && [ -x "$MNT/etc/techo5/srt-early.sh" ] || die "the image is not a TECHO5 Surface RT root"

# Kernel modules: the image's when they are for this kernel, else the running system's.
if [ ! -f "$MNT/lib/modules/$krel/modules.dep" ] && [ "$KERNEL" = "/boot/$B/vmlinuz" ]; then
	say "modules: copying /lib/modules/$krel"
	cp -a "/lib/modules/$krel" "$MNT/lib/modules/"
fi
[ -f "$MNT/lib/modules/$krel/modules.dep" ] || [ "$KERNEL" != "/boot/$B/vmlinuz" ] || die "no modules for $krel"

# Firmware: the running system's, the private BT_UART firmware with it.
say "firmware: /lib/firmware"
mkdir -p "$MNT/lib/firmware"
cp -a /lib/firmware/. "$MNT/lib/firmware/"

# admin: the same keys and password (sudo) as here. Never printed.
install -d -m 700 -o 1001 -g 1001 "$MNT/home/admin/.ssh"
install -m 600 -o 1001 -g 1001 /home/admin/.ssh/authorized_keys "$MNT/home/admin/.ssh/authorized_keys"
h=$(awk -F: '$1 == "admin" { print $2 }' /etc/shadow)
# musl's crypt (Alpine's sudo) reads these; yescrypt ($y$) it does not
case $h in '$6$'*|'$5$'*|'$2'[aby]'$'*|'$1$'*) ;; *) die "admin's password hash here is not one musl's crypt reads (sudo would refuse it)" ;; esac
awk -F: -v OFS=: -v h="$h" '$1 == "admin" { $2 = h } { print }' "$MNT/etc/shadow" > "$MNT/etc/shadow.new"
chmod 640 "$MNT/etc/shadow.new"; chown 0:42 "$MNT/etc/shadow.new" 2>/dev/null || true
mv "$MNT/etc/shadow.new" "$MNT/etc/shadow"
unset h
say "admin: authorized_keys and password copied"

# dropbear's host keys on /data, from OpenSSH's here, once: the tablet keeps its host key.
DB=/data/techo5-linux/dropbear
mkdir -p "$DB"; chmod 700 "$DB"
mount --bind /dev "$MNT/dev"
for t in ed25519 ecdsa rsa; do
	[ -s "$DB/dropbear_${t}_host_key" ] && continue
	[ -f "/etc/ssh/ssh_host_${t}_key" ] || continue
	install -m 600 "/etc/ssh/ssh_host_${t}_key" "$MNT/tmp/hk"
	if chroot "$MNT" /usr/bin/dropbearconvert openssh dropbear /tmp/hk /tmp/hk.db >/dev/null 2>&1; then
		install -m 600 "$MNT/tmp/hk.db" "$DB/dropbear_${t}_host_key"
		say "ssh: $t host key converted"
	else
		say "ssh: $t host key not converted; dropbear makes its own"
	fi
	rm -f "$MNT/tmp/hk" "$MNT/tmp/hk.db"
done

# Wi-Fi: the configuration made above.
if [ -n "$WIFI" ]; then
	mv "$TMP/wpa.conf" "$WPA"
	say "Wi-Fi: $WPA written for '$WIFI'"
fi
# The address NetworkManager gives the card on this network (its stable, per-network one): the router
# then hands the Alpine slot the same lease.
if [ ! -s /data/techo5-linux/wifi-mac ] && [ -r /sys/class/net/mlan0/address ]; then
	cat /sys/class/net/mlan0/address > /data/techo5-linux/wifi-mac
	say "Wi-Fi: address $(cat /data/techo5-linux/wifi-mac) kept for wlan0"
fi
umount "$MNT/dev"
sync
umount "$MNT"

# --- Kernel and trial, with the image's slotctl (installed here too: this system's slot-commit
# rewrites the other slot's entry, and the old slotctl would give it an initramfs it does not have).
"$TMP/slotctl" set-kernel "$S" "$KERNEL" - "$DTB"
[ -f /usr/local/sbin/slotctl.pre-alpine ] || cp -p /usr/local/sbin/slotctl /usr/local/sbin/slotctl.pre-alpine
install -m 755 "$TMP/slotctl" "$TMP/bootctl" /usr/local/sbin/
if [ -n "$TRIAL" ]; then
	slotctl trial "$S"
	say "done: slot $S is on trial (3 tries) for the next boot; reboot to try it. Without a gateway within 80 s it is marked bad and reboots into slot $BOOTED; it commits once the daemon has run 5 minutes with a default route."
else
	say "done: slot $S installed, marked bad, not on trial (slotctl trial $S)"
fi
slotctl status
