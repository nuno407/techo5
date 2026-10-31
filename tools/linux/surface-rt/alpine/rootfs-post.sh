#!/bin/sh
# rootfs-post.sh <root>: the Surface RT's last changes to the root filesystem, run by mkrootfs.sh
# (staged as tools/rootfs-post.sh) before it packs the tarball.
set -e
R=$1
[ -d "$R/etc" ] || { echo "rootfs-post: no root at $R" >&2; exit 1; }

# admin: the account postmarketOS had (uid 1001, wheel), for SSH with keys only and sudo. No
# password in the image ("*"): install-slot.sh copies the tablet's hash and authorized_keys in.
grep -q '^admin:' "$R/etc/group" || echo 'admin:x:1001:' >> "$R/etc/group"
grep -q '^admin:' "$R/etc/passwd" || echo 'admin:x:1001:1001:Admin:/home/admin:/bin/sh' >> "$R/etc/passwd"
grep -q '^admin:' "$R/etc/shadow" || echo 'admin:*:20000:0:99999:7:::' >> "$R/etc/shadow"
for g in wheel audio video input netdev; do
	grep -q "^$g:" "$R/etc/group" || continue
	grep -Eq "^$g:.*[:,]admin(,|\$)" "$R/etc/group" || sed -i -E "/^$g:/{s/:\$/:admin/;t;s/\$/,admin/}" "$R/etc/group"
done
install -d -m 755 "$R/home"
install -d -m 700 "$R/home/admin" "$R/home/admin/.ssh"
chown -R 1001:1001 "$R/home/admin"
install -d -m 750 "$R/etc/sudoers.d"
printf '%%wheel ALL=(ALL:ALL) ALL\n' > "$R/etc/sudoers.d/wheel"
chmod 440 "$R/etc/sudoers.d/wheel"

# Scripts from the overlay that are not under etc/techo5 or usr/local/sbin.
chmod 644 "$R/etc/mdev.conf" "$R/etc/modules" "$R/etc/fstab" "$R/etc/inittab"

# The kernel's modprobe (request_module) and mdev's: kmod's, which reads .ko.zst.
[ "$(readlink "$R/sbin/modprobe")" != /bin/busybox ] || { echo "rootfs-post: /sbin/modprobe is still busybox's" >&2; exit 1; }
# The kernel image's modules (the tarball build-rootfs.sh stages as inputs/kernel.tar.gz).
ls -d "$R"/lib/modules/*/modules.dep >/dev/null 2>&1 || { echo "rootfs-post: no kernel modules" >&2; exit 1; }

# No firmware in the image: install-slot.sh copies the tablet's /lib/firmware (the BT_UART firmware
# in it is not ours to publish).
rm -rf "$R/lib/firmware"
install -d -m 755 "$R/lib/firmware"

# bluetoothd (bluez 5.86) counts every [BR]/[LE] key of main.conf as a default to send, set or not,
# and with none set sends nothing and logs "Failed to set default system config for hci0". Two of
# the kernel's own LE defaults (le_scan_int_connect, le_scan_window_connect: 0x60) said out loud
# give it a real list; nothing changes on the air.
bc="$R/etc/bluetooth/main.conf"
if [ -f "$bc" ] && ! grep -q '^ScanIntervalAutoConnect=' "$bc"; then
	grep -q '^\[LE\]$' "$bc" || printf '\n[LE]\n' >> "$bc"
	sed -i 's/^\[LE\]$/[LE]\nScanIntervalAutoConnect=96\nScanWindowAutoConnect=96/' "$bc"
fi
grep -q '^ScanIntervalAutoConnect=96$' "$bc" || { echo "rootfs-post: no LE defaults in $bc" >&2; exit 1; }

# shairport-sync's D-Bus policies (etc/dbus-1/system.d) name its system user, which the package's
# install script makes and this image's passwd lacks: dbus-daemon said "Unknown username" twice
# at every start.
grep -q '^shairport-sync:' "$R/etc/group" || echo 'shairport-sync:x:180:' >> "$R/etc/group"
grep -q '^shairport-sync:' "$R/etc/passwd" ||
	echo 'shairport-sync:x:180:180:shairport-sync:/var/empty:/sbin/nologin' >> "$R/etc/passwd"
grep -q '^shairport-sync:' "$R/etc/shadow" || echo 'shairport-sync:!::0:::::' >> "$R/etc/shadow"
