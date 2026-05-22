package project_test

import (
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Load", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("returns nil when no config file exists", func() {
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).To(BeNil())
	})

	It("finds kvarn.yml", func() {
		writeYAML(dir, "kvarn.yml", validConfig())
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
	})

	It("finds .kvarn.yaml as last priority", func() {
		writeYAML(dir, ".kvarn.yaml", validConfig())
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
	})

	It("prioritizes kvarn.yml over kvarn.yaml", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: from-yml
      run: echo yml
`)
		writeYAML(dir, "kvarn.yaml", `
setup:
  steps:
    - name: from-yaml
      run: echo yaml
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Setup.Steps[0].Name).To(Equal("from-yml"))
	})

	It("prioritizes kvarn.yaml over .kvarn.yml", func() {
		writeYAML(dir, "kvarn.yaml", `
setup:
  steps:
    - name: from-yaml
      run: echo yaml
`)
		writeYAML(dir, ".kvarn.yml", `
setup:
  steps:
    - name: from-dot-yml
      run: echo dot-yml
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Setup.Steps[0].Name).To(Equal("from-yaml"))
	})

	It("returns error on malformed YAML", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: [invalid
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
	})

	It("returns error when step has empty name", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: ""
      run: echo hello
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty name"))
	})

	It("returns error when step has empty run", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: ""
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty run"))
	})

	It("returns error when step has absolute working_dir", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: npm install
      working_dir: /absolute/path
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("absolute working_dir"))
	})

	It("parses all fields correctly in a full round-trip", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install dependencies
      run: npm install
      working_dir: frontend
  health_checks:
    - name: DB reachable
      run: pg_isready -h localhost

validation:
  required:
    - name: Backend tests
      run: php bin/phpunit --testdox
      working_dir: backend
      paths:
        - "backend/**/*.php"
        - "backend/**/*.yaml"
  advisory:
    - name: Lint
      run: npm run lint
      paths:
        - "frontend/**"
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())

		Expect(cfg.Setup.Steps).To(HaveLen(1))
		Expect(cfg.Setup.Steps[0].Name).To(Equal("Install dependencies"))
		Expect(cfg.Setup.Steps[0].Run).To(Equal("npm install"))
		Expect(cfg.Setup.Steps[0].WorkingDir).To(Equal("frontend"))

		Expect(cfg.Setup.HealthChecks).To(HaveLen(1))
		Expect(cfg.Setup.HealthChecks[0].Name).To(Equal("DB reachable"))

		Expect(cfg.Validation.Required).To(HaveLen(1))
		Expect(cfg.Validation.Required[0].Name).To(Equal("Backend tests"))
		Expect(cfg.Validation.Required[0].WorkingDir).To(Equal("backend"))
		Expect(cfg.Validation.Required[0].Paths).To(Equal([]string{"backend/**/*.php", "backend/**/*.yaml"}))

		Expect(cfg.Validation.Advisory).To(HaveLen(1))
		Expect(cfg.Validation.Advisory[0].Name).To(Equal("Lint"))
		Expect(cfg.Validation.Advisory[0].Paths).To(Equal([]string{"frontend/**"}))
	})
})

var _ = Describe("Step retry", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("parses retry field on a setup step", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: npm install
      retry: 3
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Setup.Steps[0].Retry).To(Equal(uint(3)))
	})

	It("accepts retry: 0 (no-op, same as omitting the field)", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: npm install
      retry: 0
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Setup.Steps[0].Retry).To(Equal(uint(0)))
	})

	It("accepts retry at the maximum of 10", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: npm install
      retry: 10
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Setup.Steps[0].Retry).To(Equal(uint(10)))
	})

	It("rejects retry above the maximum", func() {
		writeYAML(dir, "kvarn.yml", `
setup:
  steps:
    - name: Install deps
      run: npm install
      retry: 11
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exceeds maximum"))
	})
})
var _ = Describe("VM disk size", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("parses disk size in G", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "8G"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DiskSizeBytes()).To(Equal(int64(8 * 1024 * 1024 * 1024)))
	})

	It("parses disk size in GiB", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "8GiB"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DiskSizeBytes()).To(Equal(int64(8 * 1024 * 1024 * 1024)))
	})

	It("parses disk size in M", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "8192M"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DiskSizeBytes()).To(Equal(int64(8192 * 1024 * 1024)))
	})

	It("returns 0 when vm.disk is not set", func() {
		writeYAML(dir, "kvarn.yml", validConfig())
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DiskSizeBytes()).To(Equal(int64(0)))
	})

	It("rejects disk size below minimum (4G)", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "2G"
setup:
  steps:
    - name: Install
      run: npm install
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("below minimum"))
	})

	It("rejects disk size with invalid suffix", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "4T"
setup:
  steps:
    - name: Install
      run: npm install
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported suffix"))
	})

	It("rejects non-numeric disk size", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  disk: "bigG"
setup:
  steps:
    - name: Install
      run: npm install
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("VM cpu and memory", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("parses cpus", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  cpus: 4
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.CPUs()).To(Equal(uint(4)))
	})

	It("returns 0 when cpus is not set", func() {
		writeYAML(dir, "kvarn.yml", validConfig())
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.CPUs()).To(Equal(uint(0)))
	})

	It("parses memory in G", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  memory: "4G"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MemoryBytes()).To(Equal(uint64(4 * 1024 * 1024 * 1024)))
	})

	It("parses memory in M", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  memory: "4096M"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MemoryBytes()).To(Equal(uint64(4096 * 1024 * 1024)))
	})

	It("parses memory in GiB", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  memory: "4GiB"
setup:
  steps:
    - name: Install
      run: npm install
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MemoryBytes()).To(Equal(uint64(4 * 1024 * 1024 * 1024)))
	})

	It("returns 0 when memory is not set", func() {
		writeYAML(dir, "kvarn.yml", validConfig())
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.MemoryBytes()).To(Equal(uint64(0)))
	})

	It("rejects memory below minimum (512M)", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  memory: "256M"
setup:
  steps:
    - name: Install
      run: npm install
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("below minimum"))
	})

	It("rejects memory with invalid suffix", func() {
		writeYAML(dir, "kvarn.yml", `
vm:
  memory: "4T"
setup:
  steps:
    - name: Install
      run: npm install
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported suffix"))
	})
})

var _ = Describe("Image and Dependencies", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("parses image field", func() {
		writeYAML(dir, "kvarn.yml", `
image: node:20
setup:
  steps:
    - name: Build
      run: npm run build
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Image).To(Equal("node:20"))
	})

	It("parses dependencies field with multiple sources", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  nixpkgs:
    - nodejs
    - go
  nixpkgs/nixos-unstable:
    - bun
setup:
  steps:
    - name: Build
      run: go build ./...
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Dependencies).To(HaveLen(2))
		Expect(cfg.Dependencies["nixpkgs"]).To(Equal([]string{"nodejs", "go"}))
		Expect(cfg.Dependencies["nixpkgs/nixos-unstable"]).To(Equal([]string{"bun"}))
	})

	It("resolves nixpkgs to default channel", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  nixpkgs:
    - hello
setup:
  steps:
    - name: Build
      run: hello
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := cfg.Dependencies.Resolve()
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(HaveLen(1))
		Expect(resolved[0].FlakeURI).To(Equal("github:NixOS/nixpkgs/" + project.DefaultNixpkgsChannel))
		Expect(resolved[0].Attr).To(Equal("hello"))
		Expect(resolved[0].Host).To(Equal("github.com"))
	})

	It("resolves nixpkgs/<channel> to that channel", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  nixpkgs/nixos-unstable:
    - bun
setup:
  steps:
    - name: Build
      run: bun
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := cfg.Dependencies.Resolve()
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(HaveLen(1))
		Expect(resolved[0].FlakeURI).To(Equal("github:NixOS/nixpkgs/nixos-unstable"))
		Expect(resolved[0].Host).To(Equal("github.com"))
	})

	It("resolves git+https flake URIs preserving the host", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  git+https://example.com/foo:
    - my_pkg
setup:
  steps:
    - name: Build
      run: my_pkg
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := cfg.Dependencies.Resolve()
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(HaveLen(1))
		Expect(resolved[0].FlakeURI).To(Equal("git+https://example.com/foo"))
		Expect(resolved[0].Host).To(Equal("example.com"))
	})

	It("rejects both image and dependencies set", func() {
		writeYAML(dir, "kvarn.yml", `
image: node:20
dependencies:
  nixpkgs:
    - hello
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	It("rejects whitespace-only image", func() {
		writeYAML(dir, "kvarn.yml", `
image: "   "
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("whitespace-only"))
	})

	It("rejects unknown source forms", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  "!@#$":
    - hello
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported dependency source"))
	})

	It("rejects invalid attribute names", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  nixpkgs:
    - "my pkg"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid attribute"))
	})

	It("rejects attribute names that look like flag injection", func() {
		writeYAML(dir, "kvarn.yml", `
dependencies:
  nixpkgs:
    - "--evil"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid attribute"))
	})

	It("returns the migration error when the legacy tools field is present", func() {
		writeYAML(dir, "kvarn.yml", `
tools:
  - go@1.22
setup:
  steps:
    - name: Build
      run: go build ./...
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("`tools:` has been replaced by `dependencies:`"))
	})

	It("rejects /nix as a cache path", func() {
		writeYAML(dir, "kvarn.yml", `
cache:
  paths:
    - /nix/store
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("first-class feature"))
	})
})

var _ = Describe("Network config", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("accepts valid hostnames", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "proxy.golang.org"
    - "ghcr.io"
    - "my-registry.example.com"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(Equal([]string{
			"proxy.golang.org",
			"ghcr.io",
			"my-registry.example.com",
		}))
	})

	It("accepts valid IP addresses", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "192.168.1.1"
    - "10.0.0.1"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(HaveLen(2))
	})

	It("accepts IPv6 addresses", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "::1"
    - "2001:db8::1"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Network.AllowedHosts).To(HaveLen(2))
	})

	It("rejects entries with schemes", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "https://proxy.golang.org"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("scheme"))
	})

	It("rejects entries with paths", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "proxy.golang.org/cached"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("path"))
	})

	It("rejects entries with ports", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "proxy.golang.org:443"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("port"))
	})

	It("rejects empty entries", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - ""
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty"))
	})

	It("rejects invalid hostnames", func() {
		writeYAML(dir, "kvarn.yml", `
network:
  allowed_hosts:
    - "not a hostname!"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not a valid hostname"))
	})
})

