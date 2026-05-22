package apikey_test

import (
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Token", func() {
	Describe("GenerateToken / ParseToken / HashSecret", func() {
		It("round-trips a freshly generated token", func() {
			token, keyID, hash, err := apikey.GenerateToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).To(HavePrefix("kvarn_"))

			parsedID, secret, ok := apikey.ParseToken(token)
			Expect(ok).To(BeTrue())
			Expect(parsedID).To(Equal(keyID))
			Expect(apikey.HashSecret(secret)).To(Equal(hash))
		})

		It("produces a unique token each call", func() {
			t1, _, _, err := apikey.GenerateToken()
			Expect(err).NotTo(HaveOccurred())
			t2, _, _, err := apikey.GenerateToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(t1).NotTo(Equal(t2))
		})

		It("uses an underscore-free encoding for the components", func() {
			_, keyID, _, err := apikey.GenerateToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Contains(keyID, "_")).To(BeFalse())
		})

		DescribeTable("rejects malformed tokens",
			func(token string) {
				_, _, ok := apikey.ParseToken(token)
				Expect(ok).To(BeFalse())
			},
			Entry("empty", ""),
			Entry("wrong prefix", "github_abc_def"),
			Entry("too few parts", "kvarn_abc"),
			Entry("too many parts", "kvarn_abc_def_ghi"),
			Entry("empty keyid", "kvarn__def"),
			Entry("empty secret", "kvarn_abc_"),
		)
	})

	Describe("APIKey.AllowsProject", func() {
		It("allows any project for the wildcard", func() {
			k := &apikey.APIKey{Projects: []string{"*"}}
			Expect(k.AllowsProject("anything")).To(BeTrue())
			Expect(k.AllowsProject("other")).To(BeTrue())
		})

		It("allows only listed projects", func() {
			k := &apikey.APIKey{Projects: []string{"a", "b"}}
			Expect(k.AllowsProject("a")).To(BeTrue())
			Expect(k.AllowsProject("b")).To(BeTrue())
			Expect(k.AllowsProject("c")).To(BeFalse())
		})

		It("denies everything for an empty project list", func() {
			k := &apikey.APIKey{}
			Expect(k.AllowsProject("a")).To(BeFalse())
		})
	})

	Describe("APIKey.Expired", func() {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		It("never expires when Expires is nil", func() {
			k := &apikey.APIKey{}
			Expect(k.Expired(now)).To(BeFalse())
		})

		It("is expired when the deadline is in the past", func() {
			past := now.Add(-time.Hour)
			k := &apikey.APIKey{Expires: &past}
			Expect(k.Expired(now)).To(BeTrue())
		})

		It("is not expired when the deadline is in the future", func() {
			future := now.Add(time.Hour)
			k := &apikey.APIKey{Expires: &future}
			Expect(k.Expired(now)).To(BeFalse())
		})
	})
})
