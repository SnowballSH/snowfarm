package discord

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
)

const (
	logSegmentPrefix     = "events-"
	logSegmentSuffix     = ".jsonl.age"
	logSegmentHourLayout = "2006-01-02T15"
	logSegmentDayLayout  = "2006-01-02"
	maxSegmentsPerHour   = 9999
	defaultFlushInterval = time.Minute
	logDirMode           = fs.FileMode(0o700)
	logSegmentMode       = fs.FileMode(0o600)
)

// LogStoreConfig configures a store; Recipient is the age public key its
// segments are encrypted to.
type LogStoreConfig struct {
	Dir           string
	Recipient     string
	FlushInterval time.Duration
	Now           func() time.Time
}

// LogStore keeps the gateway's events encrypted at rest. An age file cannot
// be appended to and its payload authenticates whole 64 KiB chunks, so a
// process that dies with a stream open loses everything written into it:
// every flush therefore writes, closes and fsyncs its own complete file, and
// an abrupt stop costs at most the events buffered since the last one.
type LogStore struct {
	dir       string
	recipient age.Recipient
	interval  time.Duration
	now       func() time.Time

	mu      sync.Mutex
	pending []Event
}

func NewLogStore(cfg LogStoreConfig) (*LogStore, error) {
	if cfg.Dir == "" {
		return nil, errors.New("log store: no directory")
	}
	recipient, err := age.ParseX25519Recipient(cfg.Recipient)
	if err != nil {
		return nil, fmt.Errorf("log store recipient: %w", err)
	}
	if err := os.MkdirAll(cfg.Dir, logDirMode); err != nil {
		return nil, fmt.Errorf("log store directory: %w", err)
	}
	store := &LogStore{
		dir:       cfg.Dir,
		recipient: recipient,
		interval:  cfg.FlushInterval,
		now:       cfg.Now,
	}
	if store.interval <= 0 {
		store.interval = defaultFlushInterval
	}
	if store.now == nil {
		store.now = func() time.Time { return time.Now().UTC() }
	}
	return store, nil
}

func (s *LogStore) Append(events ...Event) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range events {
		if e.At.IsZero() {
			e.At = s.now()
		}
		s.pending = append(s.pending, e)
	}
}

// Run flushes on the interval and once more when ctx ends, so a clean
// shutdown leaves no event in memory.
func (s *LogStore) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return s.Flush()
		case <-ticker.C:
			if err := s.Flush(); err != nil {
				return err
			}
		}
	}
}

func (s *LogStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	hours, byHour := groupByHour(s.pending)
	for i, hour := range hours {
		if err := s.writeSegment(hour, byHour[hour]); err != nil {
			s.pending = remaining(hours[i:], byHour)
			return err
		}
	}
	s.pending = nil
	return nil
}

// Prune deletes every segment whose hour cannot hold an event newer than the
// cutoff, so nothing inside a kept segment is younger than the retention.
func (s *LogStore) Prune(olderThan time.Duration) error {
	cutoff := s.now().UTC().Add(-olderThan)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		hour, _, ok := parseSegmentName(entry.Name())
		if !ok || hour.Add(time.Hour).After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (s *LogStore) writeSegment(hour string, events []Event) (err error) {
	file, path, err := s.createSegment(hour)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	writer, err := age.Encrypt(file, s.recipient)
	if err != nil {
		return fmt.Errorf("encrypt %s: %w", filepath.Base(path), err)
	}
	encoder := json.NewEncoder(writer)
	for _, event := range events {
		if err = encoder.Encode(event); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *LogStore) createSegment(hour string) (*os.File, string, error) {
	seq, err := s.nextSeq(hour)
	if err != nil {
		return nil, "", err
	}
	for ; seq <= maxSegmentsPerHour; seq++ {
		path := filepath.Join(s.dir, segmentName(hour, seq))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, logSegmentMode)
		switch {
		case errors.Is(err, fs.ErrExist):
			continue
		case err != nil:
			return nil, "", fmt.Errorf("create log segment: %w", err)
		}
		return file, path, nil
	}
	return nil, "", fmt.Errorf("hour %s already holds %d log segments", hour, maxSegmentsPerHour)
}

func (s *LogStore) nextSeq(hour string) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("read log directory: %w", err)
	}
	highest := 0
	for _, entry := range entries {
		at, seq, ok := parseSegmentName(entry.Name())
		if ok && at.UTC().Format(logSegmentHourLayout) == hour && seq > highest {
			highest = seq
		}
	}
	return highest + 1, nil
}

// ReadDay concatenates a day's segments in name order. A truncated newest
// segment is the tail a crash left behind: its whole lines are returned and
// the break is not an error, while a break anywhere earlier is corruption.
func ReadDay(identityPath, dir string, day time.Time) ([]Event, error) {
	identities, err := readIdentities(identityPath)
	if err != nil {
		return nil, err
	}
	names, err := daySegments(dir, day)
	if err != nil {
		return nil, err
	}
	var events []Event
	for i, name := range names {
		read, err := readSegment(filepath.Join(dir, name), identities)
		events = append(events, read...)
		if err != nil && i < len(names)-1 {
			return nil, fmt.Errorf("read log segment %s: %w", name, err)
		}
	}
	return events, nil
}

func readIdentities(path string) ([]age.Identity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open age identity: %w", err)
	}
	defer func() { _ = file.Close() }()
	identities, err := age.ParseIdentities(file)
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	return identities, nil
}

func readSegment(path string, identities []age.Identity) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	decrypted, err := age.Decrypt(file, identities...)
	if err != nil {
		return nil, err
	}
	var events []Event
	lines := bufio.NewReader(decrypted)
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return events, nil
			}
			return events, err
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return events, fmt.Errorf("decode event: %w", err)
		}
		events = append(events, event)
	}
}

func daySegments(dir string, day time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read log directory: %w", err)
	}
	prefix := logSegmentPrefix + day.UTC().Format(logSegmentDayLayout) + "T"
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, logSegmentSuffix) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func groupByHour(events []Event) ([]string, map[string][]Event) {
	byHour := make(map[string][]Event)
	var hours []string
	for _, event := range events {
		hour := event.At.UTC().Format(logSegmentHourLayout)
		if _, seen := byHour[hour]; !seen {
			hours = append(hours, hour)
		}
		byHour[hour] = append(byHour[hour], event)
	}
	slices.Sort(hours)
	return hours, byHour
}

func remaining(hours []string, byHour map[string][]Event) []Event {
	var events []Event
	for _, hour := range hours {
		events = append(events, byHour[hour]...)
	}
	return events
}

func segmentName(hour string, seq int) string {
	return fmt.Sprintf("%s%s-%04d%s", logSegmentPrefix, hour, seq, logSegmentSuffix)
}

func parseSegmentName(name string) (time.Time, int, bool) {
	if !strings.HasPrefix(name, logSegmentPrefix) || !strings.HasSuffix(name, logSegmentSuffix) {
		return time.Time{}, 0, false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, logSegmentPrefix), logSegmentSuffix)
	cut := strings.LastIndex(stem, "-")
	if cut < 0 {
		return time.Time{}, 0, false
	}
	hour, sequence := stem[:cut], stem[cut+1:]
	at, err := time.Parse(logSegmentHourLayout, hour)
	if err != nil {
		return time.Time{}, 0, false
	}
	seq, err := strconv.Atoi(sequence)
	if err != nil {
		return time.Time{}, 0, false
	}
	return at.UTC(), seq, true
}
