# TECHO5 on the Microsoft Surface RT

The Surface RT is a 2012 Windows RT tablet: an NVIDIA Tegra 3 (four Cortex-A9s at 1.3 GHz, 2 GB),
a 1366x768 landscape touchscreen, a Wolfson WM8962 codec with two digital microphones and stereo
speakers, and a Marvell 88W8797 for Wi-Fi and Bluetooth. It runs postmarketOS (Alpine, systemd) on
a mainline kernel, and TECHO5's daemon runs on it as one more board of the Show build, `surface-rt`.
It can keep postmarketOS (the daemon and the few OS settings under "On postmarketOS" below), or run
TECHO5's own minimal Alpine root filesystem in one of the tablet's A/B slots, like the Echos'
images (next section).

Status: the daemon draws through KMS (two pages, page flips; fbdev as the fallback), reads touch,
the volume keys, the lid and the headphone jack, plays on the speaker or the headphones, decodes
H.264 on tegra-vde when the stream allows it, plays to Bluetooth speakers through bluealsa, serves
the ESPHome API and shows the Work and Sky boards. Voice is wired but not tuned (milestone 2); the
camera is wired and off unless `/data/techo5-linux/camera-enabled` exists.

## TECHO5's own image (Alpine, busybox init)

A root filesystem for one of the tablet's eMMC slots, in place of postmarketOS: Alpine armv7 built by
`mkrootfs.sh` (the Echos' `packages-rootfs.txt` plus `alpine/packages.txt`), busybox init, TECHO5's
`boot.sh`, `techo5-run` and `techo5-lib.sh`, and a Surface RT overlay (`alpine/overlay`). The
kernel and boot chain stay the surface_rt project's: UEFI → srt-wdt.efi → systemd-boot → the slot's
entry. The slots, their states and the trial are the surface_rt project's too (its `slotctl`,
`docs/ab-slots.md`); TECHO5's slot store, slot trial, SSH switch and updater stay off (no
`/run/techo5/slot`).

