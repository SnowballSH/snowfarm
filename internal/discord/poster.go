package discord

import (
	"context"
	"fmt"
)

// Poster is how the supervisor speaks: status lines suppress their
// notification so a busy farm never rings a phone, and only a mention, which
// is addressed to a person, does not.
type Poster struct {
	Client          Client
	StatusChannelID string
}

func (p *Poster) Status(ctx context.Context, text string) error {
	return p.To(ctx, p.StatusChannelID, text)
}

func (p *Poster) To(ctx context.Context, channelID, text string) error {
	if _, err := p.Client.Send(ctx, channelID, text, true); err != nil {
		return err
	}
	return nil
}

func (p *Poster) Mention(ctx context.Context, text string, userID string) error {
	if _, err := p.Client.Send(ctx, p.StatusChannelID, fmt.Sprintf("<@%s> %s", userID, text), false); err != nil {
		return err
	}
	return nil
}
