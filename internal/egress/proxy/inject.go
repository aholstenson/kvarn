package proxy

import (
	"log/slog"
	"net/http"
	"strings"
)

// SecretInjector enriches an outbound proxied HTTP request with credentials
// for the destination host. Implementations must not block longer than the
// caller's context allows and must not modify req.Body.
type SecretInjector interface {
	Inject(req *http.Request, host string) error
}

// InjectorFunc adapts a plain function to the SecretInjector interface.
type InjectorFunc func(req *http.Request, host string) error

func (f InjectorFunc) Inject(req *http.Request, host string) error {
	return f(req, host)
}

// PlaceholderInjector substitutes per-job placeholder strings in outbound
// request headers with their real secret values. Placeholders are
// unguessable random tokens (kvarn:<32 hex chars>) injected into the VM as
// env-var values; the proxy here is what makes them functional credentials.
//
// Substitution is limited to headers — bodies and URL query parameters are
// untouched. This is a deliberate scope limitation to reduce the chance of
// accidental substitution inside opaque payloads.
type PlaceholderInjector struct {
	// placeholders maps "kvarn:<hex>" -> real secret value.
	placeholders map[string]string
	log          *slog.Logger
}

// NewPlaceholderInjector returns an injector that knows how to substitute
// the given placeholder→value pairs. The map is used directly; callers
// must not mutate it after construction.
func NewPlaceholderInjector(placeholders map[string]string, log *slog.Logger) *PlaceholderInjector {
	if log == nil {
		log = slog.Default()
	}
	return &PlaceholderInjector{placeholders: placeholders, log: log}
}

func (p *PlaceholderInjector) Inject(req *http.Request, host string) error {
	if len(p.placeholders) == 0 {
		return nil
	}
	for name, values := range req.Header {
		for i, v := range values {
			replaced := v
			for placeholder, real := range p.placeholders {
				if strings.Contains(replaced, placeholder) {
					replaced = strings.ReplaceAll(replaced, placeholder, real)
					p.log.Debug("substituted bearer placeholder",
						"host", host, "header", name)
				}
			}
			if replaced != v {
				values[i] = replaced
			}
		}
	}
	return nil
}
