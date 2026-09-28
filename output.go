package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-imap"
)

const labelWidth = 13

// kv writes an aligned "LABEL: value" line.
func kv(w io.Writer, label, value string) {
	pad := strings.Repeat(" ", maxInt(1, labelWidth-len(label)-2))
	fmt.Fprintf(w, "%s:%s%s\n", label, pad, value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func plural(n uint32) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// folderSummary is the headline "where am I looking" information.
type folderSummary struct {
	Total  uint32
	Unread uint32
}

func (f folderSummary) String() string {
	s := fmt.Sprintf("%d message%s", f.Total, plural(f.Total))
	if f.Unread > 0 {
		s += fmt.Sprintf(", %d unread", f.Unread)
	}
	return s
}

// folder is one entry in the folder overview.
type folder struct {
	Name   string
	Total  uint32
	Unread uint32
	Notes  []string
}

// printHeader writes the account/folder context block.
func printHeader(w io.Writer, account, mailbox string, status *folderSummary) {
	kv(w, "ACCOUNT", account)
	kv(w, "FOLDER", mailbox)
	if status != nil {
		kv(w, "STATUS", status.String())
	}
}

// printFolders renders the folder overview.
func printFolders(w io.Writer, account string, folders []folder) {
	kv(w, "ACCOUNT", account)
	if len(folders) == 0 {
		fmt.Fprintln(w, "\nNo folders found on this server.")
		return
	}

	width := 0
	for _, f := range folders {
		width = maxInt(width, len(f.Name))
	}
	width = minInt(width, 40)

	fmt.Fprintf(w, "\n%-*s  %7s  %7s  %s\n", width, "FOLDER", "TOTAL", "UNREAD", "NOTES")
	for _, f := range folders {
		notes := make([]string, 0, len(f.Notes))
		for _, n := range f.Notes {
			// "Archive" on a folder called Archive says nothing.
			if !strings.EqualFold(n, f.Name) {
				notes = append(notes, n)
			}
		}
		if f.Unread > 0 {
			notes = append([]string{"has unread mail"}, notes...)
		}
		fmt.Fprintf(w, "%-*s  %7d  %7d  %s\n", width, f.Name, f.Total, f.Unread, orDash(strings.Join(notes, ", ")))
	}
}

// printMessages renders the list view: one compact block per message.
func printMessages(w io.Writer, sums []summary, shown, total int) {
	if len(sums) == 0 {
		return
	}

	header := fmt.Sprintf("MESSAGES (showing %d of %d, newest first)", shown, total)
	if shown < total {
		header += fmt.Sprintf(" -- use -n %d for more", total)
	}
	fmt.Fprintf(w, "\n%s\n%s\n", header, strings.Repeat("-", len(header)))

	for i, m := range sums {
		tags := make([]string, 0, 3)
		if isUnread(m.Flags) {
			tags = append(tags, "UNREAD")
		}
		if m.HasAttach {
			tags = append(tags, "ATTACHMENT")
		}
		if hasFlag(m.Flags, imap.FlaggedFlag) {
			tags = append(tags, "FLAGGED")
		}

		fmt.Fprintf(w, "\n%d. %s", i+1, m.Ref)
		if len(tags) > 0 {
			fmt.Fprintf(w, "  [%s]", strings.Join(tags, ", "))
		}
		fmt.Fprintln(w)
		kv(w, "   Date", shortStamp(m.Date))
		kv(w, "   From", orDash(m.From))
		kv(w, "   Subject", orDash(m.Subject))
		kv(w, "   Size", humanSize(m.Size))
		if m.Preview != "" {
			kv(w, "   Preview", m.Preview)
		}
	}
}

// printMessage renders a single message in full.
func printMessage(w io.Writer, account string, m *message, parsed *parsedMessage, trimmed bool) {
	kv(w, "ID", m.Ref.String())
	kv(w, "FOLDER", m.Ref.Mailbox)
	kv(w, "DATE", stamp(m.Date))
	kv(w, "FROM", orDash(m.From))
	kv(w, "TO", orDash(headerAddresses(parsed, "To")))
	kv(w, "CC", orDash(headerAddresses(parsed, "Cc")))
	kv(w, "REPLY-TO", orDash(headerAddresses(parsed, "Reply-To")))
	kv(w, "SUBJECT", orDash(m.Subject))
	kv(w, "MESSAGE-ID", orDash(header(parsed, "Message-Id")))
	kv(w, "IN-REPLY-TO", orDash(header(parsed, "In-Reply-To")))
	kv(w, "FLAGS", formatFlags(m.Flags))
	kv(w, "SIZE", humanSize(m.Size))

	if len(parsed.Attachments) > 0 {
		fmt.Fprintln(w, "\nATTACHMENTS (content not decoded, use a mail client to save):")
		for _, a := range parsed.Attachments {
			fmt.Fprintf(w, "  - %s  [%s, %s, part %s]\n",
				oneLine(a.Filename), orDash(a.MIMEType), humanBytes(a.Size), a.Part)
		}
	}

	body := parsed.Text
	if trimmed {
		body = cleanBody(body)
	}

	fmt.Fprintln(w, "\n--- BEGIN BODY ---")
	if strings.TrimSpace(body) == "" {
		switch {
		case trimmed && strings.TrimSpace(parsed.Text) != "":
			fmt.Fprintln(w, "(the body consists only of quoted replies and a signature)")
		case len(parsed.Attachments) > 0:
			fmt.Fprintln(w, "(this message has no text body; see ATTACHMENTS above)")
		default:
			fmt.Fprintln(w, "(this message has no text body)")
		}
	} else {
		fmt.Fprintln(w, body)
	}
	fmt.Fprintln(w, "--- END BODY ---")

	fmt.Fprintf(w, "\nThis message has been marked as read.\n")
	fmt.Fprintf(w, "To file it away: checkemail archive %q %q\n", account, m.Ref.String())
}

// printArchived confirms a successful move.
func printArchived(w io.Writer, account string, ref msgRef, dest string) {
	kv(w, "ID", ref.String())
	kv(w, "MOVED-TO", dest)
	kv(w, "STATUS", "archived")
	fmt.Fprintf(w, "\nMessage %s was moved to %q on account %q.\n", ref, dest, account)
	fmt.Fprintf(w, "Note: the uid is only meaningful within a folder, so verify with:\n")
	fmt.Fprintf(w, "  checkemail list %q -folder %q -n 5\n", account, dest)
}