| Part | How |
| --- | --- |
| Boot | no initramfs: ext4, the eMMC and vfat are built in, so the entry says `root=PARTUUID=<slot> rootfstype=ext4 rootwait=30 rw` (slotctl writes it for a slot whose ESP directory has no `initramfs`; postmarketOS slots keep theirs). A root that never appears panics after 30 s and `panic=10` reboots |
| init | `etc/inittab`: `srt-early.sh`, then `boot.sh`, then `srt-late.sh`, then the daemon (`techo5-run`) and a getty on ttyS0 |
| Watchdog | `srt-early.sh` starts busybox `watchdog -T 100 -t 30 /dev/watchdog0` (tps65911_wdt) first thing, in place of systemd's `RuntimeWatchdogSec` |
| Mounts | `/data` = SRT_DATA (p4, shared by both slots, e2fsck -p first), `/boot` = SRT_ESP (p1); `/dev` is the kernel's devtmpfs |
| Modules | kmod's modprobe (the modules are `.ko.zst`): a coldplug pass over every `modalias`, `etc/modules`, then mdev as the hotplug helper (`etc/mdev.conf` loads each new device's driver) |
| Wi-Fi | mwifiex's `mlan0` renamed `wlan0` (TECHO5's scripts drive wlan0) and given the address NetworkManager used (`/data/techo5-linux/wifi-mac`), so the router keeps the lease; then TECHO5's own way: `wpa_supplicant` (Alpine's 2.11) with `/run/wpa`, udhcpc, `techo5-net`, the network keeper. `/data/techo5-linux/wpa_supplicant.conf` |
| SSH | dropbear on :22 (with OpenSSH's `sftp-server`, so `scp` works), kept running by `srt-early.sh`: `admin` (uid 1001, wheel, sudo with the same password), keys only (`-s`), no root (`-w`). Host keys on `/data/techo5-linux/dropbear`, converted from postmarketOS's OpenSSH ones, so the host key stays |
| A/B trial | The slot entries carry `ab.slot=<x>`, so boot.sh's own trial runs (commit after 300 s with the network up, `trial.conf`; reboot at 900 s) through bootctl's systemd-boot backend. `srt-trial-guard`: no gateway within 80 s rolls back into the good slot |
| Bluetooth | `srt-bluetooth`: `btattach -B /dev/ttyTHS1 -P h4 -S 3000000` once the Wi-Fi interface exists (the BT_UART firmware comes with mwifiex's); boot.sh then starts bluetoothd and bluez-alsa (`BT_MODULE=builtin`) |
| Other | `srt-audio-defaults` (WM8962 mixer), Wi-Fi power save off, zram swap (1 GB), fbcon unbound from the panel before the daemon starts, NTP (boot.sh), zone Europe/Lisbon |
| Firewall | the Echos' `techo5-firewall` on wlan0: 6053, 5353/udp, 22 and the daemon's other ports open (iptables-legacy; `-m state` loads, checked 2026-10-31) |
| State | `/data/misc/techo5` and `/data/techo5-linux`, on the shared partition, so they survive slot changes |

Files: `build-rootfs.sh` (the tarball), `alpine/` (`packages.txt`, `rootfs-post.sh`: admin, sudoers;
`overlay/`: inittab, `etc/techo5/device.conf`, `srt-early.sh`, `srt-late.sh`, the `srt-*` tools),
`install-slot.sh` (run on the tablet), `check-install.sh` (install-slot.sh against a pretend tablet
in a container). slotctl, srt-healthy and srt-audio-defaults come from the surface_rt project.

The tarball has no firmware and no secrets. `install-slot.sh` adds the tablet's own: its
`/lib/firmware` (the BT_UART firmware in `updates/mrvl` is Microsoft's driver's, not ours to
publish), admin's keys and password hash, the Wi-Fi network from NetworkManager's active connection
(the passphrase is hashed into a PSK on the tablet and never printed), its MAC and its SSH host keys.

### Build

```
(cd echod && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o ../bin/echod-arm ./cmd/echod)
sh tools/linux/surface-rt/build-rootfs.sh      # → ~/personal_repos/surface_rt/images/alpine-surface-rt-rootfs.tar.gz
sh tools/linux/surface-rt/check-install.sh     # optional: install-slot.sh against a pretend tablet
```

It needs Docker that runs `linux/arm/v7` containers (OrbStack on the Mac): mkrootfs.sh runs inside
Alpine's own armv7 container. `DAEMON`, `MODULES`, `SRT_REPO` and `INPUTS` override the inputs.

### Install into a slot

From the running (good) slot, into the other one, as root on the tablet:

```
scp alpine-surface-rt-rootfs.tar.gz tools/linux/surface-rt/install-slot.sh admin@$TABLET:/tmp/
ssh admin@$TABLET
sudo sh /tmp/install-slot.sh a -n      # dry run: every check, nothing changed
sudo sh /tmp/install-slot.sh a         # format slot a, unpack, add the tablet's files, slotctl trial a 1
sudo reboot
```

It formats only the target slot's partition, refuses the running slot, and puts the slot on trial for
one boot with the running one as the fallback. It also installs the image's slotctl on the running
system (the old one stays as `slotctl.pre-alpine`): that system's own slot-commit rewrites the other
slot's entry, and only the new slotctl writes one without an initramfs. Logs on the new slot:
`/run/srt-boot.log`, `/run/boot.log`, `/data/techo5-linux/{srt-boot.log,boot.log,techo5.log}`; a
trial that found no network leaves `/data/techo5-linux/srt-trial-failed.log` before rebooting.

## On postmarketOS

### What the daemon uses

| Part | How |
| --- | --- |
| Board | the device tree's root compatible `microsoft,surface-rt` (`layout.SurfaceRT`) |
| Display | `/dev/fb0`, tegra-drm's fbdev emulation, 1366x768 XRGB8888, one page, drawn as it is (not turned) |
| Backlight | `/sys/class/backlight/backlight`, 0..254 |
| Touch | Atmel maXTouch over HID-over-I2C (`hid-over-i2c 03EB:8207`), 0..4095 on both axes, scaled to the landscape frame |
| Buttons | `gpio-keys`: volume up and down only. The power key and the Windows key are not read |
| Speaker | the WM8962 card, found by its id `WM8962` (card 0 is the HDMI output), device 0, 48 kHz S16_LE stereo |
| Microphones | the same card's capture, two DMICs, 48 kHz S16_LE, decimated to 16 kHz; the echo reference is the software one (lib/echoref) |
| Mute | software only: no switch, no LED |
| Light sensor | the HID sensor hub's `als` (IIO, `in_illuminance_raw`) |
| MAC | the Wi-Fi card's permanent address (ETHTOOL_GPERMADDR on `mlan0`), not the one NetworkManager sets |
| Wi-Fi | `mlan0`, NetworkManager's. The daemon finds the interface by name (`layout.WifiInterface`) and does not manage it: there is no wpa_supplicant control socket for it in `/run/wpa`, so the screen's Wi-Fi setup is off |
| Updates | none: `update.Published` is false and the rootfs key `arm-surface-rt` has no releases, so a signed release never replaces this build |

### Install

Build the Show daemon as usual and copy it over:

```
cd echod
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o ../bin/echod-arm ./cmd/echod
cd ..
scp bin/echod-arm admin@$TABLET:/tmp/techo5
ssh admin@$TABLET
sudo install -m 755 /tmp/techo5 /usr/local/bin/techo5
sudo mkdir -p /data/misc/techo5/models /data/techo5-linux
```

Copy the wake word models from `inputs/models` (`*.tflite`, `*.json`) to `/data/misc/techo5/models`,
as the Echo image does from `/usr/share/techo5/models`. The state is in `/data/misc/techo5`, as on
every Show.

### OS changes

The files are in `overlay/`, at the paths they go to.

1. `etc/systemd/system/techo5.service`: the daemon as root, restarted whenever it exits. It conflicts
   with `getty@tty1` (the login console on the panel) and unbinds fbcon from the framebuffer before
   it starts: the console's cursor blinked in the top left corner of every frame, and kernel messages
   (`console=tty0`) would print over the screen. `echo 1 > /sys/class/vtconsole/vtcon1/bind` brings
   the console back. Logs: `journalctl -u techo5`.

   ```
   sudo systemctl daemon-reload
   sudo systemctl enable --now techo5
   sudo systemctl mask getty@tty1    # so the console never comes back on the panel at boot
   ```

2. PipeWire belongs to the panel user `surface` and opens the WM8962 when a client asks for it, which
   would take the card from the daemon. Mask it for that user:

   ```
   sudo systemctl --user -M surface@ mask pipewire.socket pipewire-pulse.socket pipewire.service \
       pipewire-pulse.service wireplumber.service
   ```

   `srt-audio-defaults.service` can stay: the daemon writes the same speaker and microphone settings
   itself when it opens the card.

3. `etc/systemd/logind.conf.d/techo5.conf`: `HandlePowerKey=ignore`. The power key is in the same
   `gpio-keys` device as the volume keys, and logind's default powers the tablet off. A long press
   still powers it off. Apply with `sudo systemctl reload systemd-logind`.

4. `etc/nftables.d/60_techo5.nft`: postmarketOS's firewall drops everything inbound but SSH, so Home
   Assistant cannot reach the API until this is in. It opens what the daemon serves, the same ports
   the Echo image's `techo5-firewall` opens: 6053/tcp (API), 5353/udp (mDNS), 8928/tcp (Sendspin),
   8181/tcp (setup page, views, DLNA), 3702/udp, 5000/tcp, 6001-6010/udp, 4070/tcp, 8554/tcp,
   1900/udp. Apply with `sudo systemctl reload nftables`.

Nothing else changes: the default target stays `graphical.target` (nothing graphical runs on it),
`mlan0` keeps its name and NetworkManager keeps the Wi-Fi. `srt-lid-blank` keeps turning the
backlight off while the cover is closed (`bl_power`, which the daemon does not touch).

## Not yet

- Voice (milestone 2): the microphones open at +21 dB digital gain with the codec's high-pass filter,
  and the software echo reference follows the speaker, but nothing is tuned: the leveler, the wake
  word's thresholds and the canceller were set on the Echos.
- The camera's colours and orientation in daylight, and its CPU cost with Home Assistant streaming
  it (74% of a core) and presence/person on (112%): tune before enabling it.
- Video without a copy: decoded frames are copied into the overlay plane (about 30% of a core at
  720p30); importing the decoder's buffers would remove it.
- Wi-Fi throughput with the BT_UART firmware (14.62) is low on this unit; see surface_rt's notes.
- The Type Cover is not read; its volume keys turn the volume like the tablet's.
