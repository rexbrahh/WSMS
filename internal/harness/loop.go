package harness

import "context"

// Loop is a minimal synchronous foreground agent loop stub.
type Loop struct {
	Session *Session
	Client  Client
}

// Turn injects the working-state capsule and calls the model client.
func (l *Loop) Turn(ctx context.Context, userText string) (assistant string, capsule string, err error) {
	if err := l.Session.IngestUser(ctx, userText); err != nil {
		return "", "", err
	}
	capsule, err = l.Session.BeforeTurn(ctx)
	if err != nil {
		return "", "", err
	}
	msgs := []Message{
		{Role: "system", Content: capsule},
		{Role: "user", Content: userText},
	}
	assistant, err = l.Client.Chat(ctx, msgs)
	if err != nil {
		return "", capsule, err
	}
	return assistant, capsule, l.Session.IngestAssistant(ctx, assistant)
}
