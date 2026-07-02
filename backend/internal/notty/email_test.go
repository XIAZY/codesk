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

func TestBuildAccountEmailIncludesCodeskLogo(t *testing.T) {
	message := buildVerificationEmail("person@example.com", "https://app.getcodesk.com/account/verify-email?token=verify-token")

	for _, want := range []string{
		`<svg width="58" height="44" viewBox="14 31 72 38"`,
		`<circle cx="24" cy="50" r="9" fill="#E3A15B">`,
		`<circle cx="76" cy="50" r="9" fill="#7FC1D6">`,
		`stroke="#1B1A17"`,
		`>codesk</td>`,
	} {
		if !strings.Contains(message.HTML, want) {
			t.Fatalf("email HTML missing logo fragment %q:\n%s", want, message.HTML)
		}
	}
	if strings.Contains(message.Text, "<svg") {
		t.Fatalf("plain-text email should not include logo markup: %q", message.Text)
	}
	if !strings.Contains(message.Text, "https://app.getcodesk.com/account/verify-email?token=verify-token") {
		t.Fatalf("plain-text email missing verification link: %q", message.Text)
	}
}

func TestValidateEmailConfigRequiresMailgunWhenStrict(t *testing.T) {
	if err := (Config{}).ValidateEmailConfig(); err != nil {
		t.Fatalf("direct test config should allow injected noop email sender: %v", err)
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

func TestLoadConfigRequiresEmailByDefault(t *testing.T) {
	t.Setenv("NOTTY_REQUIRE_EMAIL", "")
	t.Setenv("NOTTY_PUBLIC_ORIGIN", "")
	t.Setenv("NOTTY_FRONTEND_ORIGIN", "")
	t.Setenv("NOTTY_MAILGUN_DOMAIN", "")
	t.Setenv("NOTTY_MAILGUN_API_KEY", "")
	t.Setenv("NOTTY_MAILGUN_FROM", "")

	cfg := LoadConfig()
	if !cfg.RequireEmail {
		t.Fatalf("LoadConfig RequireEmail = false, want true by default")
	}
	if cfg.PublicOrigin != "https://app.getcodesk.com" {
		t.Fatalf("LoadConfig PublicOrigin = %q, want app.getcodesk.com default", cfg.PublicOrigin)
	}
	if cfg.MailgunDomain != "mail.getcodesk.com" {
		t.Fatalf("LoadConfig MailgunDomain = %q, want mail.getcodesk.com default", cfg.MailgunDomain)
	}
	if cfg.MailgunFrom != "noreply@mail.getcodesk.com" {
		t.Fatalf("LoadConfig MailgunFrom = %q, want noreply@mail.getcodesk.com default", cfg.MailgunFrom)
	}
	err := cfg.ValidateEmailConfig()
	if err == nil {
		t.Fatalf("expected default loaded config to require Mailgun settings")
	}
	if !strings.Contains(err.Error(), "NOTTY_REQUIRE_EMAIL") {
		t.Fatalf("expected default strict config error to mention NOTTY_REQUIRE_EMAIL, got %q", err.Error())
	}

	t.Setenv("NOTTY_MAILGUN_DOMAIN", "mg.example.com")
	t.Setenv("NOTTY_MAILGUN_API_KEY", "key")
	t.Setenv("NOTTY_MAILGUN_FROM", "codesk <noreply@example.com>")
	cfg = LoadConfig()
	if err := cfg.ValidateEmailConfig(); err != nil {
		t.Fatalf("expected default strict config to accept complete Mailgun settings: %v", err)
	}
}

func TestValidateEmailConfigRejectsExplicitFalseRequireEmailFlag(t *testing.T) {
	t.Setenv("NOTTY_REQUIRE_EMAIL", "false")
	t.Setenv("NOTTY_MAILGUN_DOMAIN", "mg.example.com")
	t.Setenv("NOTTY_MAILGUN_API_KEY", "key")
	t.Setenv("NOTTY_MAILGUN_FROM", "codesk <noreply@example.com>")

	cfg := LoadConfig()
	err := cfg.ValidateEmailConfig()
	if err == nil {
		t.Fatalf("expected explicit false NOTTY_REQUIRE_EMAIL to fail validation")
	}
	if !strings.Contains(err.Error(), "NOTTY_REQUIRE_EMAIL=false") {
		t.Fatalf("expected explicit false error to mention NOTTY_REQUIRE_EMAIL=false, got %q", err.Error())
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
