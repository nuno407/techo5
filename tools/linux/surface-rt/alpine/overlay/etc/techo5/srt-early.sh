#!/bin/sh
# Surface RT: the first sysinit (etc/inittab), before TECHO5's boot.sh.
#
# The kernel mounted this slot's partition as / and started init; there is no initramfs. This does
# what the Echos' initramfs does before boot.sh, plus what systemd did on postmarketOS: the PMIC
# watchdog, modules, /data and /boot, wlan0, SSH for admin and the A/B slot trial. Best effort: a step
# that fails is logged (kernel log and /run/srt-boot.log, copied to /data/techo5-linux) and the boot
# goes on, since a slot that boots unreachable falls back by itself (srt-trial-guard).
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
LOG=/run/srt-boot.log
log() { echo "<14>srt-early: $*" > /dev/kmsg 2>/dev/null; echo "$(cut -d' ' -f1 /proc/uptime 2>/dev/null) $*" >> $LOG 2>/dev/null; }
[ -r /etc/techo5/device.conf ] && . /etc/techo5/device.conf

# --- Kernel file systems.
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs -o mode=0755 devtmpfs /dev
mkdir -p /dev/pts /dev/shm
mountpoint -q /dev/pts || mount -t devpts -o gid=5,mode=0620,ptmxmode=0666 devpts /dev/pts
mountpoint -q /run || mount -t tmpfs -o mode=0755,nosuid,nodev tmpfs /run
mountpoint -q /tmp || mount -t tmpfs -o mode=1777,nosuid,nodev tmpfs /tmp
mkdir -p /run/techo5 /run/lock
# The booted slot, where the Echos' initramfs leaves it: boot.sh's slot trial reads it.
srt-slot > /run/techo5/slot 2>/dev/null || rm -f /run/techo5/slot
log "slot $(srt-slot 2>/dev/null || echo '?'), kernel $(uname -r), $(cat /etc/techo5-release 2>/dev/null)"

# --- The PMIC watchdog (tps65911_wdt, a real power cut ~100 s after the last feed), fed from here
# on as systemd's RuntimeWatchdogSec did: a hang anywhere after this power-cycles the tablet, and a
# slot on trial then falls back to the good one. busybox's watchdog stops it (magic close) when init
# kills everything at shutdown.
if [ -c "${SRT_WATCHDOG:-/dev/watchdog0}" ]; then
	if watchdog -T "${SRT_WATCHDOG_TIMEOUT:-100}" -t "${SRT_WATCHDOG_FEED:-30}" "${SRT_WATCHDOG:-/dev/watchdog0}"; then
		log "watchdog: $(cat /sys/class/watchdog/watchdog0/identity 2>/dev/null) fed every ${SRT_WATCHDOG_FEED:-30} s"
	else
		log "watchdog: busybox watchdog did not start"
	fi
else
	log "watchdog: no ${SRT_WATCHDOG:-/dev/watchdog0}"
fi

# --- The root is mounted read-write by the kernel (rw on the command line); make sure.
touch /.srt-rw 2>/dev/null && rm -f /.srt-rw || mount -o remount,rw / || log "root: still read-only"
hostname -F /etc/hostname

# --- /data (shared by both slots) and /boot (the ESP, where slotctl keeps the slot state).
dev_of() { # <label> <fallback>
	d=$(findfs "LABEL=$1" 2>/dev/null)
	echo "${d:-$2}"
}
mkdir -p /data /boot
if ! mountpoint -q /data; then
	d=$(dev_of SRT_DATA "${DATA_DEV:-/dev/mmcblk0p4}")
	timeout 120 e2fsck -p "$d" >> $LOG 2>&1
	r=$?
	[ $r -le 1 ] || log "data: e2fsck -p $d returned $r"
	mount -t ext4 -o noatime,nodev,nosuid "$d" /data && log "data: $d on /data" || log "data: mounting $d failed"
fi
if ! mountpoint -q /boot; then
	d=$(dev_of SRT_ESP "${ESP_DEV:-/dev/mmcblk0p1}")
	# systemd-boot renames a trial's entry and rewrites its random seed through the firmware's FAT
	# driver, which leaves the volume's dirty bit set: check it (and clear the bit) before mounting.
	timeout 60 fsck.vfat -a -w "$d" >> $LOG 2>&1
	r=$?
	[ $r -le 1 ] || log "esp: fsck.vfat -a $d returned $r"
	mount -t vfat -o nodev,nosuid,noexec,umask=0077,codepage=437,iocharset=ascii "$d" /boot && log "esp: $d on /boot" ||
		log "esp: mounting $d failed; this slot cannot commit"
fi
mkdir -p /data/techo5-linux /data/misc/techo5

# --- Devices: mdev handles what appears from now on (etc/mdev.conf loads its driver); the devices
# already there are loaded here by their aliases, then whatever etc/modules names.
echo /sbin/mdev > /proc/sys/kernel/hotplug
coldplug() { find /sys/devices -name modalias -type f -exec cat {} + 2>/dev/null | sort -u | xargs -r modprobe -q -b -a 2>/dev/null; }
coldplug
sed 's/#.*//' /etc/modules 2>/dev/null | xargs -r modprobe -q -a 2>/dev/null
# A second pass for the devices the first one's drivers made (HID's sensors and touch, evdev).
coldplug
log "modules: $(wc -l < /proc/modules) loaded"

