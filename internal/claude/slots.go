package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNoSlot is the farm being busy rather than broken: every slot was held
// for the whole deadline.
var ErrNoSlot = errors.New("no Claude Code slot came free")

// Slot is one held place in the farm's concurrency budget. The lock lives on
// the open file description, so it is released by Release and by the process
// dying, and by nothing else.
type Slot struct {
	number int
	file   *os.File
}

func (s *Slot) Number() int { return s.number }

func (s *Slot) Release() error { return s.file.Close() }

// AcquireSlot takes the first free slot file in dir, polling until timeout.
// The count comes from the directory apply populates from
// guard.max_claude_slots; a slot file is never created here, so a missing one
// is a loud error rather than a lock nobody else can see.
func AcquireSlot(dir string, timeout, poll time.Duration) (*Slot, error) {
	numbers, err := slotNumbers(dir)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		for _, number := range numbers {
			slot, err := trySlot(dir, number)
			if err != nil {
				return nil, err
			}
			if slot != nil {
				return slot, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w within %s", ErrNoSlot, timeout)
		}
		time.Sleep(min(poll, remaining))
	}
}

func slotNumbers(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, entry := range entries {
		number, err := strconv.Atoi(entry.Name())
		if err != nil || entry.IsDir() {
			continue
		}
		numbers = append(numbers, number)
	}
	if len(numbers) == 0 {
		return nil, fmt.Errorf("%s holds no slot file: apply creates one per guard.max_claude_slots", dir)
	}
	slices.Sort(numbers)
	return numbers, nil
}

func trySlot(dir string, number int) (*Slot, error) {
	path := filepath.Join(dir, strconv.Itoa(number))
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, closeErr
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return &Slot{number: number, file: file}, nil
}
