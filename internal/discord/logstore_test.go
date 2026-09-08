package discord

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

const testLogDay = "2026-09-07"

func testRecipient(t *testing.T) (recipient, identityPath string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	identityPath = filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return identity.Recipient().String(), identityPath
}

func at(t *testing.T, stamp string) time.Time {
	t.Helper()
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("parse %q: %v", stamp, err)
	}
	return when.UTC()
}

func loggedMessage(id string, when time.Time) Event {
	return Event{
		Kind:      EventMessageCreate,
		At:        when,
		ChannelID: "400000000000000001",
		MessageID: id,
		AuthorID:  testOperatorID,
		Content:   "message " + id,
	}
}

func messageIDs(events []Event) []string {
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.MessageID)
	}
	return ids
}

func readDay(t *testing.T, identityPath, dir, day string) []Event {
	t.Helper()
	log, err := ReadDay(identityPath, dir, at(t, day+"T00:00:00Z"))
	if err != nil {
		t.Fatalf("read day %s: %v", day, err)
	}
	if log.Break != nil {
		t.Fatalf("read day %s broke in %s: %v", day, log.Truncated, log.Break)
	}
	return log.Events
}

func segmentNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestLogStoreEncryptsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	now := at(t, "2026-09-07T14:00:00Z")
	store, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}

	store.Append(loggedMessage("1", at(t, "2026-09-06T13:30:00Z")))
	store.Append(loggedMessage("2", at(t, "2026-09-07T13:30:00Z")))
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	want := []string{"events-2026-09-06T13-0001.jsonl.age", "events-2026-09-07T13-0001.jsonl.age"}
	if got := segmentNames(t, dir); !slices.Equal(got, want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	for _, name := range want {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(content), "message ") {
			t.Fatalf("%s holds the event in plaintext", name)
		}
	}

	if got := messageIDs(readDay(t, identityPath, dir, "2026-09-06")); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("2026-09-06 = %v", got)
	}
	if got := messageIDs(readDay(t, identityPath, dir, testLogDay)); !slices.Equal(got, []string{"2"}) {
		t.Fatalf("%s = %v", testLogDay, got)
	}

	if err := store.Prune(24 * time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := readDay(t, identityPath, dir, "2026-09-06"); len(got) != 0 {
		t.Fatalf("pruned day still holds %v", messageIDs(got))
	}
	if got := messageIDs(readDay(t, identityPath, dir, testLogDay)); !slices.Equal(got, []string{"2"}) {
		t.Fatalf("prune took the newer segment: %v", got)
	}
}

