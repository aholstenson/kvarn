package orchestrator

import (
	"sort"

	projcfg "github.com/aholstenson/kvarn/internal/config/project"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// previewLinks are the addresses a preview of one ref answers on.
//
// They are worked out from the branch's `preview.sites` and the project's
// domain rather than read off a running preview, because the comment that
// carries them is written before anybody has asked for the preview. Where the
// operator has configured an `auto_start` route, following the link in the
// comment is what brings the preview into being.
type previewLinks struct {
	// Primary is the one address to put behind "the preview": the site named
	// "web" when the repository declares one, else the first by name. Empty
	// when nothing resolved.
	Primary string
	// Sites is every resolved site by name, for a comment that names one in
	// particular.
	Sites map[string]string
}

// previewLinksFor resolves the preview addresses for a ref, expanded for a pull
// request when the run has one.
//
// Every reason a link cannot be produced — previews turned off for the project,
// no `preview:` block in the branch's kvarn.yml, a `{pr}` pattern on a run with
// no pull request — yields an empty value rather than an error. That is what
// lets a comment template guard on the field with `{{ with }}`, and a link
// nobody can form is never worth failing a delivery over.
func (s *Service) previewLinksFor(
	proj *projcfg.Project, cfg *projconfig.Config, ref, pr string,
) previewLinks {
	if proj == nil || cfg == nil || ref == "" || !cfg.Preview.Enabled() {
		return previewLinks{}
	}
	domain, err := s.previewDomain(proj)
	if err != nil {
		return previewLinks{}
	}

	names := make([]string, 0, len(cfg.Preview.Sites))
	for name := range cfg.Preview.Sites {
		names = append(names, name)
	}
	sort.Strings(names)

	vars := projconfig.HostVars{Ref: ref, PR: pr}
	sites := make([]projconfig.ResolvedSite, 0, len(names))
	for _, name := range names {
		site := cfg.Preview.Sites[name]
		// A site that cannot be expanded — usually a `{pr}` pattern on a run
		// that has not opened its pull request yet — must not take the sites
		// that can be expanded with it.
		host, err := projconfig.ResolveHost(site.Host, vars, domain)
		if err != nil {
			continue
		}
		sites = append(sites, projconfig.ResolvedSite{Name: name, Port: site.Port, Host: host})
	}
	if len(sites) == 0 {
		return previewLinks{}
	}
	return previewLinks{Primary: primaryPreviewURL(sites), Sites: previewURLs(sites)}
}

// primaryPreviewURL picks the address a person means by "the preview". It is
// the rule preview.PrimaryURL applies to a booted preview, applied to the same
// sites before one exists, so a comment and a running preview cannot name
// different sites as the main one. Sites arrive sorted by name.
func primaryPreviewURL(sites []projconfig.ResolvedSite) string {
	best := sites[0]
	for _, site := range sites {
		if site.Name == "web" {
			best = site
			break
		}
	}
	return best.URL()
}
