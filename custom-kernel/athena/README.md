# Linux 7.2.11 for the Echo Show 8 2nd gen (athena)

This directory builds the Echo Show 8 2nd gen (`athena`, MediaTek MT8183) kernel on Linux 7.2.11,
the release the Echo Show 5 build in `..` uses. TECHO5's athena kernel in
[`tools/linux/athena/kernel`](../../tools/linux/athena/kernel/README.md) is on 6.18.56. This
build takes everything from that directory: the device tree, the machine driver, the patches, the
configuration, the firmware list, the boot image packer and its checker. It changes only the kernel
release, and adds `patches/port-7.2.py` for the three places where 7.2's sources differ from 6.18's.
`uname -r` says `7.2.11-techo5-athena`.

## Carried over to 7.2

`patches/port-7.2.py` runs before the athena patches:

| Step | Why |
| --- | --- |
| `jd9365` | 7.2's `jadard,jd9365da-h3` binding lists more panels, so `panel/0003` no longer applies. The script adds the five `amazon,athena-jd9365-*` compatibles to the enum itself, and `patch-display.py` then skips 0003. |
| `jd9365` driver | 7.2's `jadard_panel_desc` points at its mode (`mode_4ln`) where 6.18's held it inline (`mode`). The script applies `panel/0004` and moves its five panel descriptions to the pointer; `patch-display.py` then skips 0004. |
| `tcs3400` | 7.2's `tcs3472.c` logs through a local `dev` in probe, and its i2c id table uses `.name =`. The script runs `patch-tcs3400.py` with those two spellings translated. The change to the driver is otherwise upstream's. |

The configuration is `athena_defconfig` from the 6.18.56 build, with 7.2's new options set to their
defaults by `olddefconfig`. The build checks the same required options as the 6.18.56 build, and
writes `config-dropped.txt`: options set in that configuration that 7.2 no longer has. The radio's
temperature has no source on mainline: mt76's hwmon reading for the MT7663 answers 0, so Home
Assistant's Radio temperature stays unknown.

The camera cover's switch (GPIO152, `SW_CAMERA_LENS_COVER`) stays active low, as the DTS has it: GPIO152
reads high with the shutter open, low with it across the lens (read on the BOE-new unit 2026-10-16, with
the camera showing the room while the pin read high, and the switch following the shutter both ways).

## Building

On Linux, with the tools the 6.18.56 build lists. On a Mac, use the arm64 `techo5-kbuild`
container from `../docker`, with the repository mounted read-only and a Linux volume as the work
directory:

```sh
docker run --rm --platform linux/arm64 -v techo5-k6:/src -v "$PWD":/repo:ro -w /repo \
    techo5-kbuild bash -c 'WORK=/src/athena-7.2.11 custom-kernel/athena/build.sh'
```

Output is in `$WORK/out` (default `~/techo5-athena-7.2.11/out`): `boot-athena.img`, its parts,
`kernel.config`, `kernel.release`, `config-dropped.txt` and `SHA256SUMS`. `check-boot.py` checks
the boot image, with the amp tuning slot empty, before the build finishes.

## Installing

The boot image has the same layout as the 6.18.56 one, so the installer takes it unchanged. The
unit must already be unlocked with saiyaku-athena, as
[docs/install-athena.md](../../docs/install-athena.md) describes:

```sh
python3 tools/install-athena.py --boot "$WORK/out/boot-athena.img" --image system.img
```

The installer fills the amp tuning slot from the unit's own stock kernel. Like the 6.18.56 image,
this one ships no modules, so the same `system` image works with either kernel.

## Updates

`release.sh <version>` builds an athena release for the private channel `../release.sh` uses, signed
with the same key (`~/.config/techo5/release-key`), and copies it to Home Assistant:

```sh
custom-kernel/athena/release.sh --dry-run v1.1.1-mainline.1
custom-kernel/athena/release.sh v1.1.1-mainline.1
```

It builds the boot image (`build.sh`, in the `techo5-kbuild` container, on the kernel tree left in the
`techo5-k6` volume), the root filesystem slot (`build-rootfs.sh`: the Show's daemon on Alpine armv7
with `tools/linux/athena/overlay`, and no kernel modules), and the system image for new units
(`build-store.sh`, which runs upstream's `tools/linux/athena/mkstore.sh -k none` in a privileged
Linux container). The files have upstream's names, so `tools/install-athena.py` takes them as they
are:

| File | What |
| --- | --- |
| `techo5-rootfs-athena-<version>.tar.gz` | the slot, keyed `arm-athena` in the manifest: what the daemon installs into the spare slot |
| `techo5-system-athena-<version>.img.gz` | the `system` image for a new unit; refused if it carries an SSH key |
| `techo5-boot-athena-<version>.img` | the 7.2.11 boot image, checked by `check-boot.py` with its tuning slot empty |
| `echod-arm`, `manifest.json`, `manifest.json.sig`, `SHA256SUMS` | the daemon, and the signed manifest naming the three above |

