package apikey_test

import (
	"github.com/aholstenson/kvarn/internal/config/apikey"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Capability", func() {
	Describe("ParseCapability", func() {
		It("accepts a known capability", func() {
			c, err := apikey.ParseCapability("host")
			Expect(err).NotTo(HaveOccurred())
			Expect(c).To(Equal(apikey.CapabilityHost))
		})

		DescribeTable("rejects anything else",
			func(name string) {
				_, err := apikey.ParseCapability(name)
				Expect(err).To(HaveOccurred())
			},
			Entry("empty", ""),
			Entry("misspelled", "hosts"),
			Entry("wrong case", "HOST"),
			// A wildcard would hand a key created today whatever authority is
			// defined tomorrow, which is the one axis where that is wrong.
			Entry("wildcard", "*"),
		)
	})

	Describe("APIKey.HasCapability", func() {
		It("reports a granted capability", func() {
			k := &apikey.APIKey{Capabilities: []apikey.Capability{apikey.CapabilityHost}}
			Expect(k.HasCapability(apikey.CapabilityHost)).To(BeTrue())
		})

		It("denies a key with no capabilities", func() {
			Expect((&apikey.APIKey{}).HasCapability(apikey.CapabilityHost)).To(BeFalse())
		})

		// Project scope and capabilities are separate axes; a key that may
		// reach every project has said nothing about speaking for the host.
		It("is not implied by a wildcard project scope", func() {
			k := &apikey.APIKey{Projects: []string{apikey.Wildcard}}
			Expect(k.AllowsProject("anything")).To(BeTrue())
			Expect(k.HasCapability(apikey.CapabilityHost)).To(BeFalse())
		})
	})

	Describe("AllCapabilities", func() {
		It("returns every defined capability", func() {
			Expect(apikey.AllCapabilities()).To(ContainElement(apikey.CapabilityHost))
		})

		It("returns a copy the caller cannot use to mutate the package state", func() {
			got := apikey.AllCapabilities()
			got[0] = "tampered"
			Expect(apikey.AllCapabilities()).To(ContainElement(apikey.CapabilityHost))
		})
	})
})