# --- CPU frequency (tegra20-cpufreq, loaded above): schedutil, the kernel's default, holds all four
# cores at 1.4 GHz on this kernel however idle they are (no sugov thread; the clamp for RT tasks at 0
# changes nothing), and with power-gated idle disabled (the secure firmware sets no resume address)
# the tablet then runs hot: tsensor 58-61 °C, the skin 43-46. ondemand with a 475 MHz floor sits at
# 475-760 MHz, tsensor 51 °C and the skin 40; the wake word stays at 12% of a core and 8-10 ms worst
# frame (the 51 MHz floor tripled its frame time and gave 40-50 ms worst ones).
p=/sys/devices/system/cpu/cpufreq/policy0
if [ -w $p/scaling_governor ] && echo "${SRT_CPU_GOVERNOR:-ondemand}" > $p/scaling_governor 2>/dev/null; then
	[ -w /sys/devices/system/cpu/cpufreq/ondemand/up_threshold ] &&
		echo "${SRT_CPU_UP_THRESHOLD:-80}" > /sys/devices/system/cpu/cpufreq/ondemand/up_threshold
	echo "${SRT_CPU_MIN_KHZ:-475000}" > $p/scaling_min_freq 2>/dev/null
	log "cpufreq: $(cat $p/scaling_governor), $(cat $p/scaling_min_freq)-$(cat $p/scaling_max_freq) kHz"
else
	log "cpufreq: governor ${SRT_CPU_GOVERNOR:-ondemand} not set"
fi

# --- SSH for admin (keys only, no root), kept running. Host keys on /data, shared by both slots
# (/etc/dropbear links there; install-slot.sh converts the OpenSSH ones postmarketOS had).
mkdir -p /data/techo5-linux/dropbear
chmod 700 /data/techo5-linux/dropbear
(
	while true; do
		dropbear -F -E -R -s -w -p 22 -P /run/dropbear-admin.pid >> /run/dropbear.log 2>&1
		sleep 5
	done
) < /dev/null > /dev/null 2>&1 &
log "ssh: dropbear on :22 (admin, keys only)"

# --- A/B slot trial: boot.sh's (bootctl's record, here systemd-boot's entries: commit once the
# daemon has run five minutes with a default route, etc/techo5/trial.conf). srt-trial-guard adds the
# early way out: no gateway within SRT_TRIAL_NET_WAIT seconds marks the slot bad and reboots into
# the good one. A no-op on a good slot.
srt-trial-guard < /dev/null >> $LOG 2>&1 &

# --- zram swap.
if [ "${SRT_ZRAM_MB:-0}" -gt 0 ] && [ -e /sys/block/zram0/disksize ] && ! grep -q zram0 /proc/swaps; then
	echo "${SRT_ZRAM_MB}M" > /sys/block/zram0/disksize 2>/dev/null &&
		mkswap /dev/zram0 >/dev/null 2>&1 && swapon -p 100 /dev/zram0 && log "zram: ${SRT_ZRAM_MB} MB swap" ||
		log "zram: not set up"
fi

# --- Wi-Fi: mwifiex makes mlan0 once its firmware is in (a few seconds); TECHO5's scripts drive
# wlan0. Renamed while it is down, and given the address postmarketOS used (NetworkManager's stable
# per-network MAC, saved by install-slot.sh), so the router hands out the same lease. echod reports
# the card's permanent address either way (layout.surfaceRTMAC).
srt_wifi_name() {
	[ -e /sys/class/net/wlan0 ] && return 0
	[ -e /sys/class/net/mlan0 ] || return 1
	ip link set mlan0 down 2>/dev/null
	ip link set mlan0 name wlan0 || { log "wifi: renaming mlan0 failed"; return 1; }
	mac=$(tr -cd '0-9a-f:' < /data/techo5-linux/wifi-mac 2>/dev/null)
	if [ ${#mac} -eq 17 ]; then
		ip link set wlan0 address "$mac" && log "wifi: mlan0 is wlan0, address $mac" || log "wifi: mlan0 is wlan0; address $mac refused"
	else
		log "wifi: mlan0 is wlan0, its own address"
	fi
}
n=0
until srt_wifi_name; do
	n=$((n+1))
	if [ $n -ge "${SRT_WIFI_WAIT:-40}" ]; then
		log "wifi: no mlan0 after ${n}s; still looking for 10 minutes"
		(m=0; until srt_wifi_name; do m=$((m+1)); [ $m -ge 600 ] && exit 1; sleep 1; done) &
		break
	fi
	sleep 1
done

# --- Bluetooth: the BT_UART firmware is in once mwifiex is up.
[ "${SRT_BLUETOOTH:-1}" = 1 ] && srt-bluetooth < /dev/null >> /run/btattach.log 2>&1 &

# --- Speaker and microphone mixer defaults (the daemon sets the same when it opens the card).
(sleep 3; srt-audio-defaults) < /dev/null > /dev/null 2>&1 &

cp $LOG /data/techo5-linux/srt-boot.log 2>/dev/null
log "early boot done"
exit 0