The manifest's signature is checked against `release-key.pub` the way the installer checks it, and
every file it names against its sha256. `--dry-run` stops there: the release is in
`out/release/<version>` and nothing is copied. `--publish <version>` later copies that
staged release as it is, after checking its signature again, so what the channel offers is what was
installed from the dry run.

athena releases go to `$CHANNEL/athena` (copied to `$DEST/athena`), not to the Echo Show 5's channel:
a channel has one latest manifest, and a unit is offered only a release with a root filesystem for its
board, so sharing one would hide each board's update from the other. The daemon in the slot follows
that channel and trusts only the release key. `ATHENA_CHANNEL` and `ATHENA_DEST` override both, and
`ATHENA_BOOT=<build.sh output>/boot-athena.img` uses a boot image already built.

A network update brings the kernel too. The slot carries the release's boot image
(`/usr/share/techo5/kernel/boot-athena.img`, `build-rootfs.sh`'s `KERNEL_IMAGE`), and once the new slot
is good the daemon writes it to the boot partition with the unit's own amp tuning, on trial, putting the
previous kernel back if it does not settle (`echod/internal/update/kernel`). The slot has no modules,
so it also runs on the kernel the unit had before. A unit installed from an upstream release follows
upstream's channel; to move it to this one, install a release from here with the installer, which
writes both partitions:

```sh
R=custom-kernel/athena/out/release/<version>
python3 tools/install-athena.py --image $R/techo5-system-athena-<version>.img.gz --boot $R/techo5-boot-athena-<version>.img
```

### 64-bit userspace

The kernel is 64-bit; the root filesystem has been Alpine armv7 with a 32-bit daemon, as on every
other TECHO5 board. `T5_ARCH=aarch64` builds it 64-bit instead: Alpine aarch64, the daemon and its
tools for arm64, and the helpers from `bin/*-arm64` (`techo5-aec` built against Alpine's aarch64
webrtc-audio-processing, `techo5-librespot` built for `aarch64-unknown-linux-musl`, statically). Its
Alpine base and the Alpine 3.12 wpa_supplicant set come from `$TECHO5_INPUTS/aarch64`.

```sh
T5_ARCH=aarch64 ATHENA_PANEL=jd9365-boe-new custom-kernel/athena/release.sh --dry-run v1.2.19-mainline.7
```

The release ships the daemon as `echod-arm64` and publishes the slot under the 64-bit key, which a
64-bit daemon asks for, and under the 32-bit one too, so a unit still on the 32-bit userspace moves
over with an ordinary update. Measured 2026-10-17 against the armv7 build on the same units: the
wake word takes 10-15% less time a frame, and the daemon about a fifth to a third less CPU.

### Other panels

The DTS drives the ILI9881C on BOE glass. A unit with another panel (the installer names it and
stops) needs a boot image built for it: `PANEL` for `build.sh`, `ATHENA_PANEL` for `release.sh`, as
`patches/panel-dts.py` names them. It takes the panel's compatible from the tables in
`tools/linux/athena/kernel/patches/panel` and, for a JD9365, the jadard driver's two supplies, as the
DTS's comment at `&dsi0` says. The boot loader's `jd936x_id=0x?5` is `jd9365-boe-new`:

```sh
ATHENA_PANEL=jd9365-boe-new custom-kernel/athena/release.sh --dry-run v1.2.17-mainline.1
python3 tools/install-athena.py --force-panel --image ... --boot ...
```

The installer needs `--force-panel`, since it accepts only the ILI9881C.

The JD9365 BOE-new panel (`jd936x_id=0x35`) works, with a stable picture across reboots and blank
cycles and brightness that follows Home Assistant. It took four changes, each found on that unit:

| Patch | What it does | Without it |
| --- | --- | --- |
| `jd9365-dsi-mode.py` | Sets burst video with `MIPI_DSI_MODE_LPM` on the five JD9365 descriptions; panel/0004 sets no mode flags, so its init table goes out in high-speed mode. | Dark screen, no error. |
| `mtk-dsi-cmd-clock-lp.py` | Keeps mediatek-dsi's clock lane in LP while commands go out, and makes it continuous for video. | With a continuous clock throughout, the table takes at some boots only. With a non-continuous clock in video, the picture flickers. |
| `jd9365-dsi-mode.py` (BOE-new line) | Gives the BOE-new mode the boot loader's line: 478 Mbps a lane, HSA/HBP/HFP 56/110/235 bytes (22/32/108-pixel porches at 79.68 MHz). | panel/0004's 20/20/30 at 69.2 MHz gave 415 Mbps and a 34-byte front porch, and the picture moved and flickered. |
| `panel-dts.py` (backlight PWM) | Runs the display PWM at 25 kHz (a 40 us period) in place of the DTS's 2 kHz. | The LED driver ignored the PWM: 4% and 100% duty looked the same, so brightness did nothing. |

The boot loader's line was read from the DSI and MIPI TX registers LK leaves at the kernel's start
(`patches/mtk-dsi-dump-boot-regs.py`, a diagnostic build only, not run by `build.sh`). Amazon's kernel
names the same 478 Mbps (`PLL_CLOCK` 239) for all of athena's panels, and its BOE-new init table is the
one panel/0004 carries. Its lcd-backlight node is the display PWM (`led_mode` 5) on every board
revision, with a divider for 6 to 25 kHz; half of the revisions also list an LM36274 or KTZ8864A
backlight chip on I2C, and the BOE-new unit has none on any bus. The panel's own brightness (DCS
0x51) takes its writes, but does not reach the LEDs either. `jd9365-power-mode-log.py` logs the panel's reply to a DCS read after the init
table: `panel power mode 0x18` says the table took, and a read that times out says it did not.