func TestLogStoreSurvivesAbruptStop(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	now := at(t, "2026-09-07T15:00:00Z")
	store, err := NewLogStore(LogStoreConfig{
		Dir:           dir,
		Recipient:     recipient,
		FlushInterval: 5 * time.Millisecond,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()

	store.Append(
		loggedMessage("1", at(t, "2026-09-07T13:10:00Z")),
		loggedMessage("2", at(t, "2026-09-07T13:50:00Z")),
		loggedMessage("3", at(t, "2026-09-07T14:05:00Z")),
		loggedMessage("4", at(t, "2026-09-07T14:20:00Z")),
	)

	deadline := time.Now().Add(2 * time.Second)
	var flushed []Event
	for time.Now().Before(deadline) {
		flushed = readDay(t, identityPath, dir, testLogDay)
		if len(flushed) == 4 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := messageIDs(flushed); !slices.Equal(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("the timer flushed %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	crashed, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	crashed.Append(loggedMessage("5", at(t, "2026-09-07T14:40:00Z")))

	if got := messageIDs(readDay(t, identityPath, dir, testLogDay)); !slices.Equal(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("after the abrupt stop the day holds %v", got)
	}
	if names := segmentNames(t, dir); len(names) != 2 {
		t.Fatalf("hourly segments = %v", names)
	}
}

func TestLogStoreRunFlushesOnShutdown(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	now := at(t, "2026-09-07T15:00:00Z")
	store, err := NewLogStore(LogStoreConfig{
		Dir:           dir,
		Recipient:     recipient,
		FlushInterval: time.Hour,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	store.Append(loggedMessage("1", at(t, "2026-09-07T14:59:00Z")))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := messageIDs(readDay(t, identityPath, dir, testLogDay)); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("shutdown flush wrote %v", got)
	}
}

func TestLogStoreReadsTruncatedNewestSegment(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	now := at(t, "2026-09-07T15:00:00Z")
	store, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}

	store.Append(loggedMessage("first", at(t, "2026-09-07T13:00:00Z")))
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	const bulk = 4000
	for i := range bulk {
		event := loggedMessage(strings.Repeat("0", 8)+strconv.Itoa(i), at(t, "2026-09-07T14:00:00Z"))
		store.Append(event)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	newest := filepath.Join(dir, "events-2026-09-07T14-0001.jsonl.age")
	info, err := os.Stat(newest)
	if err != nil {
		t.Fatalf("stat newest segment: %v", err)
	}
	if err := os.Truncate(newest, info.Size()/2); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	day, err := ReadDay(identityPath, dir, at(t, testLogDay+"T00:00:00Z"))
	if err != nil {
		t.Fatalf("a truncated newest segment must read as a usable tail: %v", err)
	}
	if day.Truncated != filepath.Base(newest) || day.Break == nil {
		t.Fatalf("the tolerated break went unreported: %+v", day)
	}
	events := day.Events
	if len(events) < 2 || len(events) > bulk {
		t.Fatalf("truncated read returned %d events", len(events))
	}
	if events[0].MessageID != "first" {
		t.Fatalf("first event = %q", events[0].MessageID)
	}
	for i, e := range events[1:] {
		if want := strings.Repeat("0", 8) + strconv.Itoa(i); e.MessageID != want {
			t.Fatalf("event %d = %q, want %q", i+1, e.MessageID, want)
		}
	}

	oldest := filepath.Join(dir, "events-2026-09-07T13-0001.jsonl.age")
	oldestInfo, err := os.Stat(oldest)
	if err != nil {
		t.Fatalf("stat oldest segment: %v", err)
	}
	if err := os.Truncate(oldest, oldestInfo.Size()-8); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := ReadDay(identityPath, dir, at(t, testLogDay+"T00:00:00Z")); err == nil {
		t.Fatal("a corrupt older segment must be an error, not a silent gap")
	}
}

func TestLogStorePruneKeepsAnHourInsideTheRetention(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	now := at(t, "2026-09-07T14:00:00Z")
	store, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	store.Append(loggedMessage("1", at(t, "2026-09-07T13:59:00Z")))
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := store.Prune(time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := messageIDs(readDay(t, identityPath, dir, testLogDay)); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("prune took an event one minute old: %v", got)
	}
}

func writeOneSegment(t *testing.T, dir, recipient string) {
	t.Helper()
	store, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return at(t, "2026-09-07T14:00:00Z") },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	store.Append(loggedMessage("1", at(t, "2026-09-07T13:30:00Z")))
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestReadDayRefusesAnIdentityThatCannotDecrypt(t *testing.T) {
	dir := t.TempDir()
	recipient, _ := testRecipient(t)
	writeOneSegment(t, dir, recipient)
	_, otherIdentity := testRecipient(t)

	day, err := ReadDay(otherIdentity, dir, at(t, testLogDay+"T00:00:00Z"))
	if err == nil {
		t.Fatalf("the wrong identity read the day as %d events and no error", len(day.Events))
	}
}

func TestReadDayRefusesAMissingDirectory(t *testing.T) {
	_, identityPath := testRecipient(t)
	missing := filepath.Join(t.TempDir(), "not-the-log-directory")
	if _, err := ReadDay(identityPath, missing, at(t, testLogDay+"T00:00:00Z")); err == nil {
		t.Fatal("a log directory that is not there must be an error, not an empty day")
	}
}

func TestReadDayRefusesANewestSegmentWithNoAgeHeader(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	writeOneSegment(t, dir, recipient)
	newest := filepath.Join(dir, "events-2026-09-07T15-0001.jsonl.age")
	if err := os.WriteFile(newest, nil, 0o600); err != nil {
		t.Fatalf("write empty segment: %v", err)
	}

	if _, err := ReadDay(identityPath, dir, at(t, testLogDay+"T00:00:00Z")); err == nil {
		t.Fatal("a newest segment that is not an age file must be an error, not a truncated tail")
	}
}

func TestReadDayRefusesAnUnreadableNewestSegment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	writeOneSegment(t, dir, recipient)
	newest := filepath.Join(dir, "events-2026-09-07T15-0001.jsonl.age")
	if err := os.WriteFile(newest, nil, 0o000); err != nil {
		t.Fatalf("write unreadable segment: %v", err)
	}

	if _, err := ReadDay(identityPath, dir, at(t, testLogDay+"T00:00:00Z")); err == nil {
		t.Fatal("a segment that cannot be opened must be an error, not a truncated tail")
	}
}

func TestReadDayRefusesAGarbledLineInTheNewestSegment(t *testing.T) {
	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	writeOneSegment(t, dir, recipient)

	newest := filepath.Join(dir, "events-2026-09-07T15-0001.jsonl.age")
	file, err := os.OpenFile(newest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	parsed, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatalf("parse recipient: %v", err)
	}
	writer, err := age.Encrypt(file, parsed)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := writer.Write([]byte("this is not an event\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}

	if _, err := ReadDay(identityPath, dir, at(t, testLogDay+"T00:00:00Z")); err == nil {
		t.Fatal("a whole line that is not an event must be an error, not a truncated tail")
	}
}

func TestLogStorePublishesSegmentsWholeAndPrunesOrphans(t *testing.T) {
	dir := t.TempDir()
	recipient, _ := testRecipient(t)
	store, err := NewLogStore(LogStoreConfig{
		Dir:       dir,
		Recipient: recipient,
		Now:       func() time.Time { return at(t, "2026-09-07T15:00:00Z") },
	})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	store.Append(loggedMessage("1", at(t, "2026-09-07T14:30:00Z")))
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := segmentNames(t, dir); !slices.Equal(got, []string{"events-2026-09-07T14-0001.jsonl.age"}) {
		t.Fatalf("a finished flush left %v", got)
	}

	orphan := filepath.Join(dir, "events-2026-09-07T13-0001.jsonl.age.partial")
	if err := os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	if err := store.Prune(time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := segmentNames(t, dir); !slices.Equal(got, []string{"events-2026-09-07T14-0001.jsonl.age"}) {
		t.Fatalf("prune left %v", got)
	}
}
