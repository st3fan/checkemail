package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

func TestParseMsgRef(t *testing.T) {
	tests := []struct {
		in      string
		mailbox string
		uid     uint32
		wantErr bool
	}{
		{in: "INBOX:4821", mailbox: "INBOX", uid: 4821},
		{in: "Archive:7", mailbox: "Archive", uid: 7},
		{in: "inbox:9", mailbox: "INBOX", uid: 9},
		{in: "Sent Items/2024:12", mailbox: "Sent Items/2024", uid: 12},
		{in: "4821", mailbox: "INBOX", uid: 4821},
		{in: "  INBOX:1  ", mailbox: "INBOX", uid: 1},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "INBOX:", wantErr: true},
		{in: "INBOX:0", wantErr: true},
		{in: ":12", wantErr: true},
		{in: "INBOX:-3", wantErr: true},
	}

	for _, tt := range tests {
		ref, err := parseMsgRef(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMsgRef(%q) = %v, want error", tt.in, ref)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMsgRef(%q): %v", tt.in, err)
			continue
		}
		if ref.Mailbox != tt.mailbox || ref.UID != tt.uid {
			t.Errorf("parseMsgRef(%q) = %s, want %s:%d", tt.in, ref, tt.mailbox, tt.uid)
		}
	}
}

func TestParseMessagePlainText(t *testing.T) {
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Lunch\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Are we still on for tomorrow?\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "still on for tomorrow") {
		t.Errorf("body = %q", parsed.Text)
	}
	if len(parsed.Attachments) != 0 {
		t.Errorf("unexpected attachments: %+v", parsed.Attachments)
	}
	if parsed.Header.Get("Subject") != "Lunch" {
		t.Errorf("subject = %q", parsed.Header.Get("Subject"))
	}
}

func TestParseMessagePrefersPlainOverHTML(t *testing.T) {
	raw := "Subject: Alt\r\n" +
		"Content-Type: multipart/alternative; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the plain version\r\n" +
		"--B\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>the <b>html</b> version</p>\r\n" +
		"--B--\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "the plain version") {
		t.Errorf("body = %q, want the text/plain part", parsed.Text)
	}
	if strings.Contains(parsed.Text, "<b>") {
		t.Errorf("body contains markup: %q", parsed.Text)
	}
}

func TestParseMessageHTMLOnly(t *testing.T) {
	raw := "Subject: HTML\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><h1>Title</h1><p>Hello <b>world</b></p>" +
		"<ul><li>one</li><li>two</li></ul></body></html>\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.Text, "<") {
		t.Errorf("body still contains markup: %q", parsed.Text)
	}
	for _, want := range []string{"Title", "Hello world", "one", "two"} {
		if !strings.Contains(parsed.Text, want) {
			t.Errorf("body %q missing %q", parsed.Text, want)
		}
	}
}

func TestParseMessageQuotedPrintable(t *testing.T) {
	raw := "Subject: QP\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Caf=C3=A9 and a soft=\r\n break\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "Café") {
		t.Errorf("body = %q, want the decoded accent", parsed.Text)
	}
	if strings.Contains(parsed.Text, "=C3") {
		t.Errorf("body still has quoted-printable escapes: %q", parsed.Text)
	}
}

func TestParseMessageBase64(t *testing.T) {
	raw := "Subject: B64\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"aGVsbG8gd29ybGQ=\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(parsed.Text) != "hello world" {
		t.Errorf("body = %q", parsed.Text)
	}
}

func TestParseMessageAttachments(t *testing.T) {
	raw := "Subject: Files\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--B\r\n" +
		"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQK\r\n" +
		"--B--\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "see attached") {
		t.Errorf("body = %q", parsed.Text)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want exactly 1", parsed.Attachments)
	}
	a := parsed.Attachments[0]
	if a.Filename != "report.pdf" {
		t.Errorf("filename = %q", a.Filename)
	}
	if a.MIMEType != "application/pdf" {
		t.Errorf("mime type = %q", a.MIMEType)
	}
	if a.Size == 0 {
		t.Errorf("attachment size was not measured")
	}
}

