package claude

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSlotsSerialize(t *testing.T) {
	h := newHarness(t)
	dir := h.config().SlotsDir()

	first, err := AcquireSlot(dir, 200*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireSlot(dir, 200*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.Number() == second.Number() {
		t.Fatalf("both acquisitions took slot %d", first.Number())
	}

	start := time.Now()
	third, err := AcquireSlot(dir, 200*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("third acquisition: %v (slot %v), want ErrNoSlot", err, third)
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Errorf("third acquisition gave up after %v, want at least the 200ms deadline", waited)
	}
	if code := h.run("-p", "print the git HEAD"); code != ExitNoSlot {
		t.Errorf("wrapper exit %d with every slot held, want %d", code, ExitNoSlot)
	}
	if h.ranClaude() {
		t.Error("the wrapper execed claude without a slot")
	}
	if lines := h.records(); len(lines) != 0 {
		t.Errorf("a run that never started was recorded: %v", lines)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	fourth, err := AcquireSlot(dir, 200*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("after a release: %v", err)
	}
	if fourth.Number() != first.Number() {
		t.Errorf("took slot %d after slot %d was released", fourth.Number(), first.Number())
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := fourth.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSlotsWithoutFilesAreLoud(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	slot, err := AcquireSlot(dir, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatalf("an empty slot directory handed out slot %d", slot.Number())
	}
	if errors.Is(err, ErrNoSlot) {
		t.Fatalf("an empty slot directory reported a busy farm: %v", err)
	}

	if _, err := AcquireSlot(filepath.Join(t.TempDir(), "absent"), time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("a missing slot directory was silently accepted")
	}

	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Symlink(gone, filepath.Join(dir, "1")); err != nil {
		t.Fatal(err)
	}
	if slot, err := AcquireSlot(dir, time.Millisecond, time.Millisecond); err == nil {
		t.Errorf("a slot entry whose file is gone handed out slot %d", slot.Number())
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("AcquireSlot created the slot file, so the lock was private to this run")
	}
}
