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
max_vm_lifetime = "4h"
`), 0644)).To(Succeed())

		cfg, err := orchcfg.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Scheduler.CPUs).NotTo(BeNil())
		Expect(*cfg.Scheduler.CPUs).To(Equal(uint(8)))
		Expect(cfg.Scheduler.Memory).To(Equal("32G"))
		Expect(cfg.Scheduler.Disk).To(Equal("200G"))
		Expect(cfg.Scheduler.CPUOvercommit).NotTo(BeNil())
		Expect(*cfg.Scheduler.CPUOvercommit).To(Equal(1.5))
		Expect(cfg.Scheduler.MaxVMLifetime).To(Equal("4h"))
	})

	It("parses every repos field", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "orchestrator.toml")
		Expect(os.WriteFile(path, []byte(`
[repos]
enabled = true
dir = "/srv/kvarn/repos"
prefetch = false
prefetch_interval = "10m"
mirror_depth = 50
branch_retention = "168h"
global_bytes = "20G"
`), 0644)).To(Succeed())

		cfg, err := orchcfg.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Repos.Enabled).NotTo(BeNil())
		Expect(*cfg.Repos.Enabled).To(BeTrue())
		Expect(cfg.Repos.Dir).To(Equal("/srv/kvarn/repos"))
		Expect(cfg.Repos.Prefetch).NotTo(BeNil())
		Expect(*cfg.Repos.Prefetch).To(BeFalse())
		Expect(cfg.Repos.PrefetchInterval).To(Equal("10m"))
		Expect(cfg.Repos.MirrorDepth).NotTo(BeNil())
		Expect(*cfg.Repos.MirrorDepth).To(Equal(50))
		Expect(cfg.Repos.BranchRetention).To(Equal("168h"))
		Expect(cfg.Repos.GlobalBytes).To(Equal("20G"))
	})

	It("leaves repos unset when the table is absent", func() {
		cfg, err := orchcfg.Load(filepath.Join(GinkgoT().TempDir(), "missing.toml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Repos.Enabled).To(BeNil())
		Expect(cfg.Repos.Prefetch).To(BeNil())
		Expect(cfg.Repos.MirrorDepth).To(BeNil())
		Expect(cfg.Repos.Dir).To(BeEmpty())
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
