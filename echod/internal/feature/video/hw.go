//go:build !dot

package video

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/layout"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

// The Surface RT's hardware decoder. Its Tegra 3 has the VDE, an H.264 decoder the kernel offers as a
// V4L2 stateless decoder (tegra-vde, /dev/video0); GStreamer's v4l2slh264dec drives it. ffmpeg still
// fetches and demuxes the stream and makes the sound as it does everywhere, but hands the picture over
// as it is, H.264 in Annex B form, rather than decoding it. The daemon reads the parameter sets at the
// start (h264.go) and, if the VDE takes the stream, passes it on to gst-launch, which hands back the
// decoded pictures as I420 at their own size. The screen puts those on an overlay plane that scales
// and converts them (hardware/screen.PresentYUV): no core decodes, scales or converts a pixel.
//
// A stream the VDE does not take, or a board without the plane, decodes on the cores as before.
// GStreamer runs as the decoder's user, in the video group, which the decoder's device nodes are given
// to (hwDeviceAccess).

const gstLaunch = "/usr/bin/gst-launch-1.0"

// hwDecoderName is the kernel's name for the VDE's video node.
const hwDecoderName = "tegra-vde"

// hwMaxPixels is the largest picture the VDE decodes here: 1080p (it goes to 2032×2032, which the
// screen has no use for).
const hwMaxPixels = 1920 * 1088

// hwWanted is whether to try the hardware for this video.
func hwWanted(info Info, scr Screen) bool {
	return layout.SurfaceRT() && scr.Planes && info.VideoCodec == "h264" && !info.Interlaced &&
		(info.PixFmt == "" || info.PixFmt == "yuv420p" || info.PixFmt == "yuvj420p") &&
		info.Rotation%360 == 0 && info.Width > 0 && info.Height > 0 && info.Width%2 == 0 && info.Height%2 == 0 &&
		info.Width*info.Height <= hwMaxPixels && fileExists(gstLaunch) && hwDevice() != ""
}

// HWDecodeArgs runs ffmpeg for the hardware: the picture as H.264 in Annex B on fd 3, the sound as
// DecodeArgs makes it.
func HWDecodeArgs(d Decoding) []string {
	a := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-max_pixels", maxPixels}
	a = append(a, netArgs(d.URL, d.CAFile, d.Insecure, d.Proxy)...)
	a = append(a, "-i", d.URL)
	a = append(a, "-map", "0:v:"+strconv.Itoa(d.Info.VideoIndex), "-an", "-sn", "-dn",
		"-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "pipe:3")
	if d.Info.AudioIndex >= 0 {
		a = append(a, "-map", "0:a:"+strconv.Itoa(d.Info.AudioIndex), "-vn", "-sn", "-dn",
			"-af", soundFilter, "-ac", strconv.Itoa(soundChannels), "-ar", strconv.Itoa(soundRate),
			"-f", "s16le", "pipe:1")
	}
	return a
}

// gstArgs decodes Annex B H.264 from standard input to raw I420 pictures on standard output.
var gstArgs = []string{"-q", "fdsrc", "fd=0", "!", "h264parse", "!", "v4l2slh264dec", "!",
	"video/x-raw,format=I420", "!", "fdsink", "fd=1", "sync=false"}

// ExactRate is the stream's own frame rate, which the hardware path shows every frame of: FrameRate
// halves a rate past 30 for the cores' sake.
func ExactRate(fps float64) Rate {
	if fps <= 1 || fps > 240 || math.IsNaN(fps) {
		return Rate{30, 1}
	}
	for _, r := range []Rate{{24000, 1001}, {30000, 1001}, {60000, 1001}} {
		if math.Abs(fps-float64(r.Num)/float64(r.Den)) < 0.01 {
			return r
		}
	}
	if math.Abs(fps-math.Round(fps)) < 0.01 {
		return Rate{int(math.Round(fps)), 1}
	}
	return Rate{int(math.Round(fps * 1000)), 1000}
}

// hwDevice is the VDE's video node, "" without one.
func hwDevice() string {
	names, _ := filepath.Glob("/sys/class/video4linux/video*/name")
	for _, n := range names {
		if b, err := os.ReadFile(n); err == nil && strings.TrimSpace(string(b)) == hwDecoderName {
			return "/dev/" + filepath.Base(filepath.Dir(n))
		}
	}
	return ""
}

// hwDeviceAccess gives the VDE's video and media nodes to the video group, read and write, so the
// decoder's user (in that group) can open them: the image's devtmpfs makes them root's alone. The
// camera's nodes stay root's.
func hwDeviceAccess() (gid int, err error) {
	g, err := user.LookupGroup("video")
	if err != nil {
		return 0, err
	}
	gid, err = strconv.Atoi(g.Gid)
	if err != nil {
		return 0, err
	}
	dev := hwDevice()
	if dev == "" {
		return 0, errors.New("no " + hwDecoderName)
	}
	nodes := []string{dev}
	dir := filepath.Join("/sys/class/video4linux", filepath.Base(dev), "device")
	media, _ := filepath.Glob(filepath.Join(dir, "media*"))
	for _, m := range media {
		nodes = append(nodes, "/dev/"+filepath.Base(m))
	}
	for _, n := range nodes {
		if err := os.Chown(n, 0, gid); err != nil {
			return 0, err
		}
		if err := os.Chmod(n, 0o660); err != nil {
			return 0, err
		}
	}
	return gid, nil
}

// gstLimits are the decoder's limits (limits) with more files open: each picture the VDE has in hand is a
// media request, a DMA buffer and GStreamer's poll on it, and with the pictures waiting on the screen
// (the frames queued ahead) 64 ran out, which GStreamer reports as running out of memory.
func gstLimits() []string {
	out := make([]string, 0, len(limits))
	for _, l := range limits {
		if strings.HasPrefix(l, "-n ") {
			l = "-n 512"
		}
		out = append(out, l)
	}
	return out
}

// gstRegistryPath is GStreamer's plugin registry for the decoder, made by the daemon once a boot.
const gstRegistryPath = "/run/techo5/gst-registry.bin"

var gstRegistryMu sync.Mutex

// gstRegistry makes GStreamer's plugin registry, once: gst-inspect-1.0 run as root, which can open the
// decoder's devices, so v4l2codecs registers the decoder it finds. The file is readable by all and
// written by root only.
func gstRegistry(ctx context.Context) (string, error) {
	gstRegistryMu.Lock()
	defer gstRegistryMu.Unlock()
	if st, err := os.Stat(gstRegistryPath); err == nil && st.Size() > 0 {
		return gstRegistryPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(gstRegistryPath), 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(filepath.Dir(gstLaunch), "gst-inspect-1.0"), "v4l2slh264dec")
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/", "GST_REGISTRY=" + gstRegistryPath}
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(gstRegistryPath)
		return "", fmt.Errorf("GStreamer has no %s: %v %s", "v4l2slh264dec", err, clip(strings.TrimSpace(string(out)), 200))
	}
	if err := os.Chmod(gstRegistryPath, 0o644); err != nil {
		return "", err
	}
	return gstRegistryPath, nil
}

