//go:build !dot && !spot

package display

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
)

// A board chosen as the idle screen stands in for the clock: not over the music, away for a while once
// put away by hand, and back after a board turned to by hand has been left alone.
func TestIdleBoard(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	was := layout.Board
	layout.Board = "surface-rt"
	t.Cleanup(func() { layout.Board = was })

	if got := idleBoardOptions(); !slices.Equal(got, []string{"Clock", "Work", "Sky"}) {
		t.Fatalf("options %v", got)
	}
	d := &Display{poke: make(chan struct{}, 1)}
	shown := func(nowPlaying bool) string {
		s := scene{now: time.Now(), phase: "idle", nowPlaying: nowPlaying}
		d.boardScene(&s, s.now)
		return s.board.name
	}
	if got := shown(false); got != "" {
		t.Fatalf("no idle board chosen, %q shown", got)
	}

	d.setIdleBoard(1)
	if got := config.Get().Screen.IdleBoard; got != boardWork {
		t.Fatalf("saved %q", got)
	}
	if got := shown(false); got != boardWork {
		t.Fatalf("idle board Work, %q shown", got)
	}
	if got := shown(true); got != "" {
		t.Fatalf("music playing, %q shown over it", got)
	}

	d.closeBoard()
	if got := shown(false); got != "" {
		t.Fatalf("put away, %q shown", got)
	}
	d.mu.Lock()
	d.boards.awayUntil = time.Now().Add(-time.Second)
	d.mu.Unlock()
	if got := shown(false); got != boardWork {
		t.Fatalf("away over, %q shown", got)
	}

	d.openBoard(boardSky)
	if got := shown(false); got != boardSky {
		t.Fatalf("Sky opened by hand, %q shown", got)
	}
	d.mu.Lock()
	d.boards.touched = time.Now().Add(-time.Hour)
	d.mu.Unlock()
	if got := shown(false); got != boardWork {
		t.Fatalf("Sky left alone, %q shown rather than the idle board", got)
	}

	// A board this build does not have is the clock.
	if err := config.Set().Screen().IdleBoard("mail"); err != nil {
		t.Fatal(err)
	}
	if got := shown(false); got != "" || idleBoardIndex() != 0 {
		t.Fatalf("unknown idle board, %q shown, index %d", got, idleBoardIndex())
	}
}
