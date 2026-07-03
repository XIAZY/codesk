package notty

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

type EmailMessage struct {
	To                string
	Subject           string
	Text              string
	HTML              string
	InlineAttachments []EmailInlineAttachment
}

type EmailInlineAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

type EmailSender interface {
	SendEmail(ctx context.Context, message EmailMessage) error
}

type noopEmailSender struct{}

func (noopEmailSender) SendEmail(context.Context, EmailMessage) error {
	return nil
}

type mailgunEmailSender struct {
	domain     string
	apiKey     string
	from       string
	httpClient *http.Client
}

const accountEmailLogoFilename = "codesk-oxo.png"

//go:embed codesk-oxo.png
var accountEmailLogoPNG []byte

const accountEmailLogoHTML = `<table role="presentation" cellpadding="0" cellspacing="0" style="border-collapse:collapse;">
  <tr>
    <td style="width:58px;vertical-align:middle;">
      <img src="cid:codesk-oxo.png" width="58" height="44" alt="codesk" style="display:block;border:0;outline:none;text-decoration:none;">
    </td>
    <td style="padding-left:14px;vertical-align:middle;font-family:Georgia,serif;font-size:34px;line-height:1;color:#1B1A17;">codesk</td>
  </tr>
</table>`

func accountEmailLogoInlineAttachments() []EmailInlineAttachment {
	return []EmailInlineAttachment{{
		Filename:    accountEmailLogoFilename,
		ContentType: "image/png",
		Data:        accountEmailLogoPNG,
	}}
}

