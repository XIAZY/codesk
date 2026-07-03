package notty

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
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
		`position:relative;width:58px;height:44px`,
		`border-radius:50%;background:#E3A15B`,
		`border-radius:50%;background:#7FC1D6`,
		`background:#1B1A17`,
		`transform:rotate(45deg)`,
		`transform:rotate(-45deg)`,
		`>codesk</td>`,
	} {
		if !strings.Contains(message.HTML, want) {
			t.Fatalf("email HTML missing logo fragment %q:\n%s", want, message.HTML)
		}
	}
	for _, unwanted := range []string{"<svg", "<circle", "<line"} {
		if strings.Contains(message.HTML, unwanted) {
			t.Fatalf("email HTML should render the logo without SVG fragment %q:\n%s", unwanted, message.HTML)
		}
		if strings.Contains(message.Text, unwanted) {
			t.Fatalf("plain-text email should not include logo markup %q: %q", unwanted, message.Text)
		}
	}
	if !strings.Contains(message.Text, "https://app.getcodesk.com/account/verify-email?token=verify-token") {
		t.Fatalf("plain-text email missing verification link: %q", message.Text)
	}
}

func TestBuildWelcomeEmailUsesAppLinkWithoutExpiry(t *testing.T) {
	message := buildWelcomeEmail("person@example.com", "https://app.getcodesk.com")

	if message.Subject != "Welcome to codesk" {
		t.Fatalf("welcome email subject = %q", message.Subject)
	}
	for _, want := range []string{
		"Welcome to codesk",
		"Your email is verified.",
		"https://app.getcodesk.com",
	} {
		if !strings.Contains(message.Text, want) {
			t.Fatalf("welcome text missing %q: %q", want, message.Text)
		}
		if !strings.Contains(message.HTML, want) {
			t.Fatalf("welcome HTML missing %q:\n%s", want, message.HTML)
		}
	}
	for _, unwanted := range []string{"token=", "This link expires in 1 hour."} {
		if strings.Contains(message.Text, unwanted) || strings.Contains(message.HTML, unwanted) {
			t.Fatalf("welcome email should not contain %q:\ntext=%q\nhtml=%s", unwanted, message.Text, message.HTML)
		}
	}
}

func TestMailgunEmailSenderSendsExpectedMessageRequest(t *testing.T) {
	var sawRequest bool
	sender := &mailgunEmailSender{
		domain: "mail.getcodesk.com",
		apiKey: "mailgun-key",
		from:   "codesk <noreply@mail.getcodesk.com>",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sawRequest = true
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			if req.URL.Scheme != "https" || req.URL.Host != "api.mailgun.net" || req.URL.Path != "/v3/mail.getcodesk.com/messages" {
				t.Fatalf("unexpected Mailgun URL: %s", req.URL.String())
			}
			user, pass, ok := req.BasicAuth()
			if !ok || user != "api" || pass != "mailgun-key" {
				t.Fatalf("basic auth = user %q pass %q ok %t", user, pass, ok)
			}
			if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Fatalf("content type = %q", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			values, err := url.ParseQuery(string(body))
			if err != nil {
				t.Fatalf("parse body: %v", err)
			}
			want := map[string]string{
				"from":    "codesk <noreply@mail.getcodesk.com>",
				"to":      "person@example.com",
				"subject": "Welcome to codesk",
				"text":    "plain body",
				"html":    "<p>html body</p>",
			}
			for key, wantValue := range want {
				if got := values.Get(key); got != wantValue {
					t.Fatalf("form field %s = %q, want %q", key, got, wantValue)
				}
			}
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Body:       io.NopCloser(strings.NewReader("queued")),
			}, nil
		})},
	}

	err := sender.SendEmail(context.Background(), EmailMessage{
		To:      " person@example.com ",
		Subject: " Welcome to codesk ",
		Text:    "plain body",
		HTML:    "<p>html body</p>",
	})
	if err != nil {
		t.Fatalf("send email: %v", err)
	}
	if !sawRequest {
		t.Fatalf("Mailgun request was not sent")
	}
}

func TestMailgunEmailSenderReturnsSendFailures(t *testing.T) {
	t.Run("non_2xx", func(t *testing.T) {
		sender := &mailgunEmailSender{
			domain: "mail.getcodesk.com",
			apiKey: "mailgun-key",
			from:   "noreply@mail.getcodesk.com",
			httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader("rate limited")),
				}, nil
			})},
		}
		err := sender.SendEmail(context.Background(), EmailMessage{To: "person@example.com"})
		if err == nil || !strings.Contains(err.Error(), "status=429") || !strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("expected non-2xx error with response body, got %v", err)
		}
	})

	t.Run("transport_error", func(t *testing.T) {
		sender := &mailgunEmailSender{
			domain: "mail.getcodesk.com",
			apiKey: "mailgun-key",
			from:   "noreply@mail.getcodesk.com",
			httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			})},
		}
		err := sender.SendEmail(context.Background(), EmailMessage{To: "person@example.com"})
		if err == nil || !strings.Contains(err.Error(), "dial failed") {
			t.Fatalf("expected transport error, got %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
