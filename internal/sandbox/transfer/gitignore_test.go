package transfer_test

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
)

var _ = Describe("GitIgnoreFilter", func() {
	var tmpDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "gitignore-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("returns nil for a non-git directory", func() {
		filter, err := transfer.GitIgnoreFilter(tmpDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(filter).To(BeNil())
	})

	Context("in a git repository", func() {
		BeforeEach(func() {
			cmd := exec.Command("git", "init", tmpDir)
			cmd.Stdout = GinkgoWriter
			cmd.Stderr = GinkgoWriter
			Expect(cmd.Run()).To(Succeed())

			// Configure git user for commits.
			gitConfig(tmpDir, "user.email", "test@test.com")
			gitConfig(tmpDir, "user.name", "Test")
		})

		It("does not skip the root directory", func() {
			Expect(os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0644)).To(Succeed())
			gitAdd(tmpDir, "file.txt")

			filter, err := transfer.GitIgnoreFilter(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(filter).NotTo(BeNil())

			Expect(filter(".", true)).To(BeFalse())
		})

		It("includes .git directory", func() {
			Expect(os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0644)).To(Succeed())
			gitAdd(tmpDir, "file.txt")

			filter, err := transfer.GitIgnoreFilter(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(filter).NotTo(BeNil())

			Expect(filter(".git", true)).To(BeFalse())
			Expect(filter(filepath.Join(".git", "HEAD"), false)).To(BeFalse())
		})

		It("skips gitignored files", func() {
			Expect(os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\nbuild/\n"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "app.go"), []byte("package main"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("log"), 0644)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "build", "output"), []byte("bin"), 0644)).To(Succeed())
			gitAdd(tmpDir, ".gitignore", "app.go")

			filter, err := transfer.GitIgnoreFilter(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(filter).NotTo(BeNil())

			Expect(filter("app.go", false)).To(BeFalse())
			Expect(filter(".gitignore", false)).To(BeFalse())
			Expect(filter("debug.log", false)).To(BeTrue())
			Expect(filter("build", true)).To(BeTrue())
			Expect(filter(filepath.Join("build", "output"), false)).To(BeTrue())
		})

		It("includes untracked non-ignored files", func() {
			Expect(os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "tracked.go"), []byte("package main"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "untracked.go"), []byte("package main"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("log"), 0644)).To(Succeed())
			gitAdd(tmpDir, ".gitignore", "tracked.go")

			filter, err := transfer.GitIgnoreFilter(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(filter).NotTo(BeNil())

			Expect(filter("tracked.go", false)).To(BeFalse())
			Expect(filter("untracked.go", false)).To(BeFalse())
			Expect(filter("debug.log", false)).To(BeTrue())
		})

		It("handles nested .gitignore", func() {
			subDir := filepath.Join(tmpDir, "sub")
			Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte("*.tmp\n"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(subDir, "code.go"), []byte("package sub"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(subDir, "data.tmp"), []byte("tmp"), 0644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(subDir, "app.log"), []byte("log"), 0644)).To(Succeed())
			gitAdd(tmpDir, ".gitignore", filepath.Join("sub", ".gitignore"), filepath.Join("sub", "code.go"))

			filter, err := transfer.GitIgnoreFilter(tmpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(filter).NotTo(BeNil())

			Expect(filter(filepath.Join("sub", "code.go"), false)).To(BeFalse())
			Expect(filter(filepath.Join("sub", "data.tmp"), false)).To(BeTrue())
			Expect(filter(filepath.Join("sub", "app.log"), false)).To(BeTrue())
		})
	})
})

func gitAdd(dir string, files ...string) {
	args := append([]string{"-C", dir, "add"}, files...)
	cmd := exec.Command("git", args...)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	ExpectWithOffset(1, cmd.Run()).To(Succeed())
}

func gitConfig(dir string, key, value string) {
	cmd := exec.Command("git", "-C", dir, "config", key, value)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	ExpectWithOffset(1, cmd.Run()).To(Succeed())
}
