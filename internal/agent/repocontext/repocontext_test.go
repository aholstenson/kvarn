package repocontext_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
)

var _ = Describe("Load", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "repocontext-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(dir)
	})

	Describe("Instructions", func() {
		It("returns empty instructions when no files exist", func() {
			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Instructions).To(BeEmpty())
		})

		It("loads AGENTS.md only", func() {
			Expect(os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents instructions"), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Instructions).To(Equal("agents instructions"))
		})

		It("loads CLAUDE.md only", func() {
			Expect(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude instructions"), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Instructions).To(Equal("claude instructions"))
		})

		It("concatenates both files with separator when different", func() {
			Expect(os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents content"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude content"), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Instructions).To(Equal("agents content\n---\nclaude content"))
		})

		It("includes content once when AGENTS.md is symlinked to CLAUDE.md", func() {
			claudePath := filepath.Join(dir, "CLAUDE.md")
			agentsPath := filepath.Join(dir, "AGENTS.md")

			Expect(os.WriteFile(claudePath, []byte("shared instructions"), 0o644)).To(Succeed())
			Expect(os.Symlink(claudePath, agentsPath)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Instructions).To(Equal("shared instructions"))
		})
	})

	Describe("Skills", func() {
		It("discovers a skill in .agents/skills/", func() {
			skillDir := filepath.Join(dir, ".agents", "skills", "deploy")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: deploy
description: Deploy the application
---
# Deploy

Run the deploy script.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(HaveLen(1))
			Expect(rc.Skills[0].Name).To(Equal("deploy"))
			Expect(rc.Skills[0].Description).To(Equal("Deploy the application"))
			Expect(rc.Skills[0].Body).To(ContainSubstring("# Deploy"))
			Expect(rc.Skills[0].Dir).To(Equal(filepath.Join(".agents", "skills", "deploy")))
		})

		It("discovers a skill in .claude/skills/", func() {
			skillDir := filepath.Join(dir, ".claude", "skills", "lint")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: lint
description: Run linting
---
Lint the code.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(HaveLen(1))
			Expect(rc.Skills[0].Name).To(Equal("lint"))
		})

		It("deduplicates when .claude/skills is symlinked to .agents/skills", func() {
			agentsSkills := filepath.Join(dir, ".agents", "skills")
			skillDir := filepath.Join(agentsSkills, "test-skill")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: A test skill
---
Body content.
`), 0o644)).To(Succeed())

			claudeDir := filepath.Join(dir, ".claude")
			Expect(os.MkdirAll(claudeDir, 0o755)).To(Succeed())
			Expect(os.Symlink(agentsSkills, filepath.Join(claudeDir, "skills"))).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(HaveLen(1))
		})

		It("skips subdirectories without SKILL.md", func() {
			noSkillDir := filepath.Join(dir, ".agents", "skills", "empty")
			Expect(os.MkdirAll(noSkillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(noSkillDir, "README.md"), []byte("not a skill"), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(BeEmpty())
		})

		It("skips skills with missing description", func() {
			skillDir := filepath.Join(dir, ".agents", "skills", "bad")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: bad
---
No description.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(BeEmpty())
		})

		It("skips skills with unparseable YAML", func() {
			skillDir := filepath.Join(dir, ".agents", "skills", "broken")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: [broken
  invalid: yaml: {{
---
Body.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(BeEmpty())
		})

		It("enumerates bundled resources", func() {
			skillDir := filepath.Join(dir, ".agents", "skills", "with-resources")
			Expect(os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(skillDir, "references"), 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(skillDir, "assets"), 0o755)).To(Succeed())

			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: with-resources
description: Skill with resources
---
Use the scripts.
`), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "scripts", "run.sh"), []byte("#!/bin/sh"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("# Guide"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "assets", "logo.png"), []byte("PNG"), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(HaveLen(1))
			Expect(rc.Skills[0].Resources).To(ConsistOf(
				filepath.Join("scripts", "run.sh"),
				filepath.Join("references", "guide.md"),
				filepath.Join("assets", "logo.png"),
			))
		})

		It("uses first-found on name collision within same scope", func() {
			// Create two skills directories with same name in different scan paths.
			agentsSkillDir := filepath.Join(dir, ".agents", "skills", "dupe")
			Expect(os.MkdirAll(agentsSkillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(agentsSkillDir, "SKILL.md"), []byte(`---
name: dupe
description: First version
---
First body.
`), 0o644)).To(Succeed())

			claudeSkillDir := filepath.Join(dir, ".claude", "skills", "dupe")
			Expect(os.MkdirAll(claudeSkillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(claudeSkillDir, "SKILL.md"), []byte(`---
name: dupe
description: Second version
---
Second body.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())

			dupeSkills := 0
			for _, s := range rc.Skills {
				if s.Name == "dupe" {
					dupeSkills++
				}
			}
			Expect(dupeSkills).To(Equal(1))
		})

		It("handles malformed YAML with unquoted colons via fallback", func() {
			skillDir := filepath.Join(dir, ".agents", "skills", "colon-fix")
			Expect(os.MkdirAll(skillDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: colon-fix
description: Use when: the user wants colons
---
Handle colons.
`), 0o644)).To(Succeed())

			rc, err := repocontext.Load(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Skills).To(HaveLen(1))
			Expect(rc.Skills[0].Description).To(Equal("Use when: the user wants colons"))
		})
	})
})
