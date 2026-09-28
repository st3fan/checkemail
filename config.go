package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Account is a single IMAP account as stored in accounts.json.
//
// Only implicit TLS (IMAPS) on port 993 is supported. "server" may include an
// explicit ":port"; otherwise 993 is used.
type Account struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`

	// Port is optional and only used when "server" carries no port itself.
	Port int `json:"port,omitempty"`
	// Archive is the optional name of the folder that "archive" moves mail
	// into. When empty a folder advertised with the \Archive special-use
	// attribute is used, otherwise a folder literally named "Archive" is
	// created and used.
	Archive string `json:"archive,omitempty"`
}

// Accounts maps an account name to its configuration. The account name is the
// key used in the command line: checkemail list <accountname>.
type Accounts map[string]Account

// defaultAccountsPath resolves the location of accounts.json, honouring
// XDG_CONFIG_HOME and the CHECKEMAIL_ACCOUNTS override.
func defaultAccountsPath() string {
	if p := os.Getenv("CHECKEMAIL_ACCOUNTS"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "checkemail", "accounts.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "checkemail/accounts.json"
	}
	return filepath.Join(home, ".config", "checkemail", "accounts.json")
}

func loadAccounts() (Accounts, string, error) {
	path := defaultAccountsPath()

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, fmt.Errorf("no accounts file at %s", path)
		}
		return nil, path, fmt.Errorf("cannot access accounts file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %04o); run: chmod 600 %s\n",
			path, perm, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("cannot read accounts file %s: %w", path, err)
	}

	accounts := Accounts{}
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, path, fmt.Errorf("cannot parse accounts file %s: %w", path, err)
	}

	for name, acct := range accounts {
		if acct.Server == "" {
			return nil, path, fmt.Errorf("account %q in %s has no \"server\"", name, path)
		}
		if acct.Username == "" {
			return nil, path, fmt.Errorf("account %q in %s has no \"username\"", name, path)
		}
		if acct.Password == "" {
			return nil, path, fmt.Errorf("account %q in %s has no \"password\"", name, path)
		}
	}

	return accounts, path, nil
}

// lookup resolves an account name. The exact key wins; otherwise a
// case-insensitive match is attempted so agents need not worry about casing.
func (a Accounts) lookup(name string) (Account, error) {
	if acct, ok := a[name]; ok {
		return acct, nil
	}
	for key, acct := range a {
		if strings.EqualFold(key, name) {
			return acct, nil
		}
	}
	return Account{}, fmt.Errorf("unknown account %q; known accounts: %s",
		name, strings.Join(a.names(), ", "))
}

func (a Accounts) names() []string {
	names := make([]string, 0, len(a))
	for name := range a {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
