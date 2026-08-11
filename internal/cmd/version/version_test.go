package version

import (
	"bytes"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/buildinfo"
)

var _ = Describe("Version", func() {
	Describe("Collect", func() {
		It("reports the linked-in version, image constraint and platform", func() {
			info := Collect()
			Expect(info.Version).To(Equal(buildinfo.Version))
			Expect(info.ImageConstraint).To(Equal(buildinfo.ImageConstraint))
			Expect(info.Go).To(HavePrefix("go"))
			Expect(info.Platform).To(ContainSubstring("/"))
		})
	})

	Describe("render", func() {
		It("leads with the version and lists the build details", func() {
			var buf bytes.Buffer
			Expect(render(&buf, Info{
				Version:         "v1.2.3",
				Revision:        "abcdef123456",
				Go:              "go1.24.0",
				Platform:        "darwin/arm64",
				ImageConstraint: ">=0.2.0 <0.5.0",
			})).To(Succeed())

			out := buf.String()
			Expect(out).To(HavePrefix("kvarn v1.2.3\n"))
			Expect(out).To(ContainSubstring("abcdef123456"))
			Expect(out).To(ContainSubstring("go1.24.0"))
			Expect(out).To(ContainSubstring("darwin/arm64"))
			Expect(out).To(ContainSubstring(">=0.2.0 <0.5.0"))
		})

		It("omits the revision row for a build without a VCS stamp", func() {
			var buf bytes.Buffer
			Expect(render(&buf, Info{Version: "dev", Go: "go1.24.0", Platform: "linux/amd64"})).To(Succeed())
			Expect(buf.String()).NotTo(ContainSubstring("revision"))
		})
	})

	Describe("printJSON", func() {
		It("emits the identity as a JSON object, dropping an absent revision", func() {
			var buf bytes.Buffer
			Expect(printJSON(&buf, Info{
				Version:         "v1.2.3",
				Go:              "go1.24.0",
				Platform:        "darwin/arm64",
				ImageConstraint: ">=0.2.0 <0.5.0",
			})).To(Succeed())

			var got map[string]any
			Expect(json.Unmarshal(buf.Bytes(), &got)).To(Succeed())
			Expect(got).To(HaveKeyWithValue("version", "v1.2.3"))
			Expect(got).To(HaveKeyWithValue("image_constraint", ">=0.2.0 <0.5.0"))
			Expect(got).NotTo(HaveKey("revision"))
		})
	})
})
