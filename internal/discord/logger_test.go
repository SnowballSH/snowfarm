package discord

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

const testLogChannel = "400000000000000001"

func messageID(n int) string { return fmt.Sprintf("50000000000000%04d", n) }

func gatewayMessage(n int, when time.Time) Event {
	return Event{
		Kind:      EventMessageCreate,
		At:        when,
		ChannelID: testLogChannel,
		MessageID: messageID(n),
		AuthorID:  testOperatorID,
		Content:   "message",
	}
}

// scriptedGateway is a client whose gateway session restarts the way the
// production one does: the stream stays open across the drop and the restart
// arrives on it as an event, so a test can assert both the backfill and that
// the logger never subscribes a second time.
type scriptedGateway struct {
	*fakeClient

	events        chan Event
	subscriptions atomic.Int64
}

func (g *scriptedGateway) Events(context.Context) (<-chan Event, error) {
	g.subscriptions.Add(1)
	return g.events, nil
}

func kindsOf(events []Event, kind EventKind) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func waitForMessages(t *testing.T, store *LogStore, identityPath, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := store.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(kindsOf(readDay(t, identityPath, dir, "2026-09-07"), EventMessageCreate)) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestLoggerBackfillsGap(t *testing.T) {
	when := at(t, "2026-09-07T13:00:00Z")
	fake := newFake()
	for i := 1; i <= 6; i++ {
		fake.messages[testLogChannel] = append(fake.messages[testLogChannel], Message{
			ID:        messageID(i),
			ChannelID: testLogChannel,
			AuthorID:  testOperatorID,
			Content:   "message",
			CreatedAt: when,
		})
	}
	gateway := &scriptedGateway{fakeClient: fake, events: make(chan Event, 8)}

	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	store, err := NewLogStore(LogStoreConfig{Dir: dir, Recipient: recipient})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	logger := &Logger{Client: gateway, Store: store}

	stream, err := gateway.Events(t.Context())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- logger.Run(ctx, stream) }()

	for i := 1; i <= 3; i++ {
		gateway.events <- gatewayMessage(i, when)
	}
	gateway.events <- Event{Kind: EventGatewayResume, At: when}
	gateway.events <- gatewayMessage(6, when)

	want := []string{messageID(1), messageID(2), messageID(3), messageID(4), messageID(5), messageID(6)}
	waitForMessages(t, store, identityPath, dir, len(want))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	logged := readDay(t, identityPath, dir, "2026-09-07")
	if got := messageIDs(kindsOf(logged, EventMessageCreate)); !slices.Equal(got, want) {
		t.Fatalf("logged %v, want %v", got, want)
	}
	if got := len(kindsOf(logged, EventGatewayResume)); got != 1 {
		t.Fatalf("the log holds %d resume events, want 1", got)
	}
	if got := gateway.subscriptions.Load(); got != 1 {
		t.Fatalf("the gateway was subscribed %d times: a second subscription doubles every delivery", got)
	}
}

func TestLoggerRefusesAStreamThatEndsEarly(t *testing.T) {
	dir := t.TempDir()
	recipient, _ := testRecipient(t)
	store, err := NewLogStore(LogStoreConfig{Dir: dir, Recipient: recipient})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	gateway := &scriptedGateway{fakeClient: newFake(), events: make(chan Event)}
	logger := &Logger{Client: gateway, Store: store}

	close(gateway.events)
	if err := logger.Run(t.Context(), gateway.events); err == nil {
		t.Fatal("a stream that ends while the logger runs must be an error, not a clean stop")
	}
	if got := gateway.subscriptions.Load(); got != 0 {
		t.Fatalf("the logger resubscribed %d times", got)
	}
}

func TestPosterSuppressesNotifications(t *testing.T) {
	fake := newFake()
	poster := &Poster{Client: fake, StatusChannelID: testLogChannel}
	if err := poster.Status(t.Context(), "the farm is up"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := poster.Mention(t.Context(), "the farm needs you", testOperatorID); err != nil {
		t.Fatalf("mention: %v", err)
	}
	if len(fake.sends) != 2 {
		t.Fatalf("sends = %v", fake.sends)
	}
	status := fake.sends[0]
	if status.channelID != testLogChannel || status.content != "the farm is up" || !status.suppressed {
		t.Fatalf("status send = %+v", status)
	}
	mention := fake.sends[1]
	if mention.channelID != testLogChannel || mention.suppressed {
		t.Fatalf("a mention that suppresses its notification cannot reach anyone: %+v", mention)
	}
	if mention.content != "<@"+testOperatorID+"> the farm needs you" {
		t.Fatalf("mention content = %q", mention.content)
	}
}

func TestPosterToPostsToGivenChannel(t *testing.T) {
	fake := newFake()
	poster := &Poster{Client: fake, StatusChannelID: testLogChannel}
	const thread = "400000000000000099"
	if err := poster.To(t.Context(), thread, "the card came back"); err != nil {
		t.Fatalf("to: %v", err)
	}
	if len(fake.sends) != 1 {
		t.Fatalf("sends = %v", fake.sends)
	}
	if got := fake.sends[0]; got.channelID != thread || !got.suppressed {
		t.Fatalf("To posted %+v", got)
	}
}