var _ = Describe("Environment config", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("accepts valid env entries", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  FOO: bar
  GOPATH: /home/kvarn/custom-gopath
  _UNDERSCORE: ok
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Environment).To(HaveKeyWithValue("FOO", "bar"))
		Expect(cfg.Environment).To(HaveKeyWithValue("GOPATH", "/home/kvarn/custom-gopath"))
		Expect(cfg.Environment).To(HaveKeyWithValue("_UNDERSCORE", "ok"))
	})

	It("accepts empty values", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  FOO: ""
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Environment).To(HaveKeyWithValue("FOO", ""))
	})

	It("rejects empty key", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  "": bar
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty key"))
	})

	It("rejects keys that don't match POSIX env-var name rules", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  "1FOO": bar
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("POSIX env-var name"))
	})

	It("rejects keys with hyphens", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  "FOO-BAR": baz
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("POSIX env-var name"))
	})

	It("rejects values containing newlines", func() {
		writeYAML(dir, "kvarn.yml", "environment:\n  FOO: \"line1\\nline2\"\nsetup:\n  steps:\n    - name: Build\n      run: echo ok\n")
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("NUL or newline"))
	})

	It("rejects values containing NUL", func() {
		writeYAML(dir, "kvarn.yml", "environment:\n  FOO: \"a\\x00b\"\nsetup:\n  steps:\n    - name: Build\n      run: echo ok\n")
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("NUL or newline"))
	})
})

