package project_test

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// writePreviewConfig writes a kvarn.yml with the given preview block and loads
// it, returning whatever Load made of it.
func writePreviewConfig(body string) (*project.Config, error) {
	dir := GinkgoT().TempDir()
	err := os.WriteFile(filepath.Join(dir, "kvarn.yml"), []byte(body), 0o644)
	Expect(err).NotTo(HaveOccurred())
	return project.Load(dir)
}

var _ = Describe("Preview config", func() {
	Describe("RefLabel", func() {
		DescribeTable("reduces a git ref to exactly one DNS label",
			func(ref string) {
				label := project.RefLabel(ref)
				Expect(label).NotTo(BeEmpty())
				Expect(label).NotTo(ContainSubstring("."))
				Expect(label).NotTo(ContainSubstring("/"))
				Expect(len(label)).To(BeNumerically("<=", project.MaxRefLabelLen))
				Expect(label).To(Equal(strings.ToLower(label)))
				Expect(label).NotTo(HavePrefix("-"))
				Expect(label).NotTo(HaveSuffix("-"))
			},
			Entry("a plain branch", "main"),
			Entry("a slashed branch", "feat/add-preview-environments"),
			Entry("an uppercase branch", "Feature/Login"),
			Entry("a branch with punctuation", "fix/issue#123_(urgent)!"),
			Entry("a very long branch", strings.Repeat("very-long-branch-name-", 10)),
			Entry("a branch of only punctuation", "///"),
			Entry("a single character", "x"),
		)

		It("passes a plain lowercase ref through unchanged", func() {
			Expect(project.RefLabel("main")).To(Equal("main"))
			Expect(project.RefLabel("release-2")).To(Equal("release-2"))
		})

		It("is deterministic", func() {
			Expect(project.RefLabel("feat/login")).To(Equal(project.RefLabel("feat/login")))
		})

		It("keeps refs that share a readable form apart", func() {
			Expect(project.RefLabel("feat/login")).NotTo(Equal(project.RefLabel("feat-login")))
			Expect(project.RefLabel("Main")).NotTo(Equal(project.RefLabel("main")))
		})

		It("keeps a readable prefix when it shortens a long ref", func() {
			label := project.RefLabel("feat/" + strings.Repeat("a", 200))
			Expect(label).To(HavePrefix("feat-a"))
			Expect(len(label)).To(Equal(project.MaxRefLabelLen))
		})
	})

	Describe("ResolveHost", func() {
		It("defaults to {ref}.{domain}", func() {
			host, err := project.ResolveHost("", "main", "preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(Equal("main.preview.example.com"))
		})

		It("expands an explicit pattern", func() {
			host, err := project.ResolveHost("assets-{ref}.{domain}", "main", "preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(Equal("assets-main.preview.example.com"))
		})

		It("tolerates a domain written with leading or trailing dots", func() {
			host, err := project.ResolveHost("", "main", ".preview.example.com.")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(Equal("main.preview.example.com"))
		})

		It("refuses a hostname outside the configured domain", func() {
			_, err := project.ResolveHost("admin.example.com", "main", "preview.example.com")
			Expect(err).To(MatchError(ContainSubstring("outside the configured preview domain")))
		})

		It("refuses a suffix match that is not on a label boundary", func() {
			_, err := project.ResolveHost("evilpreview.example.com", "main", "preview.example.com")
			Expect(err).To(MatchError(ContainSubstring("outside the configured preview domain")))
		})

		It("refuses to resolve without a domain", func() {
			_, err := project.ResolveHost("", "main", "")
			Expect(err).To(MatchError(ContainSubstring("no preview domain is configured")))
		})

		It("keeps a hostile ref inside one label", func() {
			host, err := project.ResolveHost("", "evil.admin", "preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Count(host, ".")).To(Equal(strings.Count("main.preview.example.com", ".")))
			Expect(host).To(HaveSuffix(".preview.example.com"))
		})
	})

	Describe("EnvVarName", func() {
		It("uppercases the app name and replaces hyphens", func() {
			Expect(project.EnvVarName("web")).To(Equal("KVARN_PREVIEW_URL_WEB"))
			Expect(project.EnvVarName("admin-ui")).To(Equal("KVARN_PREVIEW_URL_ADMIN_UI"))
		})
	})

	Describe("Resolve", func() {
		It("resolves every app, sorted by name", func() {
			preview := project.Preview{Apps: map[string]project.PreviewApp{
				"web":    {Port: 3000},
				"assets": {Port: 8080, Host: "assets-{ref}.{domain}"},
			}}
			apps, err := preview.Resolve("feat/login", "preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(apps).To(HaveLen(2))
			Expect(apps[0].Name).To(Equal("assets"))
			Expect(apps[0].Port).To(Equal(uint16(8080)))
			Expect(apps[0].Host).To(HavePrefix("assets-feat-login-"))
			Expect(apps[1].Name).To(Equal("web"))
			Expect(apps[1].Host).To(HaveSuffix(".preview.example.com"))
		})

		It("reports which app's pattern was rejected", func() {
			preview := project.Preview{Apps: map[string]project.PreviewApp{
				"web": {Port: 3000, Host: "admin.other.com"},
			}}
			_, err := preview.Resolve("main", "preview.example.com")
			Expect(err).To(MatchError(ContainSubstring("preview.apps.web.host")))
		})
	})

	Describe("validation at load time", func() {
		It("accepts the single-app shorthand", func() {
			cfg, err := writePreviewConfig(`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
  ready:
    - { name: Web up, run: "curl -fsS http://localhost:3000/healthz" }
`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Preview.Enabled()).To(BeTrue())
			Expect(cfg.Preview.Apps["web"].Port).To(Equal(uint16(3000)))
			Expect(cfg.Preview.Apps["web"].Host).To(BeEmpty())
			Expect(cfg.Preview.Serve).To(HaveLen(1))
			Expect(cfg.Preview.Ready).To(HaveLen(1))
		})

		It("accepts several apps with explicit hosts", func() {
			cfg, err := writePreviewConfig(`
preview:
  apps:
    web:    { port: 3000, host: "{ref}.{domain}" }
    assets: { port: 8080, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Assets, run: npm run assets, app: assets }
`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Preview.Apps).To(HaveLen(2))
		})

		It("accepts several apps on one port when their hosts differ", func() {
			cfg, err := writePreviewConfig(`
preview:
  apps:
    web:    { port: 80 }
    assets: { port: 80, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Preview.Apps).To(HaveLen(2))
			Expect(cfg.Preview.Serve).To(HaveLen(1))
		})

		It("treats a config with no preview block as having no preview", func() {
			cfg, err := writePreviewConfig("setup:\n  steps: []\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Preview.Enabled()).To(BeFalse())
		})

		DescribeTable("rejects a malformed preview block",
			func(body, wantErr string) {
				_, err := writePreviewConfig(body)
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
			},
			Entry("a host pattern escaping the domain",
				`
preview:
  apps:
    web: { port: 3000, host: "admin.example.com" }
  serve:
    - { name: Web, run: npm start, app: web }
`, "must end in {domain}"),
			Entry("a host pattern with a scheme",
				`
preview:
  apps:
    web: { port: 3000, host: "https://{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
`, "must not contain a scheme"),
			Entry("a host pattern with a path",
				`
preview:
  apps:
    web: { port: 3000, host: "{ref}.{domain}/app" }
  serve:
    - { name: Web, run: npm start, app: web }
`, "must not contain a path"),
			Entry("an unknown placeholder",
				`
preview:
  apps:
    web: { port: 3000, host: "{branch}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
`, "unknown placeholder"),
			Entry("an app with no port",
				`
preview:
  apps:
    web: {}
  serve:
    - { name: Web, run: npm start, app: web }
`, "has no port"),
			Entry("two apps answering on the same host",
				`
preview:
  apps:
    web:    { port: 3000 }
    assets: { port: 8080, host: "{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Assets, run: npm run assets, app: assets }
`, `both answer on host "{ref}.{domain}"`),
			Entry("two serve steps binding one shared port",
				`
preview:
  apps:
    web:    { port: 80 }
    assets: { port: 80, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Assets, run: npm run assets, app: assets }
`, "share port 80"),
			Entry("a serve step naming an unknown app",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Other, run: npm run other, app: nope }
`, `names unknown app "nope"`),
			Entry("an app nothing serves",
				`
preview:
  apps:
    web:    { port: 3000 }
    assets: { port: 8080, host: "assets-{ref}.{domain}" }
  serve:
    - { name: Web, run: npm start, app: web }
`, "has no serve step"),
			Entry("two serve steps for one app",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
    - { name: Web again, run: npm start, app: web }
`, "is served by both"),
			Entry("a serve step with no app",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start }
`, "does not name an app"),
			Entry("a serve step with no run command",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, app: web }
`, "has empty run command"),
			Entry("a serve step with an absolute working_dir",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web, working_dir: /srv/web }
`, "absolute working_dir"),
			Entry("an app name that is not env-var safe",
				`
preview:
  apps:
    Web_App: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: Web_App }
`, "must be lowercase alphanumerics"),
			Entry("serve steps without apps",
				`
preview:
  serve:
    - { name: Web, run: npm start, app: web }
`, "declares serve or ready steps but no apps"),
			Entry("a ready check with no run command",
				`
preview:
  apps:
    web: { port: 3000 }
  serve:
    - { name: Web, run: npm start, app: web }
  ready:
    - { name: Web up }
`, "has empty run command"),
		)
	})
})
