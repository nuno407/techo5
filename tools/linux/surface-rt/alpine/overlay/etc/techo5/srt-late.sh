#!/bin/sh
# Surface RT: the last sysinit (etc/inittab), after TECHO5's boot.sh and before the daemon starts.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
LOG=/run/srt-boot.log
log() { echo "<14>srt-late: $*" > /dev/kmsg 2>/dev/null; echo "$(cut -d' ' -f1 /proc/uptime 2>/dev/null) $*" >> $LOG 2>/dev/null; }

# The daemon paints the framebuffer: take the console off it, or its cursor blinks in the corner of
# every frame and kernel messages (console=tty0) print over the screen. `echo 1 >` brings it back.
for v in /sys/class/vtconsole/vtcon*; do
	grep -q 'frame buffer' "$v/name" 2>/dev/null && echo 0 > "$v/bind" 2>/dev/null
done

# The cover (SW_LID) is the daemon's: display/lid_screen.go puts the screen out while it is closed.

# Wi-Fi power saving makes mwifiex doze between beacons: on a weak link (-75 dBm on 5 GHz in the
# bedroom, 2026-10-31) throughput fell to ~50 KB/s and a long upload stalled. An always-on panel has
# no battery to save.
iw dev wlan0 set power_save off 2>/dev/null && log "wifi: power save off"

log "late boot done: $(ip -4 addr show wlan0 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p' | head -1)"
cp $LOG /data/techo5-linux/srt-boot.log 2>/dev/null
exit 0
