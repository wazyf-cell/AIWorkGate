//go:build windows

package tray

import (
	"encoding/binary"
	"testing"
)

func TestLabels(t *testing.T) {
	running := Labels(false)
	if running.Pause != TitlePauseAll {
		t.Errorf("running pause title = %q, want %q", running.Pause, TitlePauseAll)
	}
	if !running.PauseEnabled {
		t.Error("pause item must be enabled while running")
	}
	if running.ResumeEnabled {
		t.Error("resume item must be disabled while running")
	}
	if running.Resume != TitleResumeAll || running.OpenUI != TitleOpenUI || running.Quit != TitleQuit {
		t.Errorf("unexpected titles while running: %+v", running)
	}

	paused := Labels(true)
	if paused.Pause != TitlePausedAll {
		t.Errorf("paused pause title = %q, want %q", paused.Pause, TitlePausedAll)
	}
	if paused.PauseEnabled {
		t.Error("pause item must be disabled while paused")
	}
	if !paused.ResumeEnabled {
		t.Error("resume item must be enabled while paused")
	}
	if paused.Resume != TitleResumeAll {
		t.Errorf("paused resume title = %q, want %q", paused.Resume, TitleResumeAll)
	}
	// The paused state must be visible in the menu text.
	if running.Pause == paused.Pause {
		t.Error("menu text does not change between running and paused")
	}
}

func TestIconBytes(t *testing.T) {
	b := IconBytes()
	if len(b) != iconTotalBytes {
		t.Fatalf("icon length = %d, want %d", len(b), iconTotalBytes)
	}

	if got := binary.LittleEndian.Uint16(b[0:]); got != 0 {
		t.Errorf("ICONDIR reserved = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); got != 1 {
		t.Errorf("ICONDIR type = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(b[4:]); got != 1 {
		t.Errorf("ICONDIR count = %d, want 1", got)
	}

	entry := b[iconDirSize:]
	if entry[0] != iconSize || entry[1] != iconSize {
		t.Errorf("icon dimensions = %dx%d, want %dx%d", entry[0], entry[1], iconSize, iconSize)
	}
	if got := binary.LittleEndian.Uint16(entry[4:]); got != 1 {
		t.Errorf("entry planes = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(entry[6:]); got != 32 {
		t.Errorf("entry bit count = %d, want 32", got)
	}
	if got := binary.LittleEndian.Uint32(entry[8:]); int(got) != iconImageBytes {
		t.Errorf("entry image size = %d, want %d", got, iconImageBytes)
	}
	if got := binary.LittleEndian.Uint32(entry[12:]); int(got) != iconImageOffset {
		t.Errorf("entry image offset = %d, want %d", got, iconImageOffset)
	}

	header := b[iconImageOffset:]
	if got := binary.LittleEndian.Uint32(header[0:]); got != iconHeaderSize {
		t.Errorf("BITMAPINFOHEADER size = %d, want %d", got, iconHeaderSize)
	}
	if got := binary.LittleEndian.Uint32(header[4:]); int(got) != iconSize {
		t.Errorf("bitmap width = %d, want %d", got, iconSize)
	}
	if got := binary.LittleEndian.Uint32(header[8:]); int(got) != iconSize*2 {
		t.Errorf("bitmap height = %d, want %d (XOR image plus AND mask)", got, iconSize*2)
	}
	if got := binary.LittleEndian.Uint16(header[12:]); got != 1 {
		t.Errorf("bitmap planes = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(header[14:]); got != 32 {
		t.Errorf("bitmap bit count = %d, want 32", got)
	}
	if got := binary.LittleEndian.Uint32(header[16:]); got != 0 {
		t.Errorf("bitmap compression = %d, want 0 (BI_RGB)", got)
	}

	// A visible icon needs both transparent and opaque pixels.
	pixels := b[iconImageOffset+iconHeaderSize : iconImageOffset+iconHeaderSize+iconXORBytes]
	var transparent, opaque int
	for i := 3; i < len(pixels); i += 4 {
		switch pixels[i] {
		case 0:
			transparent++
		case 255:
			opaque++
		}
	}
	if transparent == 0 || opaque == 0 {
		t.Errorf("icon alpha channel looks empty (transparent=%d, opaque=%d)", transparent, opaque)
	}

	// Callers get a copy, so mutating it cannot corrupt later icons.
	b[0] = 0xFF
	if again := IconBytes(); again[0] != 0 {
		t.Error("IconBytes returned a shared buffer")
	}
}

func TestQuitBeforeRunIsSafe(t *testing.T) {
	// No Actions are registered yet, so this must neither panic nor post to a
	// non-existent window; quitOnce makes the repeat call a no-op too.
	Quit()
	Quit()
}

func TestSetPausedBeforeRunIsSafe(t *testing.T) {
	SetPaused(true)
	SetPaused(false)
}