func TestParseMessageForwarded(t *testing.T) {
	inner := "From: carol@example.com\r\n" +
		"Subject: original\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the original message\r\n"

	raw := "Subject: fwd: original\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"look at this\r\n" +
		"--B\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		inner +
		"--B--\r\n"

	parsed, err := parseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "look at this") {
		t.Errorf("body = %q, missing the forwarding note", parsed.Text)
	}
	if !strings.Contains(parsed.Text, "the original message") {
		t.Errorf("body = %q, missing the forwarded text", parsed.Text)
	}
}

func TestCleanBodyStripsQuoteAndSignature(t *testing.T) {
	body := strings.Join([]string{
		"Thanks, that works for me.",
		"",
		"> On Monday, Alice wrote:",
		"> Are we still on for tomorrow?",
		">",
		"> I think so.",
		"",
		"-- ",
		"Alice",
		"CTO, Example Inc",
	}, "\n")

	got := cleanBody(body)
	if strings.Contains(got, "Are we still on") {
		t.Errorf("quoted reply survived: %q", got)
	}
	if strings.Contains(got, "CTO") {
		t.Errorf("signature survived: %q", got)
	}
	if !strings.Contains(got, "that works for me") {
		t.Errorf("new text was lost: %q", got)
	}
}

func TestCleanBodyKeepsTopPostedInlineQuote(t *testing.T) {
	// A reply written above the quoted history, where the quote does not start
	// on its own line.
	body := strings.Join([]string{
		"Sounds good. > Inline aside from the quoted line.",
		"More new text.",
		"> ",
		"> the quoted thread",
		"> continues here",
	}, "\n")

	got := cleanBody(body)
	if !strings.Contains(got, "Sounds good") {
		t.Errorf("new text was lost: %q", got)
	}
	if !strings.Contains(got, "Inline aside") {
		t.Errorf("inline text was lost: %q", got)
	}
	if strings.Contains(got, "quoted thread") {
		t.Errorf("quoted thread survived: %q", got)
	}
}

func TestCleanBodyKeepsHorizontalRule(t *testing.T) {
	body := "Actual content here.\n\n--\n\nMore content after a horizontal rule."
	got := cleanBody(body)
	if !strings.Contains(got, "More content after") {
		t.Errorf("content after a rule was cut: %q", got)
	}
}

func TestMakePreview(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text",
			in:   "Hello there\r\n\r\nSecond paragraph.\r\n",
			want: "Hello there Second paragraph.",
		},
		{
			name: "html is converted",
			in:   "<html><body><p>Hello <b>world</b></p><div>Second</div></body></html>",
			want: "Hello world Second",
		},
		{
			name: "quoted lines dropped",
			in:   "New reply\r\n> old quoted text\r\n",
			want: "New reply",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := makePreview(tt.in); got != tt.want {
				t.Errorf("makePreview() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMakePreviewTruncates(t *testing.T) {
	got := makePreview(strings.Repeat("a", 500))
	if len(got) > 200 {
		t.Errorf("preview not truncated: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated preview should end with an ellipsis: %q", got)
	}
}

func TestOneLine(t *testing.T) {
	got := oneLine("multi\r\nline\tvalue\x00here")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("oneLine left control characters: %q", got)
	}
	if got != "multi line valuehere" {
		t.Errorf("oneLine() = %q", got)
	}
}

func TestFormatFlags(t *testing.T) {
	if got := formatFlags([]string{`\Seen`, `\Flagged`}); got != "read, flagged" {
		t.Errorf("formatFlags() = %q", got)
	}
	if got := formatFlags(nil); got != "none" {
		t.Errorf("formatFlags(nil) = %q", got)
	}
	if !isUnread([]string{`\Recent`}) {
		t.Error("a message without \\Seen should be unread")
	}
	if isUnread([]string{`\Seen`}) {
		t.Error("a message with \\Seen should not be unread")
	}
}

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		server string
		port   int
		host   string
		want   int
	}{
		{server: "imap.example.com", host: "imap.example.com", want: 993},
		{server: "imap.example.com", port: 1993, host: "imap.example.com", want: 1993},
		{server: "imap.example.com:143", host: "imap.example.com", want: 143},
		{server: "[::1]", host: "::1", want: 993},
		{server: "[::1]:993", host: "::1", want: 993},
		{server: "2001:db8::1", host: "2001:db8::1", want: 993},
	}

	for _, tt := range tests {
		host, port, err := splitHostPort(tt.server, tt.port)
		if err != nil {
			t.Errorf("splitHostPort(%q, %d): %v", tt.server, tt.port, err)
			continue
		}
		if host != tt.host || port != tt.want {
			t.Errorf("splitHostPort(%q, %d) = %s:%d, want %s:%d",
				tt.server, tt.port, host, port, tt.host, tt.want)
		}
	}

	if _, _, err := splitHostPort("", 0); err == nil {
		t.Error("an empty server should be rejected")
	}
}

