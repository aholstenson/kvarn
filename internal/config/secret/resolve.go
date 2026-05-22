package secret

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"fmt"
)

// Resolve looks up every named secret against the per-project secret
// store. It returns the env-var map to inject into the VM and the
// placeholder→value map for the egress proxy. All missing names are
// reported in a single error so the user can fix them in one pass.
//
// Returns nil maps and nil error when names is empty.
func Resolve(ctx context.Context, store Store, projectName string, names []string) (map[string]string, map[string]string, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	if store == nil {
		return nil, nil, fmt.Errorf("kvarn.yml declares secrets but no secret store is configured")
	}

	envSecrets := make(map[string]string, len(names))
	bearerPlaceholders := make(map[string]string)
	var missing []string

	for _, name := range names {
		sec, err := store.Get(ctx, projectName, name)
		if err != nil {
			missing = append(missing, name)
			continue
		}

		switch sec.Type {
		case TypeEnv:
			envSecrets[name] = sec.Value
		case TypeBearer:
			placeholder, err := generateBearerPlaceholder()
			if err != nil {
				return nil, nil, fmt.Errorf("generate bearer placeholder: %w", err)
			}
			envSecrets[name] = placeholder
			bearerPlaceholders[placeholder] = sec.Value
		default:
			return nil, nil, fmt.Errorf("secret %q has unknown type %q", name, sec.Type)
		}
	}

	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("missing secrets for project %q: %s",
			projectName, strings.Join(missing, ", "))
	}

	return envSecrets, bearerPlaceholders, nil
}

// generateBearerPlaceholder returns an unguessable per-job placeholder of
// the form "kvarn:<32 hex chars>". The prefix lets the egress proxy
// recognize the placeholder unambiguously even when embedded in a longer
// header like `Authorization: Bearer <placeholder>`.
func generateBearerPlaceholder() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "kvarn:" + hex.EncodeToString(b), nil
}
