// Package nixbase32 encodes hashes the way Nix names files in a binary
// cache. Nix uses its own base32 alphabet and emits the digits from the least
// significant end, so a standard base32 encoder produces a different string
// for the same bytes.
package nixbase32

// Alphabet is the digit set Nix uses. It omits e, o, t and u so a hash never
// spells an offensive word by accident.
const Alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// EncodedLen returns the number of base32 digits a hash of n bytes encodes to.
// A sha256 hash encodes to 52 digits.
func EncodedLen(n int) int {
	if n == 0 {
		return 0
	}
	return (n*8-1)/5 + 1
}

// Encode returns the Nix base32 form of hash.
func Encode(hash []byte) string {
	n := EncodedLen(len(hash))
	out := make([]byte, 0, n)
	for i := n - 1; i >= 0; i-- {
		bit := uint(i) * 5
		byteIndex := bit / 8
		shift := bit % 8
		c := hash[byteIndex] >> shift
		if byteIndex+1 < uint(len(hash)) {
			c |= hash[byteIndex+1] << (8 - shift)
		}
		out = append(out, Alphabet[c&0x1f])
	}
	return string(out)
}

// IsValid reports whether s is a string of length n made only of digits from
// the Nix alphabet.
func IsValid(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return c != 'e' && c != 'o' && c != 't' && c != 'u'
	}
	return false
}