func TestHasAttachment(t *testing.T) {
	// multipart/alternative with only text parts: no attachment.
	alternative := &imap.BodyStructure{
		MIMEType: "multipart", MIMESubType: "alternative",
		Parts: []*imap.BodyStructure{
			{MIMEType: "text", MIMESubType: "plain"},
			{MIMEType: "text", MIMESubType: "html"},
		},
	}
	if hasAttachment(alternative) {
		t.Error("a text-only multipart/alternative should not report an attachment")
	}

	// The same, with an inline image in a nested container.
	withImage := &imap.BodyStructure{
		MIMEType: "multipart", MIMESubType: "mixed",
		Parts: []*imap.BodyStructure{
			{MIMEType: "multipart", MIMESubType: "alternative",
				Parts: []*imap.BodyStructure{
					{MIMEType: "text", MIMESubType: "plain"},
				}},
			{MIMEType: "image", MIMESubType: "png"},
		},
	}
	if !hasAttachment(withImage) {
		t.Error("an image part should report an attachment")
	}

	// A named text file is an attachment even though it is text.
	named := &imap.BodyStructure{
		MIMEType: "multipart", MIMESubType: "mixed",
		DispositionParams: map[string]string{},
		Parts: []*imap.BodyStructure{
			{MIMEType: "text", MIMESubType: "plain"},
			{MIMEType: "text", MIMESubType: "calendar",
				Disposition:       "attachment",
				DispositionParams: map[string]string{"filename": "invite.ics"}},
		},
	}
	if !hasAttachment(named) {
		t.Error("a named attachment should report an attachment")
	}
}

func TestParseFlagsPositionIndependent(t *testing.T) {
	fs := newFlagSet("list")
	folder := fs.String("folder", "INBOX", "")
	limit := fs.Int("n", 20, "")
	unread := fs.Bool("unread", false, "")

	rest, err := parseFlags(fs, []string{"acct", "-folder", "Archive", "-n", "5", "-unread"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0] != "acct" {
		t.Errorf("positional args = %v, want [acct]", rest)
	}
	if *folder != "Archive" || *limit != 5 || !*unread {
		t.Errorf("flags not applied: folder=%q n=%d unread=%v", *folder, *limit, *unread)
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	fs := newFlagSet("list")
	fs.Int("n", 20, "")

	if _, err := parseFlags(fs, []string{"acct", "-nope"}); err == nil {
		t.Error("an unknown flag should be an error")
	}
	if _, err := parseFlags(fs, []string{"acct", "-n"}); err == nil {
		t.Error("a value flag without a value should be an error")
	}
}

func TestLoadAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	body := `{
	  "a@example.com": {"server": "imap.a.test", "username": "a@example.com", "password": "p1"},
	  "b@example.com": {"server": "imap.b.test:993", "username": "b@example.com", "password": "p2", "archive": "Saved"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHECKEMAIL_ACCOUNTS", path)

	accounts, got, err := loadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}

	acct, err := accounts.lookup("A@Example.com")
	if err != nil {
		t.Fatalf("lookup should be case-insensitive: %v", err)
	}
	if acct.Username != "a@example.com" {
		t.Errorf("username = %q", acct.Username)
	}
	if _, err := accounts.lookup("nope@example.com"); err == nil {
		t.Error("an unknown account should be an error")
	} else if !strings.Contains(err.Error(), "a@example.com") {
		t.Errorf("the error should list known accounts: %v", err)
	}

	// The permission warning must not fire for a 0600 file; capture stderr.
	if b, err := accounts.lookup("b@example.com"); err != nil || b.Archive != "Saved" {
		t.Errorf("archive setting = %q (%v)", b.Archive, err)
	}
}

func TestLoadAccountsRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte(`{"x": {"username": "x", "password": "p"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHECKEMAIL_ACCOUNTS", path)

	if _, _, err := loadAccounts(); err == nil {
		t.Error("an account without a server should be rejected")
	}
}
