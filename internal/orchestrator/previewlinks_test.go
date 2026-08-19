package orchestrator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	projcfg "github.com/aholstenson/kvarn/internal/config/project"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

var _ = Describe("previewLinksFor", func() {
	var svc *Service

	BeforeEach(func() {
		svc = &Service{
			previews: newPreviewManager(nil, PreviewPolicy{Domain: "preview.example.com"}, nil),
		}
	})

	// sitesConfig is a kvarn.yml declaring one site per pattern.
	sitesConfig := func(patterns map[string]string) *projconfig.Config {
		sites := make(map[string]projconfig.PreviewSite, len(patterns))
		for name, host := range patterns {
			sites[name] = projconfig.PreviewSite{Port: 3000, Host: host}
		}
		return &projconfig.Config{Preview: projconfig.Preview{Sites: sites}}
	}

	proj := &projcfg.Project{Name: "web"}

	It("resolves every site the branch declares", func() {
		links := svc.previewLinksFor(proj, sitesConfig(map[string]string{
			"web": "pr-{pr}.{domain}",
			"api": "api-pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Sites).To(Equal(map[string]string{
			"web": "https://pr-42.preview.example.com",
			"api": "https://api-pr-42.preview.example.com",
		}))
	})

	It("names the site called web as the primary one", func() {
		links := svc.previewLinksFor(proj, sitesConfig(map[string]string{
			"web":   "pr-{pr}.{domain}",
			"admin": "admin-pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Primary).To(Equal("https://pr-42.preview.example.com"))
	})

	It("falls back to the first site by name when none is called web", func() {
		links := svc.previewLinksFor(proj, sitesConfig(map[string]string{
			"storefront": "shop-pr-{pr}.{domain}",
			"admin":      "admin-pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Primary).To(Equal("https://admin-pr-42.preview.example.com"))
	})

	It("keeps the sites that resolve when a pull request site cannot", func() {
		links := svc.previewLinksFor(proj, sitesConfig(map[string]string{
			"web": "{ref}.{domain}",
			"api": "api-pr-{pr}.{domain}",
		}), "feature/login", "")

		Expect(links.Sites).To(HaveKey("web"))
		Expect(links.Sites).NotTo(HaveKey("api"))
		Expect(links.Primary).To(Equal(links.Sites["web"]))
	})

	It("resolves nothing when the run has no pull request and every site needs one", func() {
		links := svc.previewLinksFor(proj, sitesConfig(map[string]string{
			"web": "pr-{pr}.{domain}",
		}), "feature/login", "")

		Expect(links.Primary).To(BeEmpty())
		Expect(links.Sites).To(BeEmpty())
	})

	It("uses the project's own domain over the operator's", func() {
		scoped := &projcfg.Project{Name: "web", Preview: projcfg.Preview{Domain: "demo.example.org"}}
		links := svc.previewLinksFor(scoped, sitesConfig(map[string]string{
			"web": "pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Primary).To(Equal("https://pr-42.demo.example.org"))
	})

	It("resolves nothing when previews are off for the project", func() {
		off := &projcfg.Project{Name: "web", Preview: projcfg.Preview{Enabled: ptr(false)}}
		links := svc.previewLinksFor(off, sitesConfig(map[string]string{
			"web": "pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Primary).To(BeEmpty())
	})

	It("resolves nothing when the operator configured no preview domain", func() {
		disabled := &Service{previews: newPreviewManager(nil, PreviewPolicy{}, nil)}
		links := disabled.previewLinksFor(proj, sitesConfig(map[string]string{
			"web": "pr-{pr}.{domain}",
		}), "feature/login", "42")

		Expect(links.Primary).To(BeEmpty())
	})

	It("resolves nothing when the branch declares no preview", func() {
		Expect(svc.previewLinksFor(proj, &projconfig.Config{}, "feature/login", "42").Primary).To(BeEmpty())
		Expect(svc.previewLinksFor(proj, nil, "feature/login", "42").Primary).To(BeEmpty())
	})
})
