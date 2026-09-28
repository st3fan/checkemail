package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const defaultIMAPPort = 993

// maxMessageBytes caps how much of a message body is read into memory. Real
// mail is far smaller than this; the cap only protects against pathological
// input.
const maxMessageBytes = 32 << 20

// Conn is a logged-in IMAP session. It must be closed by the caller.
type Conn struct {
	cli     *client.Client
	account string
	acct    Account
}

// dialAccount opens an implicit-TLS (IMAPS) connection and authenticates.
//
// The protocol is IMAP/TLS only: there is no plaintext or STARTTLS fallback,
// and the port always defaults to 993.
func dialAccount(account string, acct Account) (*Conn, error) {
	host, port, err := splitHostPort(acct.Server, acct.Port)
	if err != nil {
		return nil, fmt.Errorf("account %q: %w", account, err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	tlsConf := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}

	cli, err := client.DialWithDialerTLS(dialer, addr, tlsConf)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %w", addr, err)
	}

	c := &Conn{cli: cli, account: account, acct: acct}
	if err := cli.Login(acct.Username, acct.Password); err != nil {
		cli.Logout()
		return nil, fmt.Errorf("login as %q on %s failed: %w", acct.Username, addr, err)
	}
	return c, nil
}

func splitHostPort(server string, port int) (string, int, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", 0, errors.New("empty \"server\"")
	}

	if strings.HasPrefix(server, "[") {
		// Bracketed IPv6 literal, with or without a port.
		if host, strPort, err := net.SplitHostPort(server); err == nil {
			p, err := strconv.Atoi(strPort)
			if err != nil {
				return "", 0, fmt.Errorf("invalid port in server %q", server)
			}
			return host, p, nil
		}
		return strings.Trim(server, "[]"), defaultIMAPPort, nil
	}

	if strings.Count(server, ":") > 1 {
		// Bare IPv6 literal, no port.
		return server, defaultIMAPPort, nil
	}

	if host, strPort, err := net.SplitHostPort(server); err == nil {
		p, err := strconv.Atoi(strPort)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port in server %q", server)
		}
		return host, p, nil
	}

	if port == 0 {
		port = defaultIMAPPort
	}
	return server, port, nil
}

func (c *Conn) Close() error {
	if c == nil || c.cli == nil {
		return nil
	}
	err := c.cli.Logout()
	c.cli = nil
	return err
}

// selectBox selects a mailbox and returns its status.
func (c *Conn) selectBox(name string) (*imap.MailboxStatus, error) {
	status, err := c.cli.Select(name, false)
	if err != nil {
		return nil, fmt.Errorf("cannot open folder %q: %w", name, err)
	}
	return status, nil
}

// status reports the total and unread counts of a folder without selecting it.
// SELECT does not reveal the unread count (only the first unseen sequence
// number), so STATUS is needed for an accurate figure.
func (c *Conn) status(name string) (*imap.MailboxStatus, error) {
	status, err := c.cli.Status(name, []imap.StatusItem{imap.StatusMessages, imap.StatusUnseen})
	if err != nil {
		return nil, fmt.Errorf("cannot get status of folder %q: %w", name, err)
	}
	return status, nil
}

// mailboxes lists every selectable folder on the server.
func (c *Conn) mailboxes() ([]*imap.MailboxInfo, error) {
	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.List("", "*", ch)
	}()

	var boxes []*imap.MailboxInfo
	for box := range ch {
		if isSelectable(box.Attributes) {
			boxes = append(boxes, box)
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("cannot list folders: %w", err)
	}
	return boxes, nil
}

func isSelectable(attrs []string) bool {
	for _, attr := range attrs {
		if attr == imap.NoSelectAttr {
			return false
		}
	}
	return true
}

// hasSpecialUse reports whether attrs contains the given special-use flag.
func hasSpecialUse(attrs []string, want string) bool {
	for _, attr := range attrs {
		if attr == want {
			return true
		}
	}
	return false
}

