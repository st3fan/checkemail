# checkemail

A read-only IMAP client designed to be driven by AI agents.

Output is plain, labelled text: every field is on a `LABEL: value` line, so a
model can read it directly and a script can grep it.

## Why does this exist

Pentesting agents often need to test flows in applications that require an
email account: sign up, forgot password, magic links, email confirmation, and
so on. Those flows only complete once the agent can act on the mail the
application sends.

`checkemail` is the small piece that closes that loop. It works with
predefined test accounts and gives the agent just three things: list the
messages in an inbox, read a single message, and archive a message once it has
been dealt with. That is enough to exercise any application functionality that
generates an email the agent has to act on, without handing the agent a full
mail client.

## Install

Install straight from the repo:

```sh
go install github.com/st3fan/checkemail@latest
```

This puts the `checkemail` binary in `$(go env GOPATH)/bin`.

Or build from a checkout:

```sh
go build -o checkemail .
```

## Configure

Accounts live in `~/.config/checkemail/accounts.json`. The key is the account
name you pass on the command line. The file holds credentials, so keep it
private:

```sh
chmod 600 ~/.config/checkemail/accounts.json
```

```json
{
  "ada@example.com": {
    "server": "imap.example.com",
    "username": "ada@example.com",
    "password": "app-specific-password"
  },
  "bob@example.com": {
    "server": "imap.example.com:993",
    "username": "bob@example.com",
    "password": "app-specific-password",
    "archive": "Saved Items"
  }
}
```

| field | required | meaning |
| --- | --- | --- |
| `server` | yes | IMAP host. May include `:port`; defaults to 993. |
| `username` | yes | Login name. |
| `password` | yes | Use an app-specific password, not the real one. |
| `port` | no | Only used when `server` has no port. |
| `archive` | no | Destination folder for `archive`. Autodetected when omitted. |

`$XDG_CONFIG_HOME` and `$CHECKEMAIL_ACCOUNTS` are honoured, in that order of
override.

## Use

```
checkemail list <account> [flags]        Show recent messages
checkemail read <account> <id>           Show one message in full (marks it read)
checkemail archive <account> <id>        Move one message to the archive folder
checkemail folders <account>             Show all folders and their counts
checkemail accounts                      Show configured account names
```

```sh
checkemail list ada@example.com
checkemail list ada@example.com -n 50 -unread -folder Archive
checkemail read ada@example.com "INBOX:4821"
checkemail read ada@example.com "INBOX:4821" -raw
checkemail archive ada@example.com "INBOX:4821"
```

Flags may appear anywhere on the command line, before or after the account
name.

### Message ids

IMAP uids are only meaningful inside one folder, so an id is
`"<folder>:<uid>"` — exactly the form `list` prints. A bare number is
shorthand for a message in `INBOX`.

### list flags

| flag | default | effect |
| --- | --- | --- |
| `-folder` | `INBOX` | folder to read |
| `-n` | `20` | how many messages to show |
| `-unread` | off | only unread messages |
| `-no-preview` | off | skip the body preview; one round trip instead of N |

### read flags

| flag | default | effect |
| --- | --- | --- |
| `-raw` | off | keep quoted replies and the signature |

By default `read` strips the quoted reply chain and the signature so the
agent sees only the newly written text. `text/html` is converted to text;
`text/plain` is always preferred. Attachments are listed with name, type and
size but never decoded.

## Behaviour worth knowing

- **Reading marks mail as read.** `read` fetches the body without `PEEK`,
  which is what sets `\Seen`. `list` only reads envelopes, plus a `PEEK` of the
  first few body bytes for the preview, so browsing never marks anything read.
- **Archive moves, it does not copy.** The message leaves the source folder.
  The destination is the account's `archive` setting, else the folder flagged
  `\Archive`, else an existing `Archive`/`Archives` folder, else a newly created
  `Archive`.
- **TLS only.** Implicit TLS on port 993. There is no plaintext and no
  STARTTLS path.
- **No sending.** There is no code path that can send, reply, edit or delete
  mail. `archive` is the only command that changes anything, and it only moves.

## How to make your agent use this

`checkemail` is not an MCP server or a plugin; it is a plain command-line tool.
An agent uses it the same way a person would: by running it in a shell. So you
make your agent use it by telling the agent that the tool exists, what it is
for, and the exact commands to run.

A prompt that works well:

```text
You have a tool called `checkemail` for reading email from test accounts.
Use it to complete email-driven flows (signup, password reset, magic links).

- List unread mail:   checkemail list <account> -n 20 -unread
- Read one message:   checkemail read <account> "<folder>:<uid>"
- Archive it:         checkemail archive <account> "<folder>:<uid>"

The account name is printed by `checkemail accounts`. `list` never marks mail
read, so it is safe to poll. Read the `ID:` field, not the list index. Exit
status is 0 on success, 1 on error, with `ERROR: <message>` on stderr.
```

Give the agent the real account names (or let it discover them with
`checkemail accounts`) and the loop is self-contained: poll with `list`, act on
the `ID:` it prints with `read`, then `archive` once the mail has been dealt
with. Nothing else needs to change on the agent side.

(This should probably be a skill)

## Agent notes

Typical loop:

```sh
checkemail list ada@example.com -n 20 -unread
checkemail read ada@example.com "INBOX:4821"
checkemail archive ada@example.com "INBOX:4821"
```

- `list` never marks mail read, so it is safe to poll.
- Read the `ID:` field, not the list index; indices shift as new mail arrives,
  uids do not.
- Exit status is `0` on success, `1` on error. Errors go to stderr as
  `ERROR: <message>`.
- Account names are matched case-insensitively; an unknown name is an error
  that lists the known ones.
