package main

import (
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-imap"
)

// oneLine collapses a header value onto a single line, dropping any control
// characters that would confuse a line-oriented reader.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// formatAddresses renders an IMAP address list as "Name <a@b>, ...".
func formatAddresses(addrs []*imap.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		name := oneLine(addr.PersonalName)
		email := strings.TrimSpace(addr.MailboxName + "@" + addr.HostName)
		switch {
		case name == "" && email == "":
			continue
		case name == "":
			out = append(out, email)
		case email == "":
			out = append(out, name)
		default:
			out = append(out, fmt.Sprintf("%s <%s>", name, email))
		}
	}
	return strings.Join(out, ", ")
}

// formatFlags turns IMAP flags into readable words.
func formatFlags(flags []string) string {
	if len(flags) == 0 {
		return "none"
	}
	words := make([]string, 0, len(flags))
	for _, f := range flags {
		switch strings.ToLower(f) {
		case strings.ToLower(imap.SeenFlag):
			words = append(words, "read")
		case strings.ToLower(imap.AnsweredFlag):
			words = append(words, "answered")
		case strings.ToLower(imap.FlaggedFlag):
			words = append(words, "flagged")
		case strings.ToLower(imap.DeletedFlag):
			words = append(words, "deleted")
		case strings.ToLower(imap.DraftFlag):
			words = append(words, "draft")
		case strings.ToLower(imap.RecentFlag):
			words = append(words, "recent")
		default:
			words = append(words, f)
		}
	}
	return strings.Join(words, ", ")
}

func isUnread(flags []string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, imap.SeenFlag) {
			return false
		}
	}
	return true
}

// humanSize renders a byte count compactly.
func humanSize(n uint32) string {
	return humanBytes(int64(n))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// stamp renders a time for display; a zero time becomes a dash.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05 -0700")
}

// shortStamp renders a time without seconds, for the dense list view.
func shortStamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

// header returns a single header value, normalised to one line.
func header(parsed *parsedMessage, key string) string {
	if parsed == nil || parsed.Header == nil {
		return ""
	}
	return oneLine(parsed.Header.Get(key))
}

// headerAddresses renders an address header such as To or Cc.
func headerAddresses(parsed *parsedMessage, key string) string {
	if parsed == nil || parsed.Header == nil {
		return ""
	}
	raw := parsed.Header.Get(key)
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if list, err := mail.ParseAddressList(raw); err == nil {
		return formatMailAddresses(list)
	}
	// Unparseable header: show it raw rather than losing the information.
	return oneLine(raw)
}

// formatMailAddresses renders net/mail addresses as "Name <a@b>, ...".
func formatMailAddresses(list []*mail.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		name := oneLine(a.Name)
		switch {
		case name == "":
			parts = append(parts, a.Address)
		case a.Address == "":
			parts = append(parts, name)
		default:
			parts = append(parts, fmt.Sprintf("%s <%s>", name, a.Address))
		}
	}
	return strings.Join(parts, ", ")
}

// hasFlag reports whether flags contains want, case-insensitively.
func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// makePreview builds a one-line summary of a raw body snippet.
func makePreview(raw string) string {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	// The peeked bytes may slice through a quoted-printable soft line break.
	text = strings.ReplaceAll(text, "=\n", "")
	text = decodeQuotedPrintableText(text)

	// Most bulk mail is HTML-only, so convert before anything else; otherwise
	// raw markup leaks into the preview.
	if looksLikeHTML(text) {
		text = htmlToText(text)
	}

	// Drop quoted lines: clients that top-post put the history in the first
	// lines of the message.
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if quoteLineRe.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.Join(kept, "\n")

	if looksLikeHTML(text) {
		text = stripTags(text)
	}
	text = oneLine(collapseSpace(text))

	const max = 160
	if len(text) > max {
		// Cut on a rune boundary.
		cut := max
		for cut > 0 && !utf8Start(text[cut]) {
			cut--
		}
		text = strings.TrimSpace(text[:cut]) + "..."
	}
	return text
}

var htmlHintRe = regexp.MustCompile(`(?i)<\s*(html|head|body|div|p|br|table|tr|td|a|span|ul|li|h[1-6]|img|meta|style|script|!doctype)\b`)

// looksLikeHTML reports whether a snippet contains enough HTML markup to be
// worth converting rather than merely having stray angle brackets.
func looksLikeHTML(s string) bool {
	return len(htmlHintRe.FindAllString(s, 3)) >= 2
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

var quotePrintableRe = regexp.MustCompile(`=[0-9A-Fa-f]{2}`)

// decodeQuotedPrintableText expands "=XX" escapes. Bodies fetched via a peek
// are sometimes raw quoted-printable, and an escape may straddle the peek
// boundary, so malformed input is passed through unchanged.
func decodeQuotedPrintableText(s string) string {
	return quotePrintableRe.ReplaceAllStringFunc(s, func(m string) string {
		v, err := strconv.ParseUint(m[1:], 16, 8)
		if err != nil {
			return m
		}
		return string(rune(v))
	})
}

var (
	// Signature separator: a line consisting of exactly two dashes, optionally
	// followed by a space.
	signatureSepRe = regexp.MustCompile(`^-- ?$`)
	// Standard "reply above" and attribution banners.
	replyHeaderRe = regexp.MustCompile(`(?mi)^-{2,}\s*original message\s*-{2,}\s*$`)
	replyOnRe     = regexp.MustCompile(`(?mi)^(on .{3,80}wrote:|from:\s*.{0,80}wrote:)$`)
	// Quoted lines, possibly nested.
	quoteLineRe = regexp.MustCompile(`^\s*>`)
)

// cleanBody strips the quoted reply chain and the signature from a message so
// that agents see only the newly written text.
func cleanBody(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	cut := len(lines)

	// Everything from the signature separator onwards is boilerplate. Only
	// trust a separator in the last third of the body, since a line of dashes
	// mid-message is far more likely to be a horizontal rule or a signature
	// belonging to quoted text that is about to be cut anyway.
	sigFloor := (len(lines) * 2) / 3
	for i := sigFloor; i < len(lines); i++ {
		if signatureSepRe.MatchString(lines[i]) {
			cut = i
			break
		}
	}

	// Cut at the first quoted block. A leading ">" only starts a quote when it
	// is preceded by a blank line or followed by another ">", which avoids
	// mangling top-posted replies that quote inline.
	for i := 0; i < cut; i++ {
		if !quoteLineRe.MatchString(lines[i]) {
			continue
		}
		if i == 0 {
			// A reply that is nothing but quoted text.
			cut = 0
			break
		}
		prevBlank := strings.TrimSpace(lines[i-1]) == ""
		nextQuoted := i+1 < len(lines) && quoteLineRe.MatchString(lines[i+1])
		if prevBlank || nextQuoted {
			cut = i
			break
		}
	}

	body := strings.Join(lines[:cut], "\n")

	// Drop any remaining attribution banner.
	body = replyHeaderRe.ReplaceAllString(body, "")
	body = replyOnRe.ReplaceAllString(body, "")

	return collapseSpace(body)
}
