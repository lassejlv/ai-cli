// Package auth stores provider credentials saved by `ai login`. Each provider
// can hold several named accounts; one of them is the provider's default.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DefaultAccount is the account name used when none is given.
const DefaultAccount = "default"

// Credential holds whatever a provider needs to authenticate.
type Credential struct {
	APIKey    string `json:"apiKey,omitempty"`
	APISecret string `json:"apiSecret,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`

	// OAuth (Codex / Grok subscriptions).
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	AccountID    string `json:"accountId,omitempty"`
	LastRefresh  string `json:"lastRefresh,omitempty"`
	// TokenEndpoint is the discovered OAuth token endpoint (Grok).
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
	// Email is a display label for OAuth accounts.
	Email string `json:"email,omitempty"`
	// Link points the account at another tool's credentials ("codex-cli"),
	// so rotating refresh tokens have a single owner.
	Link string `json:"link,omitempty"`
}

// ProviderAccounts holds the named accounts of one provider.
type ProviderAccounts struct {
	Default  string                `json:"default,omitempty"`
	Accounts map[string]Credential `json:"accounts"`
}

// File is the on-disk auth.json layout.
type File struct {
	// APIKey is the legacy AI Gateway key written by the TypeScript ai-cli.
	APIKey          string                       `json:"apiKey,omitempty"`
	DefaultProvider string                       `json:"defaultProvider,omitempty"`
	Providers       map[string]*ProviderAccounts `json:"providers,omitempty"`

	// hadLegacy records that the file carried the TypeScript CLI's root apiKey,
	// which is kept in sync so that CLI stays signed in.
	hadLegacy bool
}

var mu sync.Mutex

// ConfigDir returns $AI_CLI_CONFIG_DIR, $XDG_CONFIG_HOME/ai-cli or ~/.config/ai-cli.
func ConfigDir() string {
	if dir := os.Getenv("AI_CLI_CONFIG_DIR"); dir != "" {
		return dir
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ai-cli")
}

// Path returns the auth.json path.
func Path() string { return filepath.Join(ConfigDir(), "auth.json") }

// Load reads auth.json; a missing file yields an empty File.
func Load() (*File, error) {
	mu.Lock()
	defer mu.Unlock()
	return load()
}

func load() (*File, error) {
	f := &File{Providers: map[string]*ProviderAccounts{}}
	data, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", Path(), err)
	}
	if f.Providers == nil {
		f.Providers = map[string]*ProviderAccounts{}
	}
	for name, p := range f.Providers {
		if p == nil {
			delete(f.Providers, name)
			continue
		}
		if p.Accounts == nil {
			p.Accounts = map[string]Credential{}
		}
	}
	if f.APIKey != "" {
		f.hadLegacy = true
		p := f.provider("gateway")
		if _, ok := p.Accounts[DefaultAccount]; !ok {
			p.Accounts[DefaultAccount] = Credential{APIKey: f.APIKey}
		}
		f.APIKey = ""
	}
	return f, nil
}

func (f *File) provider(name string) *ProviderAccounts {
	p, ok := f.Providers[name]
	if !ok {
		p = &ProviderAccounts{Accounts: map[string]Credential{}}
		f.Providers[name] = p
	}
	return p
}