// hwFrame is the size of one picture from the hardware.
func hwFrame(info Info) (w, h, size int) {
	return info.Width, info.Height, i420Size(info.Width, info.Height)
}

// i420Size is GStreamer's I420 layout's size (hardware/screen.I420Size).
func i420Size(w, h int) int {
	ys, cs := (w+3)&^3, ((w+1)/2+3)&^3
	return ys*h + 2*cs*((h+1)/2)
}

// hwCheckBytes is how much of the stream's start may be read looking for its parameter sets: they lead
// a stream that starts on a keyframe, which ffmpeg's copy does.
const hwCheckBytes = 2 << 20

// checkHW reads the start of ffmpeg's dec (already started with HWDecodeArgs) until its parameter sets
// say whether the VDE takes it: the start read, and the first SPS, if it does; if not,
// the error says why.
func (s *session) checkHW(dec *decoder) (head, sps []byte, err error) {
	var check h264Check
	head = make([]byte, 0, 64<<10)
	buf := make([]byte, 32<<10)
	_ = dec.video.SetReadDeadline(time.Now().Add(probeTimeout))
	for !check.done() && len(head) < hwCheckBytes {
		n, err := dec.video.Read(buf)
		head = append(head, buf[:n]...)
		check.feed(head)
		if err != nil {
			if check.done() {
				break
			}
			return nil, nil, fmt.Errorf("the stream's start: %w", err)
		}
	}
	_ = dec.video.SetReadDeadline(time.Time{})
	if !check.done() {
		return nil, nil, errors.New("no H.264 parameter sets in the stream's start")
	}
	if check.err != nil {
		return nil, nil, check.err
	}
	return head, check.spsNAL, nil
}

