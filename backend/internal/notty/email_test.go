package notty

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
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
		`<img src="cid:codesk-oxo.png" width="58" height="44" alt="codesk"`,
		`>codesk</td>`,
	} {
		if !strings.Contains(message.HTML, want) {
			t.Fatalf("email HTML missing logo fragment %q:\n%s", want, message.HTML)
		}
	}
	for _, unwanted := range []string{"<svg", "<circle", "<line", "data:image", "transform:rotate", "position:absolute", "position:relative"} {
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
	if len(message.InlineAttachments) != 1 {
		t.Fatalf("inline attachments = %d, want 1", len(message.InlineAttachments))
	}
	logo := message.InlineAttachments[0]
	if logo.Filename != accountEmailLogoFilename {
		t.Fatalf("logo filename = %q, want %q", logo.Filename, accountEmailLogoFilename)
	}
	if logo.ContentType != "image/png" {
		t.Fatalf("logo content type = %q, want image/png", logo.ContentType)
	}
	if !bytes.Equal(logo.Data, accountEmailLogoPNG) {
		t.Fatalf("logo attachment data does not match embedded PNG")
	}
}

func TestBuildAccountEmailLogoPNGMatchesOriginalSVGGeometry(t *testing.T) {
	message := buildVerificationEmail("person@example.com", "https://app.getcodesk.com/account/verify-email?token=verify-token")
	if len(message.InlineAttachments) != 1 {
		t.Fatalf("inline attachments = %d, want 1", len(message.InlineAttachments))
	}
	img, err := png.Decode(bytes.NewReader(message.InlineAttachments[0].Data))
	if err != nil {
		t.Fatalf("decode logo PNG: %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 116, 88); got != want {
		t.Fatalf("logo bounds = %v, want %v", got, want)
	}

	assertPixelNear(t, img, image.Pt(16, 44), color.RGBA{R: 0xE3, G: 0xA1, B: 0x5B, A: 0xFF})
	assertPixelNear(t, img, image.Pt(100, 44), color.RGBA{R: 0x7F, G: 0xC1, B: 0xD6, A: 0xFF})
	assertPixelNear(t, img, image.Pt(58, 44), color.RGBA{R: 0x1B, G: 0x1A, B: 0x17, A: 0xFF})
	assertTransparentPixel(t, img, image.Pt(0, 0))
	assertTransparentPixel(t, img, image.Pt(58, 10))
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

func TestMailgunEmailSenderSendsInlineAttachments(t *testing.T) {
	var sawRequest bool
	sender := &mailgunEmailSender{
		domain: "mail.getcodesk.com",
		apiKey: "mailgun-key",
		from:   "codesk <noreply@mail.getcodesk.com>",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sawRequest = true
			if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "multipart/form-data; boundary=") {
				t.Fatalf("content type = %q, want multipart/form-data", got)
			}
			reader, err := req.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			fields := map[string]string{}
			var inlineFilename string
			var inlineContentType string
			var inlineData []byte
			for {
				part, err := reader.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("next multipart part: %v", err)
				}
				data, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("read multipart part: %v", err)
				}
				if part.FormName() == "inline" {
					inlineFilename = part.FileName()
					inlineContentType = part.Header.Get("Content-Type")
					inlineData = data
					continue
				}
				fields[part.FormName()] = string(data)
			}
			wantFields := map[string]string{
				"from":    "codesk <noreply@mail.getcodesk.com>",
				"to":      "person@example.com",
				"subject": "Verify your email to finish signing up",
				"text":    "plain body",
				"html":    `<img src="cid:codesk-oxo.png">`,
			}
			for key, wantValue := range wantFields {
				if got := fields[key]; got != wantValue {
					t.Fatalf("multipart field %s = %q, want %q", key, got, wantValue)
				}
			}
			if inlineFilename != accountEmailLogoFilename {
				t.Fatalf("inline filename = %q, want %q", inlineFilename, accountEmailLogoFilename)
			}
			if inlineContentType != "image/png" {
				t.Fatalf("inline content type = %q, want image/png", inlineContentType)
			}
			if !bytes.Equal(inlineData, accountEmailLogoPNG) {
				t.Fatalf("inline data does not match logo PNG")
			}
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Body:       io.NopCloser(strings.NewReader("queued")),
			}, nil
		})},
	}

	err := sender.SendEmail(context.Background(), EmailMessage{
		To:      " person@example.com ",
		Subject: " Verify your email to finish signing up ",
		Text:    "plain body",
		HTML:    `<img src="cid:codesk-oxo.png">`,
		InlineAttachments: []EmailInlineAttachment{{
			Filename:    accountEmailLogoFilename,
			ContentType: "image/png",
			Data:        accountEmailLogoPNG,
		}},
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

func assertPixelNear(t testing.TB, img image.Image, point image.Point, want color.RGBA) {
	t.Helper()
	got := color.RGBAModel.Convert(img.At(point.X, point.Y)).(color.RGBA)
	if !colorNear(got, want, 1) {
		t.Fatalf("pixel %v = %#v, want near %#v", point, got, want)
	}
}

func assertTransparentPixel(t testing.TB, img image.Image, point image.Point) {
	t.Helper()
	_, _, _, alpha := img.At(point.X, point.Y).RGBA()
	if alpha != 0 {
		t.Fatalf("pixel %v alpha = %d, want transparent", point, alpha)
	}
}

func colorNear(got color.RGBA, want color.RGBA, tolerance uint8) bool {
	return componentNear(got.R, want.R, tolerance) &&
		componentNear(got.G, want.G, tolerance) &&
		componentNear(got.B, want.B, tolerance) &&
		componentNear(got.A, want.A, tolerance)
}

func componentNear(got uint8, want uint8, tolerance uint8) bool {
	if got > want {
		return got-want <= tolerance
	}
	return want-got <= tolerance
}
