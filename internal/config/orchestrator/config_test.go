package orchestrator_test

import (
	"os"
	"path/filepath"

	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Orchestrator config Load", func() {
	It("returns an empty config when the file does not exist", func() {
		cfg, err := orchcfg.Load(filepath.Join(GinkgoT().TempDir(), "missing.toml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(cfg.Scheduler.CPUs).To(BeNil())
		Expect(cfg.Scheduler.Memory).To(BeEmpty())
		Expect(cfg.Scheduler.Disk).To(BeEmpty())
		Expect(cfg.Scheduler.CPUOvercommit).To(BeNil())
	})

	It("parses every scheduler field", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "orchestrator.toml")
		Expect(os.WriteFile(path, []byte(`
[scheduler]
cpus = 8
memory = "32G"
disk = "200G"
cpu_overcommit = 1.5
`), 0644)).To(Succeed())

		cfg, err := orchcfg.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Scheduler.CPUs).NotTo(BeNil())
		Expect(*cfg.Scheduler.CPUs).To(Equal(uint(8)))
		Expect(cfg.Scheduler.Memory).To(Equal("32G"))
		Expect(cfg.Scheduler.Disk).To(Equal("200G"))
		Expect(cfg.Scheduler.CPUOvercommit).NotTo(BeNil())
		Expect(*cfg.Scheduler.CPUOvercommit).To(Equal(1.5))
	})

	It("returns parse errors with the path attached", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "orchestrator.toml")
		Expect(os.WriteFile(path, []byte("this = is not = valid"), 0644)).To(Succeed())

		_, err := orchcfg.Load(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(path))
	})

	It("treats an empty file as all-unset", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "orchestrator.toml")
		Expect(os.WriteFile(path, []byte{}, 0644)).To(Succeed())

		cfg, err := orchcfg.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Scheduler.CPUs).To(BeNil())
		Expect(cfg.Scheduler.CPUOvercommit).To(BeNil())
	})
})