// AccountNames returns a provider's account names, default first.
func (f *File) AccountNames(provider string) []string {
	p, ok := f.Providers[provider]
	if !ok {
		return nil
	}
	def := p.DefaultName()
	names := make([]string, 0, len(p.Accounts))
	for name := range p.Accounts {
		if name != def {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if _, ok := p.Accounts[def]; ok {
		names = append([]string{def}, names...)
	}
	return names
}

// DefaultName resolves which account is the provider default.
func (p *ProviderAccounts) DefaultName() string {
	if _, ok := p.Accounts[p.Default]; ok && p.Default != "" {
		return p.Default
	}
	if _, ok := p.Accounts[DefaultAccount]; ok {
		return DefaultAccount
	}
	names := make([]string, 0, len(p.Accounts))
	for name := range p.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// Lookup finds a credential; an empty account selects the provider default.
func (f *File) Lookup(provider, account string) (Credential, string, bool) {
	p, ok := f.Providers[provider]
	if !ok {
		return Credential{}, "", false
	}
	if account == "" {
		account = p.DefaultName()
	}
	c, ok := p.Accounts[account]
	return c, account, ok
}

func save(f *File) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	for name, p := range f.Providers {
		if len(p.Accounts) == 0 {
			delete(f.Providers, name)
		}
	}
	out := *f
	out.APIKey = ""
	if f.hadLegacy {
		if c, _, ok := f.Lookup("gateway", ""); ok {
			out.APIKey = c.APIKey
		}
	}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(Path(), append(data, '\n'))
}

// Update applies fn to the stored file under a lock and saves the result.
func Update(fn func(*File) error) error {
	mu.Lock()
	defer mu.Unlock()
	return withFileLock(Path(), func() error {
		f, err := load()
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
		return save(f)
	})
}

// RefreshFunc exchanges a credential's refresh token for new tokens.
type RefreshFunc func(Credential) (Credential, error)

// RefreshStored refreshes a saved OAuth account under the auth file lock.
// When another process already refreshed it, the stored tokens are reused
// instead of spending the (single-use) refresh token again.
func RefreshStored(provider, account string, current Credential, force bool, expiring func(Credential) bool, refresh RefreshFunc) (Credential, error) {
	var result Credential
	err := Update(func(f *File) error {
		stored, _, ok := f.Lookup(provider, account)
		if !ok {
			stored = current
		}
		if stored.AccessToken != current.AccessToken && !expiring(stored) {
			result = stored
			return nil
		}
		if !force && !expiring(stored) {
			result = stored
			return nil
		}
		next, err := refresh(stored)
		if err != nil {
			return err
		}
		f.provider(provider).Accounts[account] = next
		result = next
		return nil
	})
	return result, err
}

// Get returns a stored credential and the resolved account name.
func Get(provider, account string) (Credential, string, bool) {
	f, err := Load()
	if err != nil {
		return Credential{}, "", false
	}
	return f.Lookup(provider, account)
}

// Set stores a credential. The first account of a provider becomes its default.
func Set(provider, account string, c Credential, makeDefault bool) error {
	if account == "" {
		account = DefaultAccount
	}
	return Update(func(f *File) error {
		p := f.provider(provider)
		first := len(p.Accounts) == 0
		p.Accounts[account] = c
		if makeDefault || first {
			p.Default = account
		}
		return nil
	})
}

// Replace updates an existing account's credential without changing defaults.
func Replace(provider, account string, c Credential) error {
	return Update(func(f *File) error {
		p := f.provider(provider)
		p.Accounts[account] = c
		return nil
	})
}

// Delete removes one account, or every account of a provider when account is "".
func Delete(provider, account string) (int, error) {
	removed := 0
	err := Update(func(f *File) error {
		p, ok := f.Providers[provider]
		if !ok {
			return nil
		}
		if account == "" {
			removed = len(p.Accounts)
			delete(f.Providers, provider)
		} else if _, ok := p.Accounts[account]; ok {
			delete(p.Accounts, account)
			removed = 1
			if p.Default == account {
				p.Default = ""
			}
		}
		if _, still := f.Providers[provider]; (!still || len(p.Accounts) == 0) && f.DefaultProvider == provider {
			f.DefaultProvider = ""
		}
		return nil
	})
	return removed, err
}

// DeleteAll removes the whole auth file.
func DeleteAll() error {
	mu.Lock()
	defer mu.Unlock()
	err := os.Remove(Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SetDefaultAccount marks an existing account as the provider default.
func SetDefaultAccount(provider, account string) error {
	return Update(func(f *File) error {
		p, ok := f.Providers[provider]
		if !ok {
			return fmt.Errorf("no saved accounts for %s", provider)
		}
		if _, ok := p.Accounts[account]; !ok {
			return fmt.Errorf("no %s account named %q (have: %v)", provider, account, f.AccountNames(provider))
		}
		p.Default = account
		return nil
	})
}

// SetDefaultProvider stores the provider used for unprefixed model IDs.
func SetDefaultProvider(provider string) error {
	return Update(func(f *File) error {
		f.DefaultProvider = provider
		return nil
	})
}
