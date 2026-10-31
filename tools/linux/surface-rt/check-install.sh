#!/bin/sh
# check-install.sh: run install-slot.sh for real against a pretend tablet, in a privileged armv7 Alpine
# container: a loop device as the slot's partition, tmpfs as /boot and /data, NetworkManager, lsblk and
# findmnt as stubs, test keys and a test passphrase. Then look at what it made. Needs Docker that can
# run linux/arm/v7 with --privileged (OrbStack).
#
#   sh tools/linux/surface-rt/check-install.sh [rootfs.tar.gz]
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
TARBALL=$(cd "$(dirname "${1:-$HOME/personal_repos/surface_rt/images/alpine-surface-rt-rootfs.tar.gz}")" && pwd)/$(basename "${1:-alpine-surface-rt-rootfs.tar.gz}")
[ -f "$TARBALL" ] || { echo "check-install: no $TARBALL" >&2; exit 1; }
docker run --rm --privileged --platform linux/arm/v7 -v "$TARBALL:/in/rootfs.tar.gz:ro" -v "$HERE:/srt:ro" alpine:3.24 sh -euc '
apk add -q e2fsprogs e2fsprogs-extra python3 openssh-keygen >/dev/null
fail() { echo "check-install: FAIL: $*"; exit 1; }
# the pretend tablet
mkdir -p /usr/local/bin; ln -s /usr/local/bin /stub
cat > /stub/nmcli <<"N"
#!/bin/sh
case "$*" in
*"NAME,TYPE con show --active"*) echo "home net:802-11-wireless"; echo "lo:loopback" ;;
*key-mgmt*) echo wpa-psk ;;
*"-s -g 802-11-wireless-security.psk"*) echo "averylongpassphrase" ;;
*802-11-wireless.ssid*) echo "my net" ;;
*802-11-wireless.hidden*) echo no ;;
esac
N
printf "#!/bin/sh\ncase \"\$*\" in *PARTUUID*p2*) echo 11111111-2222-3333-4444-555555555555;; *UUID*) echo B619-E469;; esac\n" > /stub/lsblk
printf "#!/bin/sh\necho /dev/mmcblk0p3\n" > /stub/findmnt
chmod +x /stub/*
mkdir -p /boot /data; mount -t tmpfs tmpfs /boot; mount -t tmpfs tmpfs /data
mkdir -p /boot/loader/entries /boot/B; for f in vmlinuz initramfs dtb; do echo $f > /boot/B/$f; done
printf "timeout 5\nauto-entries no\ndefault slot-b.conf\n" > /boot/loader/loader.conf
printf "title Slot B (good)\nsort-key slot-b\nlinux /B/vmlinuz\ninitrd /B/initramfs\n" > /boot/loader/entries/slot-b.conf
mkdir -p /usr/local/sbin; printf "#!/bin/sh\necho old\n" > /usr/local/sbin/slotctl; chmod 755 /usr/local/sbin/slotctl
adduser -D -u 1001 admin 2>/dev/null; h=$(mkpasswd -m sha512 testpw)
sed -i "s|^admin:[^:]*:|admin:$h:|" /etc/shadow
mkdir -p /home/admin/.ssh; echo "ssh-ed25519 AAAATESTKEY test@mac" > /home/admin/.ssh/authorized_keys
mkdir -p /etc/ssh; for t in ed25519 rsa; do ssh-keygen -q -t $t -N "" -f /etc/ssh/ssh_host_${t}_key; done
mkdir -p /lib/firmware/updates/mrvl; echo fw > /lib/firmware/updates/mrvl/sd8797_uapsta.bin
echo vm > /tmp/vm; echo dtb > /tmp/dtb
dd if=/dev/zero of=/tmp/slot.img bs=1M count=400 2>/dev/null
LOOP=$(losetup -f); losetup "$LOOP" /tmp/slot.img
echo "== dry run"; SRT_TEST_DEV=$LOOP SRT_TEST_BOOTED=b sh /srt/install-slot.sh a -t /in/rootfs.tar.gz -k /tmp/vm -d /tmp/dtb -n | tail -3
echo "== install"; SRT_TEST_DEV=$LOOP SRT_TEST_BOOTED=b sh /srt/install-slot.sh a -t /in/rootfs.tar.gz -k /tmp/vm -d /tmp/dtb 2>&1 | grep -v "^slot \|^loader\|^booted"
echo "== checks"
mkdir /m; mount $LOOP /m; mount --bind /dev /m/dev
[ "$(e2label $LOOP)" = SRT_ROOT_A ] || fail label; echo "ok label SRT_ROOT_A"
grep -q "^admin:$h:" /m/etc/shadow || fail "admin hash"; echo "ok admin hash copied"
[ "$(stat -c "%a %u" /m/home/admin/.ssh/authorized_keys)" = "600 1001" ] && grep -q TESTKEY /m/home/admin/.ssh/authorized_keys || fail authorized_keys; echo "ok authorized_keys"
[ -f /m/lib/firmware/updates/mrvl/sd8797_uapsta.bin ] || fail firmware; echo "ok firmware"
[ -f /m/lib/modules/7.2.13-surface+/modules.dep ] || fail modules; echo "ok modules"
for t in ed25519 rsa; do
	cp /data/techo5-linux/dropbear/dropbear_${t}_host_key /m/tmp/k; a=$(chroot /m dropbearkey -y -f /tmp/k 2>&1 | sed -n "s/^Fingerprint: //p"); rm -f /m/tmp/k
	b=$(ssh-keygen -lf /etc/ssh/ssh_host_${t}_key.pub | cut -d" " -f2)
	[ "$a" = "$b" ] || fail "host key $t: $a vs $b"; echo "ok host key $t kept ($b)"
done
grep -q "ssid=6d79206e6574" /data/techo5-linux/wpa_supplicant.conf && grep -q "psk=360e8db08a42e7cfaf2bb0e86d3787e6e46ca91a9553eed0e7eb9f8763cbc2d0" /data/techo5-linux/wpa_supplicant.conf || fail wpa; echo "ok wpa_supplicant.conf ($(stat -c %a /data/techo5-linux/wpa_supplicant.conf))"
grep -q averylong /data/techo5-linux/wpa_supplicant.conf && fail "passphrase in clear"
[ -f /boot/A/vmlinuz ] && [ -f /boot/A/dtb ] && [ ! -e /boot/A/initramfs ] || fail esp; echo "ok /boot/A: $(ls /boot/A | tr "\n" " ")"
cat /boot/loader/entries/slot-a+3.conf; cat /boot/loader/loader.conf
grep -q "root=PARTUUID=11111111-2222-3333-4444-555555555555 rootfstype=ext4 rootwait=30 rw .*ab.slot=a ab.kernel=a" /boot/loader/entries/slot-a+3.conf || fail entry
grep -qx "preferred slot-a.conf" /boot/loader/loader.conf && grep -qx "default slot-b.conf" /boot/loader/loader.conf || fail loader.conf
[ -x /usr/local/sbin/bootctl ] && /usr/local/sbin/bootctl -q status | grep -q "^slot a: priority 15, tries 3, successful 0 (trial)" || fail bootctl; echo "ok slot a on trial in the boot record"
grep -q rootwait /usr/local/sbin/slotctl && [ -f /usr/local/sbin/slotctl.pre-alpine ] || fail "running slotctl"; echo "ok running slotctl replaced, old kept"
chroot /m su admin -s /bin/sh -c "echo testpw | sudo -S -p \"\" true" && echo "ok sudo for admin in the slot"
umount /m/dev /m; losetup -d $LOOP
echo "check-install: all good"
'