// resolveArchive picks the destination folder for archived mail.
//
// Precedence: the account's "archive" setting, then a folder flagged with the
// \Archive special-use attribute, then an existing folder named "Archive" (or
// "Archives") in any case. If none exists, "Archive" is created.
func (c *Conn) resolveArchive() (string, error) {
	boxes, err := c.mailboxes()
	if err != nil {
		return "", err
	}

	if c.acct.Archive != "" {
		for _, box := range boxes {
			if box.Name == c.acct.Archive {
				return box.Name, nil
			}
		}
		return "", fmt.Errorf("folder %q configured as \"archive\" does not exist", c.acct.Archive)
	}

	for _, box := range boxes {
		if hasSpecialUse(box.Attributes, imap.ArchiveAttr) {
			return box.Name, nil
		}
	}
	for _, box := range boxes {
		if strings.EqualFold(box.Name, "Archive") || strings.EqualFold(box.Name, "Archives") {
			return box.Name, nil
		}
	}

	if err := c.cli.Create("Archive"); err != nil {
		return "", fmt.Errorf("no archive folder found and it could not be created: %w", err)
	}
	return "Archive", nil
}

// msgRef identifies a single message for the read and archive commands.
//
// The canonical form is "<folder>:<uid>", for example "INBOX:4821". A bare
// number is accepted as shorthand for a message in INBOX.
type msgRef struct {
	Mailbox string
	UID     uint32
}

func parseMsgRef(s string) (msgRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return msgRef{}, errors.New("empty message id")
	}

	mailbox := imap.InboxName
	uidPart := s
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		mailbox = strings.TrimSpace(s[:idx])
		uidPart = strings.TrimSpace(s[idx+1:])
		if mailbox == "" {
			return msgRef{}, fmt.Errorf("invalid message id %q: missing folder name", s)
		}
	}

	uid, err := strconv.ParseUint(uidPart, 10, 32)
	if err != nil || uid == 0 {
		return msgRef{}, fmt.Errorf("invalid message id %q: expected \"<folder>:<uid>\" or a plain uid", s)
	}
	return msgRef{Mailbox: imap.CanonicalMailboxName(mailbox), UID: uint32(uid)}, nil
}

func (r msgRef) String() string {
	return r.Mailbox + ":" + strconv.FormatUint(uint64(r.UID), 10)
}

// summary is the lightweight per-message view used by the list command.
type summary struct {
	Ref       msgRef
	From      string
	Subject   string
	Date      time.Time
	Flags     []string
	Size      uint32
	Preview   string
	HasAttach bool
}

// message is a fully retrieved mail: its IMAP metadata plus the raw RFC822
// bytes.
type message struct {
	summary
	Raw []byte
}

// listMessages returns the newest messages of a folder, newest first.
func (c *Conn) listMessages(mailbox string, limit int, unreadOnly bool, wantPreview bool) (*imap.MailboxStatus, []summary, error) {
	status, err := c.selectBox(mailbox)
	if err != nil {
		return nil, nil, err
	}
	if status.Messages == 0 {
		return status, nil, nil
	}

	seqset, err := c.rangeSeqSet(status, limit, unreadOnly)
	if err != nil {
		if errors.Is(err, errNoMatches) {
			return status, nil, nil
		}
		return status, nil, err
	}

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		imap.FetchBodyStructure,
	}

	messages := make(chan *imap.Message, 128)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	var out []summary
	for msg := range messages {
		sum := summary{Ref: msgRef{Mailbox: mailbox, UID: msg.Uid}}
		if msg.Envelope != nil {
			sum.From = formatAddresses(msg.Envelope.From)
			sum.Subject = oneLine(msg.Envelope.Subject)
			if !msg.Envelope.Date.IsZero() {
				sum.Date = msg.Envelope.Date
			}
		}
		if sum.Date.IsZero() {
			sum.Date = msg.InternalDate
		}
		sum.Flags = msg.Flags
		sum.Size = msg.Size
		if msg.BodyStructure != nil {
			sum.HasAttach = hasAttachment(msg.BodyStructure)
		}
		out = append(out, sum)
	}
	if err := <-done; err != nil {
		return status, nil, fmt.Errorf("cannot fetch messages: %w", err)
	}

	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	if wantPreview {
		c.attachPreviews(mailbox, out, 240)
	}
	return status, out, nil
}

var errNoMatches = errors.New("no matching messages")

