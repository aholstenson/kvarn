package cache_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

const testChannel = "nixos-25.11"

func goDeps() []project.ResolvedDep {
	return []project.ResolvedDep{{FlakeURI: "github:NixOS/nixpkgs/" + testChannel, Attr: "go"}}
}

func goLookup(attr string) (cache.ToolEntry, bool) {
	if attr == "go" {
		return cache.ToolEntry{
			Bucket:     "go",
			Lockfiles:  []string{"**/go.sum", "**/go.mod"},
			CachePaths: []string{"/home/kvarn/go"},
		}, true
	}
	return cache.ToolEntry{}, false
}

func bucketInputKey(layers []cache.Layer, bucket string) (string, bool) {
	for _, l := range layers {
		if l.Key.Bucket == bucket {
			return l.Key.InputKey, true
		}
	}
	return "", false
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeFile(dir, rel, content string) {
	GinkgoHelper()
	p := filepath.Join(dir, rel)
	Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
	Expect(os.WriteFile(p, []byte(content), 0o644)).To(Succeed())
}

var _ = Describe("DeriveLayers", func() {
	It("produces the same InputKey for identical lockfile content (cross-branch sharing)", func() {
		dirA := GinkgoT().TempDir()
		dirB := GinkgoT().TempDir()
		writeFile(dirA, "go.mod", "module x\n")
		writeFile(dirA, "go.sum", "deadbeef\n")
		writeFile(dirB, "go.mod", "module x\n")
		writeFile(dirB, "go.sum", "deadbeef\n")

		la, err := cache.DeriveLayers(dirA, goDeps(), goLookup, project.Cache{}, "proj", "")
		Expect(err).NotTo(HaveOccurred())
		lb, err := cache.DeriveLayers(dirB, goDeps(), goLookup, project.Cache{}, "proj", "")
		Expect(err).NotTo(HaveOccurred())

		ka, ok := bucketInputKey(la, "go")
		Expect(ok).To(BeTrue())
		kb, _ := bucketInputKey(lb, "go")
		Expect(ka).To(Equal(kb))
	})

	It("produces a different InputKey when lockfile content diverges", func() {
		dirA := GinkgoT().TempDir()
		dirB := GinkgoT().TempDir()
		writeFile(dirA, "go.mod", "module x\n")
		writeFile(dirA, "go.sum", "aaaa\n")
		writeFile(dirB, "go.mod", "module x\n")
		writeFile(dirB, "go.sum", "bbbb\n")

		la, _ := cache.DeriveLayers(dirA, goDeps(), goLookup, project.Cache{}, "proj", "")
		lb, _ := cache.DeriveLayers(dirB, goDeps(), goLookup, project.Cache{}, "proj", "")
		ka, _ := bucketInputKey(la, "go")
		kb, _ := bucketInputKey(lb, "go")
		Expect(ka).NotTo(Equal(kb))
	})

	It("is monorepo-aware: lockfile location affects the key", func() {
		dirA := GinkgoT().TempDir()
		dirB := GinkgoT().TempDir()
		// Same two contents, swapped between modules.
		writeFile(dirA, "mod-a/go.sum", "one\n")
		writeFile(dirA, "mod-b/go.sum", "two\n")
		writeFile(dirB, "mod-a/go.sum", "two\n")
		writeFile(dirB, "mod-b/go.sum", "one\n")

		la, _ := cache.DeriveLayers(dirA, goDeps(), goLookup, project.Cache{}, "proj", "")
		lb, _ := cache.DeriveLayers(dirB, goDeps(), goLookup, project.Cache{}, "proj", "")
		ka, _ := bucketInputKey(la, "go")
		kb, _ := bucketInputKey(lb, "go")
		Expect(ka).NotTo(Equal(kb))
	})

	It("ignores lockfiles under vendored/build directories", func() {
		clean := GinkgoT().TempDir()
		polluted := GinkgoT().TempDir()
		writeFile(clean, "go.sum", "root\n")
		writeFile(polluted, "go.sum", "root\n")
		writeFile(polluted, "node_modules/dep/go.sum", "junk\n")

		lc, _ := cache.DeriveLayers(clean, goDeps(), goLookup, project.Cache{}, "proj", "")
		lp, _ := cache.DeriveLayers(polluted, goDeps(), goLookup, project.Cache{}, "proj", "")
		kc, _ := bucketInputKey(lc, "go")
		kp, _ := bucketInputKey(lp, "go")
		Expect(kc).To(Equal(kp))
	})

	It("uses a degraded channel-only key when no lockfile is present", func() {
		dir := GinkgoT().TempDir() // no go.mod/go.sum
		layers, err := cache.DeriveLayers(dir, goDeps(), goLookup, project.Cache{}, "proj", "")
		Expect(err).NotTo(HaveOccurred())
		key, ok := bucketInputKey(layers, "go")
		Expect(ok).To(BeTrue())
		Expect(key).To(Equal(hexSum("go-nolock||" + testChannel)))
	})

	It("folds the channel into the key", func() {
		dir := GinkgoT().TempDir()
		writeFile(dir, "go.sum", "same\n")

		base, _ := cache.DeriveLayers(dir, goDeps(), goLookup, project.Cache{}, "proj", "")
		other := []project.ResolvedDep{{FlakeURI: "github:NixOS/nixpkgs/nixos-24.05", Attr: "go"}}
		bumped, _ := cache.DeriveLayers(dir, other, goLookup, project.Cache{}, "proj", "")

		kb, _ := bucketInputKey(base, "go")
		ko, _ := bucketInputKey(bumped, "go")
		Expect(kb).NotTo(Equal(ko))
	})

	It("emits a nix-eval layer when nixpkgs deps are present", func() {
		dir := GinkgoT().TempDir()
		layers, _ := cache.DeriveLayers(dir, goDeps(), goLookup, project.Cache{}, "proj", "")
		_, ok := bucketInputKey(layers, "nix-eval")
		Expect(ok).To(BeTrue())
	})

	It("derives user entries: manual key, lockfile-keyed, and unkeyed", func() {
		dir := GinkgoT().TempDir()
		writeFile(dir, "deps.lock", "v1\n")
		uc := project.Cache{
			Paths: []string{"/opt/plain"},
			Entries: []project.CacheEntry{
				{Path: "/opt/manual", Key: "fixed-1", Bucket: "manualbucket"},
				{Path: "/opt/keyed", Lockfiles: []string{"**/deps.lock"}},
			},
		}
		layers, err := cache.DeriveLayers(dir, nil, goLookup, uc, "proj", "")
		Expect(err).NotTo(HaveOccurred())

		manual, ok := bucketInputKey(layers, "manualbucket")
		Expect(ok).To(BeTrue())
		Expect(manual).To(Equal(hexSum("manual||fixed-1")))

		// Plain path produces a user:<flatpath> bucket.
		_, ok = bucketInputKey(layers, "user:opt_plain")
		Expect(ok).To(BeTrue())
	})
})
