package discord

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// backfillLimit is Discord's maximum page for Get Channel Messages.
const backfillLimit = 100

// Logger persists every gateway event. Discord logs no message send, so this
// store is the farm's only record of what was said; a dropped connection is
// covered by reading each channel's messages after the last id the logger
// saw, which is also why a message that arrives twice is stored once. Run
// only appends: the store reaches disk through its own Run, which the
// supervisor starts alongside this one.
type Logger struct {
	Client Client
	Store  *LogStore
}

func (l *Logger) Run(ctx context.Context, events <-chan Event) error {
	lastSeen := make(map[string]string)
	for {
		stopped := l.consume(ctx, events, lastSeen)
		if stopped {
			return nil
		}
		if err := l.backfill(ctx, lastSeen); err != nil {
			return err
		}
		next, err := l.Client.Events(ctx)
		if err != nil {
			return fmt.Errorf("resubscribe to the gateway: %w", err)
		}
		events = next
	}
}

func (l *Logger) consume(ctx context.Context, events <-chan Event, lastSeen map[string]string) bool {
	for {
		select {
		case <-ctx.Done():
			return true
		case event, open := <-events:
			if !open {
				return ctx.Err() != nil
			}
			l.record(event, lastSeen)
		}
	}
}

func (l *Logger) record(event Event, lastSeen map[string]string) {
	if event.Kind == EventMessageCreate && event.ChannelID != "" && event.MessageID != "" {
		if !laterID(event.MessageID, lastSeen[event.ChannelID]) {
			return
		}
		lastSeen[event.ChannelID] = event.MessageID
	}
	l.Store.Append(event)
}

func (l *Logger) backfill(ctx context.Context, lastSeen map[string]string) error {
	for _, channelID := range slices.Sorted(maps.Keys(lastSeen)) {
		for {
			after := lastSeen[channelID]
			messages, err := l.Client.Messages(ctx, channelID, after, backfillLimit)
			if err != nil {
				return fmt.Errorf("backfill channel %s: %w", channelID, err)
			}
			slices.SortFunc(messages, func(a, b Message) int { return compareIDs(a.ID, b.ID) })
			for _, message := range messages {
				l.record(messageFromHistory(message), lastSeen)
			}
			if len(messages) < backfillLimit || lastSeen[channelID] == after {
				break
			}
		}
	}
	return nil
}

func messageFromHistory(m Message) Event {
	return Event{
		Kind:      EventMessageCreate,
		At:        m.CreatedAt.UTC(),
		ChannelID: m.ChannelID,
		MessageID: m.ID,
		AuthorID:  m.AuthorID,
		Content:   m.Content,
	}
}

func laterID(id, than string) bool { return compareIDs(id, than) > 0 }

// compareIDs orders snowflakes, which are ascending integers of unequal width.
func compareIDs(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}
