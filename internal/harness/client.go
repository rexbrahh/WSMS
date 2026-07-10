package harness

import "context"

// Message is a chat message for OpenAI-compatible stubs.
type Message struct {
	Role    string
	Content string
}

// Client is the model interface. Scaffold uses NoopClient.
type Client interface {
	Chat(ctx context.Context, messages []Message) (string, error)
}

// NoopClient returns a fixed stub without network I/O.
type NoopClient struct {
	Reply string
}

func (c *NoopClient) Chat(ctx context.Context, messages []Message) (string, error) {
	_ = ctx
	_ = messages
	if c.Reply != "" {
		return c.Reply, nil
	}
	return "noop", nil
}
