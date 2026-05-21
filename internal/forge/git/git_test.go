package forgegit_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/forge"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
)

var _ = Describe("Git Forge", func() {
	var g *forgegit.Git

	BeforeEach(func() {
		g = forgegit.New()
	})

	Describe("ResolveCloneURL", func() {
		It("returns the input as-is for HTTPS URLs", func() {
			url, err := g.ResolveCloneURL("https://git.internal/team/tools.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("https://git.internal/team/tools.git"))
		})

		It("returns the input as-is for SSH URLs", func() {
			url, err := g.ResolveCloneURL("git@git.internal:team/tools.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("git@git.internal:team/tools.git"))
		})
	})

	Describe("ResolveCredentials", func() {
		It("passes through token", func() {
			creds, err := g.ResolveCredentials(context.Background(), map[string]string{
				"token": "my-token",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Token).To(Equal("my-token"))
		})

		It("passes through SSH key path", func() {
			creds, err := g.ResolveCredentials(context.Background(), map[string]string{
				"ssh_key_path": "/path/to/key",
				"ssh_key_pass": "passphrase",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(creds.SSHKey)).To(Equal("/path/to/key"))
			Expect(creds.SSHKeyPass).To(Equal("passphrase"))
		})

		It("passes through username/password", func() {
			creds, err := g.ResolveCredentials(context.Background(), map[string]string{
				"username": "user",
				"password": "pass",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Username).To(Equal("user"))
			Expect(creds.Password).To(Equal("pass"))
		})

		It("handles empty config", func() {
			creds, err := g.ResolveCredentials(context.Background(), map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(creds).NotTo(BeNil())
		})
	})

	Describe("CreatePullRequest", func() {
		It("returns an error", func() {
			_, err := g.CreatePullRequest(context.Background(), forge.CreatePROpts{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not supported"))
		})
	})

	Describe("PostComment", func() {
		It("returns an error", func() {
			err := g.PostComment(context.Background(), forge.PostCommentOpts{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not supported"))
		})
	})

	Describe("SCM", func() {
		It("returns a git SCM", func() {
			Expect(g.SCM()).NotTo(BeNil())
		})
	})
})