var _ = Describe("Secrets config", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "projectconfig-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	It("accepts valid secret names", func() {
		writeYAML(dir, "kvarn.yml", `
secrets:
  - HMAC_SIGN
  - DOCKERHUB_TOKEN
setup:
  steps:
    - name: Build
      run: echo ok
`)
		cfg, err := project.Load(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Secrets).To(ConsistOf("HMAC_SIGN", "DOCKERHUB_TOKEN"))
	})

	It("rejects empty secret name", func() {
		writeYAML(dir, "kvarn.yml", `
secrets:
  - ""
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty entry"))
	})

	It("rejects secret names that don't match POSIX env-var rules", func() {
		writeYAML(dir, "kvarn.yml", `
secrets:
  - "1FOO"
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("POSIX env-var name"))
	})

	It("rejects duplicated secret names", func() {
		writeYAML(dir, "kvarn.yml", `
secrets:
  - FOO
  - FOO
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("duplicated"))
	})

	It("rejects secret names overlapping with environment keys", func() {
		writeYAML(dir, "kvarn.yml", `
environment:
  FOO: bar
secrets:
  - FOO
setup:
  steps:
    - name: Build
      run: echo ok
`)
		_, err := project.Load(dir)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("overlaps with environment"))
	})
})

func writeYAML(dir, name, content string) {
	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	Expect(err).NotTo(HaveOccurred())
}

func validConfig() string {
	return `
setup:
  steps:
    - name: Install
      run: npm install
`
}