func emailSenderFromConfig(cfg Config) EmailSender {
	if !cfg.MailgunConfigured() {
		return noopEmailSender{}
	}
	domain := strings.TrimSpace(cfg.MailgunDomain)
	apiKey := strings.TrimSpace(cfg.MailgunAPIKey)
	from := strings.TrimSpace(cfg.MailgunFrom)
	return &mailgunEmailSender{
		domain: domain,
		apiKey: apiKey,
		from:   from,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *mailgunEmailSender) SendEmail(ctx context.Context, message EmailMessage) error {
	if s == nil {
		return fmt.Errorf("email sender is not configured")
	}
	endpoint := "https://api.mailgun.net/v3/" + url.PathEscape(s.domain) + "/messages"

	req, err := s.newMessageRequest(ctx, endpoint, message)
	if err != nil {
		return err
	}
	req.SetBasicAuth("api", s.apiKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("mailgun send failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func (s *mailgunEmailSender) newMessageRequest(ctx context.Context, endpoint string, message EmailMessage) (*http.Request, error) {
	fields := mailgunMessageFields(s.from, message)
	if len(message.InlineAttachments) == 0 {
		form := url.Values{}
		for key, value := range fields {
			form.Set(key, value)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	for _, attachment := range message.InlineAttachments {
		filename := strings.TrimSpace(attachment.Filename)
		if filename == "" {
			return nil, fmt.Errorf("inline attachment filename is required")
		}
		if len(attachment.Data) == 0 {
			return nil, fmt.Errorf("inline attachment %q is empty", filename)
		}
		contentType := strings.TrimSpace(attachment.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		part, err := writer.CreatePart(multipartFileHeader("inline", filename, contentType))
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(attachment.Data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, nil
}

func mailgunMessageFields(from string, message EmailMessage) map[string]string {
	return map[string]string{
		"from":    from,
		"to":      strings.TrimSpace(message.To),
		"subject": strings.TrimSpace(message.Subject),
		"text":    message.Text,
		"html":    message.HTML,
	}
}

func multipartFileHeader(fieldName string, filename string, contentType string) textproto.MIMEHeader {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeMultipartParam(fieldName), escapeMultipartParam(filename)))
	header.Set("Content-Type", contentType)
	return header
}

func escapeMultipartParam(value string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(value)
}

func buildVerificationEmail(to string, link string) EmailMessage {
	return buildAccountEmail(
		to,
		"Verify your email to finish signing up",
		"Verify your email",
		"Confirm it's you",
		"Click below to verify your email and activate your codesk account.",
		"Verify email",
		link,
		"This link expires in 1 hour.",
		"Didn't sign up? You can ignore this email.",
	)
}

func buildWelcomeEmail(to string, link string) EmailMessage {
	return buildAccountEmail(
		to,
		"Welcome to codesk",
		"Welcome to codesk",
		"Welcome to codesk",
		"Your email is verified. Open codesk to create or join a workspace and start working with your agents.",
		"Open codesk",
		link,
		"",
		"If you didn't sign up, you can ignore this email.",
	)
}

func buildPasswordResetEmail(to string, link string) EmailMessage {
	return buildAccountEmail(
		to,
		"Reset your codesk password",
		"Reset your password",
		"Reset your password",
		"We got a request to reset the password for your codesk account. Click below to choose a new one.",
		"Reset password",
		link,
		"This link expires in 1 hour.",
		"Didn't request this? You can ignore this email.",
	)
}

func buildAccountEmail(to string, subject string, eyebrow string, heading string, body string, button string, link string, linkNote string, footer string) EmailMessage {
	escapedLink := html.EscapeString(link)
	escapedSubject := html.EscapeString(subject)
	escapedEyebrow := html.EscapeString(eyebrow)
	escapedHeading := html.EscapeString(heading)
	escapedBody := html.EscapeString(body)
	escapedButton := html.EscapeString(button)
	escapedLinkNote := html.EscapeString(strings.TrimSpace(linkNote))
	escapedFooter := html.EscapeString(footer)
	linkNoteHTML := ""
	if escapedLinkNote != "" {
		linkNoteHTML = fmt.Sprintf(`<div style="margin-top:16px;font-size:13px;color:#A6A29A;">%s</div>`, escapedLinkNote)
	}
	htmlBody := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
</head>
<body style="margin:0;background:#F3F1EA;color:#1B1A17;font-family:Arial,sans-serif;">
  <div style="padding:40px 24px;">
    <div style="max-width:600px;margin:0 auto;background:#FCFBF7;border:1px solid #E6E2D8;border-radius:16px;overflow:hidden;">
      <div style="padding:40px 44px;">
        %s
        <div style="margin-top:36px;font-family:monospace;font-size:11px;letter-spacing:.14em;text-transform:uppercase;color:#A6A29A;">%s</div>
        <h1 style="font-family:Georgia,serif;font-weight:400;font-size:28px;line-height:1.2;margin:12px 0 0;color:#1B1A17;">%s</h1>
        <p style="font-size:15px;line-height:1.6;color:#6F6B62;margin:14px 0 0;max-width:44ch;">%s</p>
        <div style="margin-top:26px;">
          <a href="%s" style="display:inline-block;background:#1B1A17;color:#FCFBF7;font-weight:600;font-size:15px;padding:13px 26px;border-radius:24px;text-decoration:none;">%s</a>
        </div>
        %s
        <div style="margin-top:26px;padding-top:20px;border-top:1px solid #E6E2D8;">
          <div style="font-size:12.5px;color:#A6A29A;margin-bottom:8px;">Or paste this link into your browser:</div>
          <div style="font-family:monospace;font-size:12px;color:#6F6B62;background:#F3F1EA;border:1px solid #E6E2D8;border-radius:8px;padding:10px 12px;word-break:break-all;">%s</div>
        </div>
        <div style="margin-top:34px;padding-top:22px;border-top:1px solid #E6E2D8;font-size:12px;color:#A6A29A;">%s</div>
      </div>
    </div>
  </div>
</body>
</html>`, escapedSubject, accountEmailLogoHTML, escapedEyebrow, escapedHeading, escapedBody, escapedLink, escapedButton, linkNoteHTML, escapedLink, escapedFooter)
	textParts := []string{heading, body, link}
	if strings.TrimSpace(linkNote) != "" {
		textParts = append(textParts, strings.TrimSpace(linkNote))
	}
	textBody := strings.Join(textParts, "\n\n")
	return EmailMessage{To: to, Subject: subject, Text: textBody, HTML: htmlBody, InlineAttachments: accountEmailLogoInlineAttachments()}
}
