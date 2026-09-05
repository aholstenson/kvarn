package nixbase32_test

import (
	"crypto/sha256"

	"github.com/aholstenson/kvarn/internal/nixcache/nixbase32"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Encode", func() {
	It("matches what `nix hash to-base32` prints for the sha256 of an empty input", func() {
		sum := sha256.Sum256(nil)
		Expect(nixbase32.Encode(sum[:])).To(Equal("0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73"))
	})

	It("encodes a sha256 hash to 52 digits", func() {
		sum := sha256.Sum256([]byte("kvarn"))
		Expect(nixbase32.EncodedLen(len(sum))).To(Equal(52))
		Expect(nixbase32.Encode(sum[:])).To(HaveLen(52))
	})

	It("uses only digits from the Nix alphabet", func() {
		sum := sha256.Sum256([]byte("alphabet"))
		s := nixbase32.Encode(sum[:])
		for i := 0; i < len(s); i++ {
			Expect(nixbase32.Alphabet).To(ContainSubstring(string(s[i])))
		}
	})
})

var _ = Describe("IsValid", func() {
	It("accepts a store path hash", func() {
		Expect(nixbase32.IsValid("0mdqa9w1p6cmli6976v4wi0sw9r4p5pr", 32)).To(BeTrue())
	})

	It("rejects the wrong length", func() {
		Expect(nixbase32.IsValid("0mdqa9w1", 32)).To(BeFalse())
	})

	It("rejects digits outside the alphabet", func() {
		Expect(nixbase32.IsValid("emdqa9w1p6cmli6976v4wi0sw9r4p5pr", 32)).To(BeFalse())
		Expect(nixbase32.IsValid("Amdqa9w1p6cmli6976v4wi0sw9r4p5pr", 32)).To(BeFalse())
		Expect(nixbase32.IsValid("../qa9w1p6cmli6976v4wi0sw9r4p5pr", 32)).To(BeFalse())
	})
})
