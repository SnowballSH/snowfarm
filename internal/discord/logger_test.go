package discord

import (
	"context"
	"fmt"
	"slices"
	"sync"
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

// scriptedGateway is a gateway that drops its connection: each Events call
// delivers one scripted batch and closes, so the logger must resubscribe and
// backfill what it missed in between.
type scriptedGateway struct {
	*fakeClient

	mu      sync.Mutex
	batches [][]Event
}

func (g *scriptedGateway) Events(ctx context.Context) (<-chan Event, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.batches) == 0 {
		ch := make(chan Event)
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	batch := g.batches[0]
	g.batches = g.batches[1:]
	ch := make(chan Event, len(batch))
	for _, e := range batch {
		ch <- e
	}
	close(ch)
	return ch, nil
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
	gateway := &scriptedGateway{fakeClient: fake, batches: [][]Event{
		{gatewayMessage(1, when), gatewayMessage(2, when), gatewayMessage(3, when)},
		{gatewayMessage(6, when)},
	}}

	dir := t.TempDir()
	recipient, identityPath := testRecipient(t)
	store, err := NewLogStore(LogStoreConfig{Dir: dir, Recipient: recipient})
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	logger := &Logger{Client: gateway, Store: store}

	first, err := gateway.Events(t.Context())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- logger.Run(ctx, first) }()

	want := []string{messageID(1), messageID(2), messageID(3), messageID(4), messageID(5), messageID(6)}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := store.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(readDay(t, identityPath, dir, "2026-09-07")) == len(want) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := messageIDs(readDay(t, identityPath, dir, "2026-09-07")); !slices.Equal(got, want) {
		t.Fatalf("logged %v, want %v", got, want)
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