The other four JD9365 descriptions get the mode flags but keep panel/0004's timing, untried on a unit.
If the screen stays dark, the unit still boots and answers on SSH, and the backup's `p20-boot.img`
written back from TWRP restores Amazon's kernel.

## Camera

The OmniVision OV13855 works on 7.2.11, and the daemon finds it as it finds the Echo Show 5's: camera,
presence, presence detection and sensitivity, screen off when nobody is near, and gestures all appear
in Home Assistant. Mainline has no MT8183 sensor interface or ISP driver, and the ChromeOS ones need the
SCP's firmware to program the ISP, so `patches/mt8183-athena-camera.c` drives one fixed path itself,
the way the Show 5's `cronos-camera` does on MT8163:

```
OV13855 --4 lanes--> CSI0 D-PHY --> seninf1 --> MUX0 --> CAM2 --> CAMSV1 --IMGO DMA (larb6)--> /dev/videoN
```

| Part | From |
| --- | --- |
| Wiring | Amazon's athena-pvt device tree in the stock boot image: I2C2 address 0x36 (EEPROM at 0x54), reset GPIO102, power-down GPIO177, MCLK on CMMCLK0 (GPIO99), vcama1 2.8 V, vcamd 1.2 V, vcamio 1.8 V, CAMSV1 interrupt 258 |
| Sensor tables | MediaTek's `ov13855_mipi_raw` imgsensor driver (`sensor_init`, and `preview_setting`: 2112x1568 binned, 540 Mbps a lane), with the frame stretched to 6428 lines for 15 frames a second |
| D-PHY, seninf, CAMSV | the MT8183 bring-up of BayLibre's MT8365 seninf/CAMSV drivers (github.com/vpavlin/mt8183-camera), confirmed there on an MT8183 Chromebook: CAM2 feeds CAMSV1, 1-pixel mode, `SENINF_TOP_CTRL` 0xc00, the packer's mode numbers reversed (PAK 1 is 10-bit) |

`patches/athena-camera.py` adds the driver to the tree (`CONFIG_VIDEO_ATHENA_CAMERA`, with V4L2 built
in, since the image ships no modules), and `patches/panel-dts.py` adds its node and pins on every panel.
The sensor is on the bit-banged I2C2 the light sensor uses (`i2c2_gpio`). The video node is
`athena-ov13855`: 2112x1568, four pixels in five bytes, least significant bits first, the first pixel
blue; `V4L2_CID_EXPOSURE` in lines (4 to 6412) and `V4L2_CID_ANALOGUE_GAIN` in 1/64 (64 to 992). The
daemon develops it as it does the Show 5's frames (`echod/internal/hardware/camera`, `sensorOV13855`).

Read on the BOE-new unit 2026-10-16: the sensor identifies (0xd855), frames come at the expected size
with one interrupt each, and a still from Home Assistant's camera entity shows the room the right way
up. Two things found on the way:

- CAMSV's `IMGO_ERR` (bit 16) stays raised for the whole stream while every frame is whole and correct,
  in 8-bit and 10-bit packing and with any IMGO bus size, and neither reading nor writing the status
  clears it. Enabled, it interrupts about half a million times a second, so the driver leaves it off.
  The Chromebook bring-up masked it as well.
- `lane_mux` (a module parameter, writable in `/sys/module/mt8183_athena_camera/parameters`) is
  MediaTek's usual 4D1C lane order, 0xc9; it is right on athena as it is on the Chromebooks.

To try a boot image by hand, note that `boot.sh` puts the slot's own kernel back at every boot
(`techo5 tools kernel apply`) when the boot partition holds another one, unless
`/data/techo5-linux/kernel/state` says the kernel in the partition is the one it committed.

## License

`build.sh` and the release scripts (`release.sh`, `build-rootfs.sh`, `build-store.sh`) are MIT, and
`patches/port-7.2.py` is GPL-2.0, like the files they reuse from `tools/linux/athena/kernel`.
The kernel in a boot image built from them is under the GPL-2.0.
