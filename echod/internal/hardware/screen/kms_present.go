//go:build !dot && !panel

package screen

import (
	"image"

	"github.com/HuskerMinion/techo5/echod/internal/lib/memx"
)

// Present on KMS (the Surface RT). The pages are XBGR8888, which is the canvas's own byte order
// (red first in memory), so a row goes to the page as it is, never converted. Which rows to write is
// told by a checksum of each row of each page (lib/memx) rather than a copy of the page: a frame
// reads the canvas once, and a copy to compare against would be read as well, twice the memory at a
// third of a gigabyte a second. One core: a second makes it only 15% quicker for twice the CPU time.

const fourccXBGR8888 = 0x34324258 // 'XB24'

// presentSums puts img on page dst (page next), writing the rows whose checksum differs from what the
// page holds.
func (d *Device) presentSums(img *image.RGBA, dst []byte, next int) {
	if len(d.sums) != d.pages {
		d.sums = make([][]uint64, d.pages)
		d.sumsOK = make([]bool, d.pages)
	}
	if len(d.sums[next]) != d.panelH {
		d.sums[next] = make([]uint64, d.panelH)
		d.sumsOK[next] = false
	}
	whole := d.takeStale(next) || !d.sumsOK[next]
	d.sumsOK[next] = true
	h := min(img.Rect.Dy(), d.panelH)
	n := min(img.Rect.Dx(), d.panelW) * 4
	sums := d.sums[next]
	// A row the page on show already has goes from there by the 2D engine, rather than from the canvas
	// by a core: the frame after one that changed much changes that much again on this page.
	var shown []uint64
	var g *gr2d
	if front := d.page; !whole && front != next && front < len(d.sumsOK) && d.sumsOK[front] && d.kms != nil {
		if g = d.kms.gpu2D(); g != nil {
			shown = d.sums[front]
		}
	}
	runs := d.runs[:0]
	gpuRows := 0
	for y := range h {
		src := img.Pix[y*img.Stride : y*img.Stride+n]
		s := memx.RowSum(src)
		if !whole && s == sums[y] {
			continue
		}
		sums[y] = s
		if shown != nil && shown[y] == s {
			if k := len(runs) - 1; k >= 0 && runs[k][1] == y {
				runs[k][1] = y + 1
			} else {
				runs = append(runs, [2]int{y, y + 1})
			}
			gpuRows++
			continue
		}
		memx.Copy(dst[y*d.line:y*d.line+n], src)
	}
	d.runs = runs
	if gpuRows == 0 {
		return
	}
	if gpuRows >= gr2dMinRows {
		g.copyRows(next, d.page, runs, n/4, d.line)
		err := g.run()
		if err == nil {
			return
		}
		d.kms.g2dFail(err)
	}
	for _, r := range runs {
		for y := r[0]; y < r[1]; y++ {
			memx.Copy(dst[y*d.line:y*d.line+n], img.Pix[y*img.Stride:y*img.Stride+n])
		}
	}
}

// gr2dMinRows is the fewest rows worth a 2D engine job: fewer, the cores write them quicker than a job
// is submitted and waited for.
const gr2dMinRows = 32
