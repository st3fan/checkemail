package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/emersion/go-imap"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maxTextPartBytes = 1 << 20 // 1 MiB of text kept per part
	maxTotalText     = 4 << 20 // 4 MiB of text kept across all parts
	maxMimeDepth     = 20
)

// attachment describes a non-text part of a message. The content itself is
// never decoded, only described.
type attachment struct {
	Filename string
	MIMEType string
	Size     int64
	Part     string
}

// parsedMessage is the decoded form of a raw RFC822 message.
type parsedMessage struct {
	Header      mail.Header
	Text        string
	Attachments []attachment
	// PlainText is false when the text had to be derived from an HTML part.
	PlainText bool
}

type collector struct {
	text        strings.Builder
	hasPlain    bool
	hasHTML     bool
	attachments []attachment
}

// parseMessage decodes a raw RFC822 message into readable text.
//
// A text/plain part is preferred; if there is none, an HTML part is converted
// to text. Forwarded (message/rfc822) messages are unwrapped so their content
// is visible too.
func parseMessage(raw []byte) (*parsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("cannot parse message: %w", err)
	}

	c := &collector{}
	collect(c, textproto.MIMEHeader(msg.Header), msg.Body, "1", 0, false)

	return &parsedMessage{
		Header:      msg.Header,
		Text:        strings.TrimSpace(c.text.String()),
		Attachments: c.attachments,
		PlainText:   c.hasPlain || !c.hasHTML,
	}, nil
}

// collect walks a MIME entity, accumulating the best text candidate and every
// attachment found along the way.
func collect(c *collector, hdr textproto.MIMEHeader, body io.Reader, path string, depth int, forwarded bool) {
	if depth > maxMimeDepth {
		return
	}

	mediaType, params := parseTypeParams(hdr)
	disposition := strings.ToLower(firstToken(params["disposition"]))

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(body, boundary)
		for i := 1; ; i++ {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			collect(c, part.Header, part, fmt.Sprintf("%s.%d", path, i), depth+1, forwarded)
			part.Close()
		}
	}

	cr := &countingReader{r: body}
	decoded := decodeBody(cr, hdr, params["charset"])

	switch {
	case mediaType == "text/plain" && !isAttachment(disposition, params):
		c.addText(string(decoded), true, forwarded)
	case mediaType == "text/html" && !isAttachment(disposition, params):
		c.addText(htmlToText(string(decoded)), false, forwarded)
	case mediaType == "message/rfc822":
		// Forwarded mail: surface the inner text too.
		if inner, err := mail.ReadMessage(bytes.NewReader(decoded)); err == nil {
			collect(c, textproto.MIMEHeader(inner.Header), inner.Body, path+".rfc822", depth+1, true)
		}
	default:
		c.attachments = append(c.attachments, attachment{
			Filename: attachmentName(params, disposition),
			MIMEType: mediaType,
			Size:     cr.n,
			Part:     path,
		})
	}
}

// addText keeps the text/plain version of the body in preference to text
// converted from HTML.
//
// Text from a forwarded (message/rfc822) part is appended rather than replacing
// what came before, since the forwarding note and the forwarded mail are both
// worth reading.
func (c *collector) addText(text string, plain, forwarded bool) {
	text = strings.TrimSpace(text)
	if text == "" || c.text.Len() >= maxTotalText {
		return
	}
	if len(text) > maxTextPartBytes {
		text = text[:maxTextPartBytes] + "\n[... truncated ...]"
	}

	if forwarded {
		if c.text.Len() > 0 {
			c.text.WriteString("\n\n[forwarded message]\n")
		}
		if plain {
			c.hasPlain = true
		} else {
			c.hasHTML = true
		}
		c.text.WriteString(text)
		return
	}

	if plain && !c.hasPlain {
		c.hasPlain = true
		c.text.Reset()
		c.text.WriteString(text)
		return
	}
	if !plain && !c.hasPlain && !c.hasHTML {
		c.hasHTML = true
		c.text.WriteString("[no text/plain part; converted from HTML]\n")
		c.text.WriteString(text)
	}
}

// parseTypeParams parses the Content-Type and Content-Disposition headers into a
// single normalised parameter map. The media type is lowercased; parameter
// names are lowercased as well.
func parseTypeParams(hdr textproto.MIMEHeader) (mediaType string, params map[string]string) {
	params = make(map[string]string)
	mediaType = "text/plain"

	raw := hdr.Get("Content-Type")
	if raw == "" {
		raw = "text/plain"
	}
	mt, ctParams, err := mime.ParseMediaType(raw)
	if err == nil {
		mediaType = strings.ToLower(mt)
		for k, v := range ctParams {
			params[strings.ToLower(k)] = v
		}
	} else {
		// Malformed Content-Type: fall back to the leading token.
		mediaType = strings.ToLower(strings.TrimSpace(firstToken(raw)))
	}

	if disp := hdr.Get("Content-Disposition"); disp != "" {
		if _, dp, err := mime.ParseMediaType(disp); err == nil {
			for k, v := range dp {
				params[strings.ToLower(k)] = v
			}
		}
	}
	return mediaType, params
}

