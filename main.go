// checkemail is a small read-only IMAP client built for AI agents.
//
// It connects to the accounts listed in ~/.config/checkemail/accounts.json
// over implicit TLS (IMAPS, port 993) and can list, read and archive
// messages. It can never send mail.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/emersion/go-imap"
)

const usageText = `checkemail - read email over IMAP/TLS, built for AI agents

Usage:
  checkemail list <account> [flags]        Show recent messages
  checkemail read <account> <id>           Show one message in full (marks it read)
  checkemail archive <account> <id>        Move one message to the archive folder
  checkemail folders <account>             Show all folders and their counts
  checkemail accounts                      Show configured account names

Message id:
  "<folder>:<uid>"   e.g. "INBOX:4821"   -- the form printed by list
  "<uid>"            e.g. "4821"         -- shorthand for a message in INBOX

List flags:
  -folder string   folder to read (default "INBOX")
  -n int           number of messages to show (default 20)
  -unread          only unread messages
  -no-preview      skip the one-line body preview (faster)

Read flags:
  -raw             keep quoted replies and the signature instead of stripping them

Accounts are read from $CHECKEMAIL_ACCOUNTS, else $XDG_CONFIG_HOME/checkemail/accounts.json,
else ~/.config/checkemail/accounts.json. The format is:

  {
    "you@example.com": {
      "server": "imap.example.com",
      "username": "you@example.com",
      "password": "app-specific-password",
      "archive": "Archive"
    }
  }

Only implicit TLS on port 993 is supported. "archive" is optional; without it
a folder flagged \Archive is used, otherwise one named "Archive".

This tool can only read and move mail. It cannot send, delete or reply.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("no command given")
	}

	switch args[0] {
	case "list":
		return cmdList(args[1:])
	case "read":
		return cmdRead(args[1:])
	case "archive":
		return cmdArchive(args[1:])
	case "folders":
		return cmdFolders(args[1:])
	case "accounts":
		return cmdAccounts(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	default:
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// newFlagSet builds a flag set that reports errors through the returned error
// rather than writing to stderr and exiting.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseFlags parses flags that may appear before, between or after the
// positional arguments. The standard library stops at the first positional
// argument, which is surprising for callers (and for agents) that habitually
// write "checkemail list acct -n 5".
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// "--" ends flag parsing; everything after it is positional.
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		name := strings.TrimLeft(arg, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			flagArgs = append(flagArgs, arg)
			continue // value is attached
		}

		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("unknown flag %q (run with -h to see the options)", arg)
		}
		if isBoolFlag(f) {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag %q needs a value", arg)
		}
		i++
		flagArgs = append(flagArgs, arg, args[i])
	}

	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return append(positional, fs.Args()...), nil
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

func cmdAccounts(args []string) error {
	fs := newFlagSet("accounts")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	accounts, path, err := loadAccounts()
	if err != nil {
		return err
	}

	out := os.Stdout
	kv(out, "CONFIG", path)
	kv(out, "ACCOUNTS", fmt.Sprintf("%d configured", len(accounts)))
	for _, name := range accounts.names() {
		acct := accounts[name]
		host, port, _ := splitHostPort(acct.Server, acct.Port)
		fmt.Fprintf(out, "  - %s  (user %q on %s:%d)\n", name, acct.Username, host, port)
	}
	return nil
}

// connect loads the named account and opens an IMAP/TLS session.
func connect(name string) (*Conn, error) {
	accounts, _, err := loadAccounts()
	if err != nil {
		return nil, err
	}
	acct, err := accounts.lookup(name)
	if err != nil {
		return nil, err
	}
	return dialAccount(name, acct)
}

func cmdList(args []string) error {
	fs := newFlagSet("list")
	folder := fs.String("folder", "INBOX", "folder to read")
	limit := fs.Int("n", 20, "number of messages to show")
	unread := fs.Bool("unread", false, "only show unread messages")
	preview := fs.Bool("no-preview", false, "skip the body preview")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: checkemail list <account> [-folder name] [-n count] [-unread] [-no-preview]")
	}
	if *limit < 1 {
		return fmt.Errorf("-n must be at least 1")
	}

	account := rest[0]
	conn, err := connect(account)
	if err != nil {
		return err
	}
	defer conn.Close()

	mailbox := canonicalFolder(*folder)

	stats, err := conn.status(mailbox)
	if err != nil {
		return err
	}
	summary := &folderSummary{Total: stats.Messages, Unread: stats.Unseen}

	_, msgs, err := conn.listMessages(mailbox, *limit, *unread, !*preview)
	if err != nil {
		return err
	}

	out := os.Stdout
	printHeader(out, account, mailbox, summary)

	if len(msgs) == 0 {
		what := "messages"
		if *unread {
			what = "unread messages"
		}
		fmt.Fprintf(out, "\nNo %s in %q.\n", what, mailbox)
		return nil
	}

	printMessages(out, msgs, len(msgs), int(summary.Total))
	fmt.Fprintf(out, "\n\nTo read a message: checkemail read %q %q\n",
		account, msgs[0].Ref.String())
	fmt.Fprintf(out, "To archive it:     checkemail archive %q %q\n",
		account, msgs[0].Ref.String())
	return nil
}

func cmdRead(args []string) error {
	fs := newFlagSet("read")
	raw := fs.Bool("raw", false, "keep quoted replies and signatures")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: checkemail read <account> <messageid> [-raw]")
	}

	account := rest[0]
	ref, err := parseMsgRef(rest[1])
	if err != nil {
		return err
	}

	conn, err := connect(account)
	if err != nil {
		return err
	}
	defer conn.Close()

	msg, err := conn.fetchMessage(ref)
	if err != nil {
		return err
	}

	parsed, err := parseMessage(msg.Raw)
	if err != nil {
		// Fall back to the raw bytes so the agent still sees something useful
		// rather than nothing at all.
		parsed = &parsedMessage{Text: string(msg.Raw)}
	}

	printMessage(os.Stdout, account, msg, parsed, !*raw)
	return nil
}

func cmdArchive(args []string) error {
	fs := newFlagSet("archive")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: checkemail archive <account> <messageid>")
	}

	account := rest[0]
	ref, err := parseMsgRef(rest[1])
	if err != nil {
		return err
	}

	conn, err := connect(account)
	if err != nil {
		return err
	}
	defer conn.Close()

	dest, err := conn.moveMessage(ref)
	if err != nil {
		return err
	}

	printArchived(os.Stdout, account, ref, dest)
	return nil
}

func cmdFolders(args []string) error {
	fs := newFlagSet("folders")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: checkemail folders <account>")
	}

	account := rest[0]
	conn, err := connect(account)
	if err != nil {
		return err
	}
	defer conn.Close()

	boxes, err := conn.mailboxes()
	if err != nil {
		return err
	}

	folders := make([]folder, 0, len(boxes))
	for _, box := range boxes {
		f := folder{Name: box.Name, Notes: folderNotes(box.Attributes)}
		if st, err := conn.status(box.Name); err == nil {
			f.Total = st.Messages
			f.Unread = st.Unseen
		} else {
			f.Notes = append(f.Notes, "count unavailable")
		}
		folders = append(folders, f)
	}

	printFolders(os.Stdout, account, folders)
	return nil
}

// folderNotes turns special-use attributes into readable notes.
func folderNotes(attrs []string) []string {
	special := []string{
		imap.ArchiveAttr, imap.AllAttr, imap.DraftsAttr, imap.FlaggedAttr,
		imap.JunkAttr, imap.SentAttr, imap.TrashAttr, imap.ImportantAttr,
	}
	notes := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		for _, s := range special {
			if attr == s {
				notes = append(notes, strings.TrimPrefix(attr, `\`))
			}
		}
	}
	return notes
}

func canonicalFolder(name string) string {
	if strings.TrimSpace(name) == "" {
		return "INBOX"
	}
	return strings.TrimSpace(name)
}
