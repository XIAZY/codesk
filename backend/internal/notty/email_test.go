package notty

import (
	"context"
	"testing"
)

func TestEmailSenderFromConfigDefaultsToNoopWhenMailgunMissing(t *testing.T) {
	sender := emailSenderFromConfig(Config{})
	if _, ok := sender.(noopEmailSender); !ok {
		t.Fatalf("expected missing Mailgun config to use noop sender, got %T", sender)
	}
	if err := sender.SendEmail(context.Background(), EmailMessage{To: "dev@example.com"}); err != nil {
		t.Fatalf("noop email sender returned error: %v", err)
	}
}