func isAttachment(disposition string, params map[string]string) bool {
	if disposition == "attachment" {
		return true
	}
	_, hasName := params["filename"]
	return hasName
}

func attachmentName(params map[string]string, disposition string) string {
	if name := params["filename"]; name != "" {
		return name
	}
	if name := params["name"]; name != "" {
		return name
	}
	if id := params["content-id"]; id != "" {
		return strings.Trim(id, "<> ")
	}
	if disposition != "" {
		return "(inline " + disposition + " part)"
	}
	return "(unnamed)"
}

func firstToken(s string) string {
	if idx := strings.IndexByte(s, ';'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// countingReader counts the bytes flowing through it without storing them, so
// attachment sizes are known without buffering the payload.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// decodeBody undoes the Content-Transfer-Encoding and transcodes the charset to
// UTF-8.
func decodeBody(r io.Reader, hdr textproto.MIMEHeader, charsetName string) []byte {
	limited := io.LimitReader(r, maxTextPartBytes+1)

	var raw []byte
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Content-Transfer-Encoding"))) {
	case "base64":
		data, _ := io.ReadAll(limited)
		decoded, err := base64.StdEncoding.DecodeString(stripWhitespace(string(data)))
		if err != nil {
			// Tolerate unpadded or line-broken base64.
			decoded, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(stripWhitespace(string(data)), "="))
		}
		raw = decoded
	case "quoted-printable":
		raw, _ = io.ReadAll(quotedprintable.NewReader(limited))
	default: // 7bit, 8bit, binary, or absent
		raw, _ = io.ReadAll(limited)
	}

	if len(raw) > maxTextPartBytes {
		raw = raw[:maxTextPartBytes]
	}

	charsetName = strings.TrimSpace(charsetName)
	if charsetName == "" {
		charsetName = strings.ToLower(firstToken(hdr.Get("Content-Type")))
	}
	if charsetName != "" && charsetName != "utf-8" && charsetName != "us-ascii" {
		if reader, err := charset.NewReaderLabel(charsetName, bytes.NewReader(raw)); err == nil {
			if converted, err := io.ReadAll(reader); err == nil {
				raw = converted
			}
		}
	}
	return raw
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
}

// htmlToText converts an HTML document into readable plain text.
func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return collapseSpace(stripTags(s))
	}

	var b strings.Builder
	isBlock := func(tag string) bool {
		switch tag {
		case "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6",
			"table", "blockquote", "section", "article", "ul", "ol", "pre":
			return true
		}
		return false
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "noscript", "svg":
				return
			}
			if isBlock(n.Data) {
				b.WriteString("\n")
			} else if n.Data == "br" {
				b.WriteString("\n")
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if isBlock(n.Data) {
				b.WriteString("\n")
			}
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return collapseSpace(unescapeEntities(b.String()))
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	entityRe = regexp.MustCompile(`&(?:[a-zA-Z][a-zA-Z0-9]{1,31}|#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6});`)
)

func stripTags(s string) string { return tagRe.ReplaceAllString(s, "") }

func unescapeEntities(s string) string { return entityRe.ReplaceAllStringFunc(s, html.UnescapeString) }

// collapseSpace normalises line endings, trims trailing spaces and squeezes runs
// of blank lines.
func collapseSpace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// A block boundary written as "\n\n" collapses to a single blank line.
	s = strings.ReplaceAll(s, "\n\n\n", "\n\n")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(s)
}

// hasAttachment walks a BODYSTRUCTURE looking for parts that are not displayable
// text.
func hasAttachment(bs *imap.BodyStructure) bool {
	found := false
	bs.Walk(func(_ []int, part *imap.BodyStructure) bool {
		if found {
			return false
		}
		// Multipart parts are containers, not content.
		if part.MIMEType == "multipart" {
			return true
		}
		ct := strings.ToLower(part.MIMEType + "/" + part.MIMESubType)
		if _, hasName := part.DispositionParams["filename"]; hasName {
			found = true
			return false
		}
		if part.Disposition == "attachment" {
			found = true
			return false
		}
		if !strings.HasPrefix(ct, "text/") && ct != "message/rfc822" {
			found = true
			return false
		}
		return true
	})
	return found
}