// startHW starts gst-launch on the stream, head being what checkHW read of it, the rest still to come
// from dec: the returned file is the pictures.
func (s *session) startHW(dec *decoder, head []byte) (*os.File, *decoder, error) {
	gid, err := hwDeviceAccess()
	if err != nil {
		return nil, nil, fmt.Errorf("the decoder's device: %w", err)
	}
	cred, err := decoderCred()
	if err != nil {
		return nil, nil, err
	}
	hwCred := *cred
	hwCred.Groups = append(append([]uint32(nil), cred.Groups...), uint32(gid))

	g := &decoder{stderr: &tail{}, done: make(chan struct{})}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, err
	}
	growPipe(outR)
	name, argv := wrapWith(gstLaunch, gstArgs, gstLimits())
	cmd := exec.CommandContext(s.ctx, name, argv...)
	cmd.Dir = "/"
	// The decoder may write no file (limits), so it reads a plugin registry the daemon made and never
	// updates it.
	reg, err := gstRegistry(s.ctx)
	if err != nil {
		return nil, nil, err
	}
	cmd.Env = append(decoderEnv[:len(decoderEnv):len(decoderEnv)], "GST_REGISTRY="+reg, "GST_REGISTRY_UPDATE=no")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, g.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &hwCred, Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, err
	}
	inR.Close()
	outW.Close()
	g.cmd, g.video = cmd, outR
	go func() {
		g.err = cmd.Wait()
		if g.err != nil && s.ctx.Err() == nil {
			slog.Warn("video: the hardware decoder stopped", "id", s.id, "err", g.err, "said", clip(strings.Join(strings.Fields(g.stderr.all()), " "), 1500))
		}
		close(g.done)
	}()
	// The stream into GStreamer: what was read to check it, then the rest as ffmpeg makes it. When
	// ffmpeg's picture ends, GStreamer's input does, and it finishes its last pictures and stops.
	safe.Go("video hw feed", func() {
		defer inW.Close()
		if _, err := inW.Write(head); err != nil {
			return
		}
		_, _ = io.Copy(inW, dec.video)
	})
	return outR, g, nil
}

// playHW plays with the hardware, after the probe. It answers whether it did: false with no frames
// read leaves the session for the cores to play from the start.
func (s *session) playHW(info Info, d Decoding) (played bool, readErr error, dec, gst *decoder) {
	dec, err := start(s.ctx, HWDecodeArgs(d), true, s.sound)
	if err != nil {
		slog.Warn("video: no hardware decode", "id", s.id, "err", err)
		return false, nil, nil, nil
	}
	head, sps, err := s.checkHW(dec)
	if err != nil {
		slog.Info("video: decoding on the cores", "id", s.id, "why", s.scrub(err))
		dec.stop()
		return false, nil, nil, nil
	}
	// In the daemon, into the screen's own buffers (hw_vde.go); GStreamer where that cannot start.
	if bufs := s.screen.Bufs; bufs != nil && os.Getenv("TECHO5_VIDEO_DECODER") != "gst" {
		started, readErr, err := s.playVDE(dec, bufs, head, sps, info)
		if started {
			return true, readErr, dec, nil
		}
		slog.Info("video: the decoder in the daemon did not start; GStreamer then", "id", s.id, "err", err)
	}
	frames, gst, err := s.startHW(dec, head)
	if err != nil {
		slog.Info("video: decoding on the cores", "id", s.id, "why", s.scrub(err))
		dec.stop()
		return false, nil, nil, nil
	}
	w, h, size := hwFrame(info)
	fit := FitIn(info, s.screen)
	s.mu.Lock()
	s.dec = dec
	s.hw = &hwPicture{w: w, h: h, size: size, dst: image.Rect(fit.X, fit.Y, fit.X+fit.W, fit.Y+fit.H)}
	s.rate = ExactRate(info.FPS)
	s.most = max(queueBytes/size, 3)
	s.mu.Unlock()
	slog.Info("video: decoding in hardware", "id", s.id, "decoder", hwDecoderName, "picture", itoa(w)+"x"+itoa(h), "rate", s.rate.String())
	s.startSound(dec)
	readErr = s.readFrames(frames)
	// GStreamer's pictures ended: it finished or failed. Failed, ffmpeg is stopped too, or it would wait
	// forever on a picture nobody takes, and the video ends with GStreamer's reason.
	select {
	case <-gst.done:
	case <-time.After(2 * time.Second):
	case <-s.ctx.Done():
	}
	if gerr := gst.exitErr(); gerr != nil && s.ctx.Err() == nil {
		dec.stop()
		readErr = fmt.Errorf("the hardware decoder: %w", gst.why())
	}
	return true, readErr, dec, gst
}

// exitErr is how the run ended, nil while it is still running or if it ended well.
func (d *decoder) exitErr() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}

// hwPicture is the hardware's pictures: their size, bytes, and where on the panel they go.
type hwPicture struct {
	w, h, size int
	dst        image.Rectangle
}

// stopAll ends both processes of a hardware run.
func stopAll(ds ...*decoder) {
	for _, d := range ds {
		if d != nil {
			d.stop()
		}
	}
}

// hwWhy is what went wrong in a hardware run: GStreamer's failure if it failed, else ffmpeg's.
func hwWhy(gst, dec *decoder) error {
	if gst != nil {
		select {
		case <-gst.done:
			if gst.err != nil {
				return fmt.Errorf("the hardware decoder: %w", gst.why())
			}
		default:
		}
	}
	return dec.why()
}
