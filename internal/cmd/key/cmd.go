package key

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	apikeytoml "github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
)

// Cmd is the parent command for `kvarn key <subcommand>`. These subcommands
// edit the orchestrator's local apikeys.toml directly, so they run on the host
// where that file lives — keys are bootstrapped without a running orchestrator.
type Cmd struct {
	Create  CreateCmd  `cmd:"" help:"Create a new API key."`
	List    ListCmd    `cmd:"" help:"List API keys."`
	Revoke  RevokeCmd  `cmd:"" help:"Delete an API key."`
	Disable DisableCmd `cmd:"" help:"Disable a key without deleting it."`
}

func openStore(path string) apikey.Store {
	return apikeytoml.OpenDefault(path)
}

// CreateCmd mints a key and prints the full token once.
type CreateCmd struct {
	APIKeysFile string   `help:"Path to API keys TOML file." default:""`
	Name        string   `help:"Human-readable name for the key." required:""`
	Projects    []string `help:"Projects this key may access; repeat or comma-separate. Use '*' for all." default:"*"`
	Expires     string   `help:"Optional expiry: RFC3339 timestamp or Go duration (e.g. 720h)." default:""`
}

func (c *CreateCmd) Run() error {
	var expires *time.Time
	if c.Expires != "" {
		t, err := parseExpiry(c.Expires)
		if err != nil {
			return err
		}
		expires = &t
	}

	token, keyID, hash, err := apikey.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}

	store := openStore(c.APIKeysFile)
	if err := store.Put(context.Background(), &apikey.APIKey{
		KeyID:    keyID,
		Name:     c.Name,
		Hash:     hash,
		Projects: c.Projects,
		Created:  time.Now().UTC(),
		Expires:  expires,
	}); err != nil {
		return fmt.Errorf("put api key: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Created API key %q (id: %s, projects: %s)\n",
		c.Name, keyID, strings.Join(c.Projects, ","))
	fmt.Fprintf(os.Stdout, "\nToken — store it now, it will not be shown again:\n\n  %s\n\n", token)
	return nil
}

// parseExpiry accepts either a Go duration relative to now or an absolute
// RFC3339 timestamp.
func parseExpiry(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid --expires %q: want an RFC3339 timestamp or a Go duration like 720h", s)
}

// ListCmd prints key metadata. The hash is never printed.
type ListCmd struct {
	APIKeysFile string `help:"Path to API keys TOML file." default:""`
}

func (c *ListCmd) Run() error {
	store := openStore(c.APIKeysFile)
	keys, err := store.List(context.Background())
	if err != nil {
		return fmt.Errorf("list api keys: %w", err)
	}

	if len(keys) == 0 {
		fmt.Fprintln(os.Stdout, "No API keys configured")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY ID\tNAME\tPROJECTS\tCREATED\tEXPIRES\tDISABLED")
	for _, k := range keys {
		expires := "-"
		if k.Expires != nil {
			expires = k.Expires.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%t\n",
			k.KeyID, k.Name, strings.Join(k.Projects, ","),
			k.Created.Format(time.RFC3339), expires, k.Disabled)
	}
	return tw.Flush()
}

// RevokeCmd permanently deletes a key.
type RevokeCmd struct {
	APIKeysFile string `help:"Path to API keys TOML file." default:""`
	KeyID       string `arg:"" help:"ID of the key to delete."`
}

func (c *RevokeCmd) Run() error {
	store := openStore(c.APIKeysFile)
	if err := store.Delete(context.Background(), c.KeyID); err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Revoked API key %s\n", c.KeyID)
	return nil
}

// DisableCmd marks a key disabled, keeping it in the store for audit.
type DisableCmd struct {
	APIKeysFile string `help:"Path to API keys TOML file." default:""`
	KeyID       string `arg:"" help:"ID of the key to disable."`
}

func (c *DisableCmd) Run() error {
	store := openStore(c.APIKeysFile)
	k, err := store.Get(context.Background(), c.KeyID)
	if err != nil {
		return fmt.Errorf("get api key: %w", err)
	}
	k.Disabled = true
	if err := store.Put(context.Background(), k); err != nil {
		return fmt.Errorf("put api key: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Disabled API key %s\n", c.KeyID)
	return nil
}