// rangeSeqSet builds the sequence set of messages to fetch: the newest
// `limit` messages, optionally restricted to unread ones.
func (c *Conn) rangeSeqSet(status *imap.MailboxStatus, limit int, unreadOnly bool) (*imap.SeqSet, error) {
	if unreadOnly {
		criteria := imap.NewSearchCriteria()
		criteria.WithoutFlags = []string{imap.SeenFlag}
		seqNums, err := c.cli.Search(criteria)
		if err != nil {
			return nil, fmt.Errorf("cannot search for unread messages: %w", err)
		}
		if len(seqNums) == 0 {
			return nil, errNoMatches
		}
		if len(seqNums) > limit {
			seqNums = seqNums[len(seqNums)-limit:]
		}
		seqset := new(imap.SeqSet)
		for _, num := range seqNums {
			seqset.AddNum(num)
		}
		return seqset, nil
	}

	from := uint32(1)
	if uint32(limit) < status.Messages {
		from = status.Messages - uint32(limit) + 1
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, status.Messages)
	return seqset, nil
}

// attachPreviews fills in a one-line text preview for each message. Bodies are
// peeked at, so listing messages never marks them as read.
func (c *Conn) attachPreviews(mailbox string, sums []summary, maxBytes int) {
	for i := range sums {
		section := &imap.BodySectionName{}
		section.Specifier = imap.TextSpecifier
		section.Peek = true
		section.Partial = []int{0, maxBytes}

		seqset := new(imap.SeqSet)
		seqset.AddNum(sums[i].Ref.UID)

		messages := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() {
			done <- c.cli.UidFetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
		}()

		var raw string
		for msg := range messages {
			if body := msg.GetBody(section); body != nil {
				buf := make([]byte, maxBytes)
				n, _ := io.ReadFull(body, buf)
				raw = string(buf[:n])
			}
		}
		<-done

		sums[i].Preview = makePreview(raw)
	}
}

// rawSection addresses the complete message including its headers. It is not a
// PEEK, which is what causes the message to be marked as read.
func rawSection() *imap.BodySectionName {
	return &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{Specifier: imap.EntireSpecifier},
	}
}

// fetchMessage retrieves a single message with its full raw RFC822 body.
//
// The body is fetched without PEEK, so the message is marked as read.
func (c *Conn) fetchMessage(ref msgRef) (*message, error) {
	status, err := c.selectBox(ref.Mailbox)
	if err != nil {
		return nil, err
	}
	if status.ReadOnly {
		return nil, fmt.Errorf("folder %q is read-only", ref.Mailbox)
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ref.UID)

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		imap.FetchRFC822,
	}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var found *imap.Message
	for msg := range messages {
		found = msg
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("cannot fetch message %s: %w", ref, err)
	}
	if found == nil {
		return nil, fmt.Errorf("message %s not found in folder %q (it may have been moved or deleted)", ref, ref.Mailbox)
	}

	out := &message{summary: summary{Ref: ref, Flags: found.Flags, Size: found.Size}}
	if found.Envelope != nil {
		out.From = formatAddresses(found.Envelope.From)
		out.Subject = oneLine(found.Envelope.Subject)
		out.Date = found.Envelope.Date
	}
	if out.Date.IsZero() {
		out.Date = found.InternalDate
	}

	if body := found.GetBody(rawSection()); body != nil {
		raw, err := readLimited(body, maxMessageBytes)
		if err != nil {
			return nil, fmt.Errorf("cannot read body of %s: %w", ref, err)
		}
		out.Raw = raw
	}
	return out, nil
}

// moveMessage moves a message into the archive folder and returns its name.
func (c *Conn) moveMessage(ref msgRef) (string, error) {
	status, err := c.selectBox(ref.Mailbox)
	if err != nil {
		return "", err
	}
	if status.ReadOnly {
		return "", fmt.Errorf("folder %q is read-only, cannot archive from it", ref.Mailbox)
	}

	dest, err := c.resolveArchive()
	if err != nil {
		return "", err
	}
	if dest == ref.Mailbox {
		return "", fmt.Errorf("message %s is already in the archive folder %q", ref, dest)
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ref.UID)
	if err := c.cli.UidMove(seqset, dest); err != nil {
		return "", fmt.Errorf("cannot move %s to %q: %w", ref, dest, err)
	}
	return dest, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}
