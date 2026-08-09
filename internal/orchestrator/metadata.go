package orchestrator

import (
	"fmt"
	"strings"
)

// Bounds on the annotations a submission may carry. Metadata lands on every
// session row and is echoed by every listing, so an unbounded map would let one
// caller inflate the store and every response that reads from it. The limits
// are generous for what callers actually attach — ticket ids, request ids, a
// channel name — and small enough that the worst case stays bounded.
const (
	maxMetadataEntries  = 32
	maxMetadataKeyLen   = 64
	maxMetadataValueLen = 512
	maxMetadataBytes    = 8 * 1024
)

// metadataReservedPrefix is held back so kvarn can stamp its own entries later
// without colliding with a caller's. Refusing it now is what makes that
// possible: a key nobody may write today is one that can be given a meaning
// tomorrow without breaking anyone.
const metadataReservedPrefix = "kvarn."

// validateMetadata checks a submission's annotations against the bounds above.
//
// Everything here is refused rather than repaired. A truncated value or a
// dropped pair would leave a record that reads as complete and is not, which
// defeats the one thing metadata is for; a caller that sent too much would
// rather learn it than find out months later from a filter that matches
// nothing.
func validateMetadata(md map[string]string) error {
	if len(md) == 0 {
		return nil
	}
	if len(md) > maxMetadataEntries {
		return fmt.Errorf("metadata has %d entries; the limit is %d", len(md), maxMetadataEntries)
	}
	total := 0
	for k, v := range md {
		if err := validateMetadataKey(k); err != nil {
			return err
		}
		if len(v) > maxMetadataValueLen {
			return fmt.Errorf("metadata value for %q is %d bytes; the limit is %d", k, len(v), maxMetadataValueLen)
		}
		total += len(k) + len(v)
	}
	if total > maxMetadataBytes {
		return fmt.Errorf("metadata is %d bytes; the limit is %d", total, maxMetadataBytes)
	}
	return nil
}

// validateMetadataKey holds keys to a shape that survives every place a key is
// written down — a CLI `--meta key=value` flag, a log attribute, a JSON object
// — so a key that round-trips through the API is one a caller can still search
// for afterwards.
func validateMetadataKey(k string) error {
	if k == "" {
		return fmt.Errorf("metadata has an empty key")
	}
	if len(k) > maxMetadataKeyLen {
		return fmt.Errorf("metadata key %q is %d bytes; the limit is %d", k, len(k), maxMetadataKeyLen)
	}
	if strings.HasPrefix(k, metadataReservedPrefix) {
		return fmt.Errorf("metadata key %q uses the reserved %q prefix", k, metadataReservedPrefix)
	}
	for i := 0; i < len(k); i++ {
		if !isMetadataKeyByte(k[i]) {
			return fmt.Errorf("metadata key %q contains %q; keys may use letters, digits and -_./", k, k[i])
		}
	}
	if isMetadataSeparator(k[0]) || isMetadataSeparator(k[len(k)-1]) {
		return fmt.Errorf("metadata key %q must start and end with a letter or digit", k)
	}
	return nil
}

func isMetadataKeyByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return isMetadataSeparator(c)
	}
}

func isMetadataSeparator(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c == '/'
}
