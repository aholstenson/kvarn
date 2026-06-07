package secret

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"fmt"
)

// Ref names a secret a job requires and, for managed secrets, how and where
// the egress proxy should apply it. It mirrors the kvarn.yml `secrets:` entry
// (project.SecretRef) but lives in this package so resolution need not import
// project. Scheme empty defaults to bearer.
type Ref struct {
	Name   string
	Scheme string
	Hosts  []string
}

// Managed is a resolved managed secret: the real value the egress proxy
// substitutes for the per-job placeholder, plus the scheme and host scope that
// govern how and where it is applied. Scheme empty means bearer.
type Managed struct {
	Value  string
	Scheme string
	Hosts  []string
}

// Resolve looks up every named secret against the per-project secret
// store. It returns the env-var map to inject into the VM and the
// placeholder→managed map for the egress proxy. All missing names are
// reported in a single error so the user can fix them in one pass.
//
// Returns nil maps and nil error when refs is empty.
func Resolve(ctx context.Context, store Store, projectName string, refs []Ref) (map[string]string, map[string]Managed, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	if store == nil {
		return nil, nil, fmt.Errorf("kvarn.yml declares secrets but no secret store is configured")
	}

	envSecrets := make(map[string]string, len(refs))
	managed := make(map[string]Managed)
	var missing []string

	for _, ref := range refs {
		sec, err := store.Get(ctx, projectName, ref.Name)
		if err != nil {
			missing = append(missing, ref.Name)
			continue
		}

		switch sec.Type {
		case TypeEnv:
			// A scheme/host scope describes a proxy-applied protocol, but an
			// env secret's value lives inside the VM and is never seen by the
			// proxy. The store type is only known here, so this mismatch can
			// only be caught at resolution time.
			if ref.Scheme != "" || len(ref.Hosts) > 0 {
				return nil, nil, fmt.Errorf("secret %q is type %q but kvarn.yml sets a scheme/hosts; those apply only to %q secrets",
					ref.Name, TypeEnv, TypeManaged)
			}
			envSecrets[ref.Name] = sec.Value
		case TypeManaged:
			placeholder, err := generatePlaceholder()
			if err != nil {
				return nil, nil, fmt.Errorf("generate secret placeholder: %w", err)
			}
			envSecrets[ref.Name] = placeholder
			managed[placeholder] = Managed{
				Value:  sec.Value,
				Scheme: ref.Scheme,
				Hosts:  ref.Hosts,
			}
		default:
			return nil, nil, fmt.Errorf("secret %q has unknown type %q", ref.Name, sec.Type)
		}
	}

	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("missing secrets for project %q: %s",
			projectName, strings.Join(missing, ", "))
	}

	return envSecrets, managed, nil
}

// generatePlaceholder returns an unguessable per-job placeholder of the form
// "kvarn_<32 hex chars>". The prefix lets the egress proxy recognize the
// placeholder unambiguously even when embedded in a longer value (a bearer
// header, a Basic auth blob, or a request body). The "_" delimiter survives
// base64 and percent-encoding, where ":" would not.
func generatePlaceholder() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "kvarn_" + hex.EncodeToString(b), nil
}
