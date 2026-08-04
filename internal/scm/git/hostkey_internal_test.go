package git

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var _ = Describe("Pinned host keys", func() {
	// The security property kvarn has always had is "these three forges are
	// verified against fingerprints we checked against their published docs".
	// Moving to OpenSSH means shipping the full keys instead, so these specs
	// hold the key blobs to the fingerprints rather than trusting whoever
	// pasted them in.
	It("hashes to the published fingerprints", func() {
		byHost := map[string][]string{}
		for _, line := range strings.Split(embeddedHostKeys, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			_, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line + "\n"))
			Expect(err).NotTo(HaveOccurred(), "unparseable known_hosts line: %s", line)
			Expect(hosts).To(HaveLen(1))

			host := hosts[0]
			fp := ssh.FingerprintSHA256(key)
			Expect(pinnedHostFingerprints).To(HaveKey(host))
			Expect(pinnedHostFingerprints[host]).To(ContainElement(fp),
				"key for %s (%s) does not match any published fingerprint", host, key.Type())
			byHost[host] = append(byHost[host], fp)
		}

		// Neither table may carry an entry the other lacks: a pinned
		// fingerprint with no key would silently stop being enforced.
		Expect(byHost).To(HaveLen(len(pinnedHostFingerprints)))
		for host, want := range pinnedHostFingerprints {
			Expect(byHost[host]).To(ConsistOf(want))
		}
	})

	It("enforces the version floor", func() {
		major, minor, ok := parseGitVersion("git version 2.43.0")
		Expect(ok).To(BeTrue())
		Expect(major).To(Equal(2))
		Expect(minor).To(Equal(43))

		// Apple and other vendors append a build suffix.
		major, minor, ok = parseGitVersion("git version 2.39.5 (Apple Git-154)")
		Expect(ok).To(BeTrue())
		Expect(major).To(Equal(2))
		Expect(minor).To(Equal(39))

		_, _, ok = parseGitVersion("something else entirely")
		Expect(ok).To(BeFalse())
	})

	It("writes a known_hosts file OpenSSH can parse", func() {
		dir := GinkgoT().TempDir()
		path, err := writeKnownHosts(dir)
		Expect(err).NotTo(HaveOccurred())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))

		callback, err := knownhosts.New(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(callback).NotTo(BeNil())
	})
})
