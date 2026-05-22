package secrets

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aholstenson/kvarn/internal/config/secret"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
)

// Cmd is the parent command for `kvarn secrets <subcommand>`.
type Cmd struct {
	Set    SetCmd    `cmd:"" help:"Set a secret value for a project."`
	List   ListCmd   `cmd:"" help:"List secret names and types for a project."`
	Remove RemoveCmd `cmd:"" help:"Remove a secret from a project."`
}

func openStore(path string) secret.Store {
	return secrettoml.OpenDefault(path)
}

// SetCmd writes a secret. The value is read from --value or stdin so it
// never appears in shell history when stdin is used.
type SetCmd struct {
	SecretsFile string `help:"Path to per-project secrets TOML file." default:""`
	Project     string `arg:"" help:"Project name."`
	Name        string `arg:"" help:"Secret name (must be a valid POSIX env-var name)."`
	Type        string `help:"Secret type: env or bearer." default:"env"`
	Value       string `help:"Secret value. If omitted, the value is read from stdin."`
}

func (c *SetCmd) Run() error {
	value := c.Value
	if value == "" {
		fmt.Fprint(os.Stderr, "Reading secret value from stdin (end with EOF)...\n")
		data, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return fmt.Errorf("read value from stdin: %w", err)
		}
		value = strings.TrimRight(string(data), "\r\n")
	}

	store := openStore(c.SecretsFile)
	if err := store.Put(context.Background(), &secret.Secret{
		Project: c.Project,
		Name:    c.Name,
		Type:    c.Type,
		Value:   value,
	}); err != nil {
		return fmt.Errorf("put secret: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Set secret %s/%s (type=%s)\n", c.Project, c.Name, c.Type)
	return nil
}

// ListCmd prints the names and types of secrets for a project. Values are
// never printed.
type ListCmd struct {
	SecretsFile string `help:"Path to per-project secrets TOML file." default:""`
	Project     string `arg:"" help:"Project name."`
}

func (c *ListCmd) Run() error {
	store := openStore(c.SecretsFile)
	secrets, err := store.List(context.Background(), c.Project)
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}

	if len(secrets) == 0 {
		fmt.Fprintf(os.Stdout, "No secrets configured for project %q\n", c.Project)
		return nil
	}

	for _, s := range secrets {
		fmt.Fprintf(os.Stdout, "%s\t%s\n", s.Name, s.Type)
	}
	return nil
}

// RemoveCmd deletes a secret.
type RemoveCmd struct {
	SecretsFile string `help:"Path to per-project secrets TOML file." default:""`
	Project     string `arg:"" help:"Project name."`
	Name        string `arg:"" help:"Secret name."`
}

func (c *RemoveCmd) Run() error {
	store := openStore(c.SecretsFile)
	if err := store.Delete(context.Background(), c.Project, c.Name); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed secret %s/%s\n", c.Project, c.Name)
	return nil
}
