package apikey

import "fmt"

// Capability names an action that has no project to scope it to.
//
// Project scope and capabilities are separate axes because they answer
// different questions. A key's project list says which work it may reach;
// a capability says whether it may act on the orchestrator itself. Reading
// authority off the project wildcard would conflate the two — `*` exists so
// one key can drive every project, which is what a CI bot needs and not at all
// the same claim as being the host's operator.
type Capability string

// CapabilityHost covers actions taken against the orchestrator rather than
// against any project: changing whether it accepts work, and sweeps whose
// filter is the host rather than a project.
const CapabilityHost Capability = "host"

// allCapabilities is every capability that exists, in the order help output
// and `key list` should show them.
var allCapabilities = []Capability{CapabilityHost}

// AllCapabilities returns every defined capability. It is what the host-local
// control socket grants, since a caller who already owns the host's filesystem
// cannot be meaningfully restricted by kvarn.
func AllCapabilities() []Capability {
	out := make([]Capability, len(allCapabilities))
	copy(out, allCapabilities)
	return out
}

// ParseCapability validates a capability name from config or a command line.
// Unknown names are rejected rather than ignored: a key whose capability was
// misspelled would otherwise look granted in the file and be denied at the
// only moment anyone finds out, which is when an operator needs it to work.
func ParseCapability(s string) (Capability, error) {
	for _, c := range allCapabilities {
		if Capability(s) == c {
			return c, nil
		}
	}
	return "", fmt.Errorf("unknown capability %q (known: %s)", s, JoinCapabilities(allCapabilities))
}

// JoinCapabilities renders capabilities as a comma-separated list for display.
func JoinCapabilities(caps []Capability) string {
	out := ""
	for i, c := range caps {
		if i > 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

// HasCapability reports whether the key holds c. There is deliberately no
// wildcard: a key created today would otherwise silently gain whatever
// authority is defined tomorrow, and authority is the one axis where that is
// the wrong default.
func (k *APIKey) HasCapability(c Capability) bool {
	for _, have := range k.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}
