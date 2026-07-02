package notty

import (
	"context"
	"strings"
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

func TestValidateEmailConfigRequiresMailgunWhenStrict(t *testing.T) {
	if err := (Config{}).ValidateEmailConfig(); err != nil {
		t.Fatalf("default config should allow noop email sender: %v", err)
	}

	err := (Config{RequireEmail: true}).ValidateEmailConfig()
	if err == nil {
		t.Fatalf("expected strict email config to reject missing Mailgun settings")
	}
	if !strings.Contains(err.Error(), "NOTTY_REQUIRE_EMAIL") {
		t.Fatalf("expected strict config error to mention NOTTY_REQUIRE_EMAIL, got %q", err.Error())
	}

	cfg := Config{
		RequireEmail:  true,
		MailgunDomain: " mg.example.com ",
		MailgunAPIKey: " key ",
		MailgunFrom:   " noreply@example.com ",
	}
	if err := cfg.ValidateEmailConfig(); err != nil {
		t.Fatalf("strict config should accept complete Mailgun settings: %v", err)
	}
}

func TestValidateEmailConfigRejectsInvalidRequireEmailFlag(t *testing.T) {
	t.Setenv("NOTTY_REQUIRE_EMAIL", "treu")
	cfg := LoadConfig()
	err := cfg.ValidateEmailConfig()
	if err == nil {
		t.Fatalf("expected invalid NOTTY_REQUIRE_EMAIL value to fail validation")
	}
	if !strings.Contains(err.Error(), "NOTTY_REQUIRE_EMAIL") {
		t.Fatalf("expected invalid flag error to mention NOTTY_REQUIRE_EMAIL, got %q", err.Error())
	}
}
