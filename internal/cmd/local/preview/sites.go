package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/project"
)

// A local preview reaches its sites in one of two ways.
//
// Without a base domain there are no hostnames, so each site gets a loopback
// port and the port is what tells sites apart — the same server, addressed the
// only way a machine without a domain can address it.
//
// With --base-domain the sites get the hostnames they would have on the
// orchestrator, formed from the same patterns by the same resolver, and one
// Host-routed listener carries all of them. That is what makes a repository
// whose behaviour depends on its own domain — virtual hosts, cookie scopes,
// absolute links, OAuth redirects — testable before it is deployed.

// defaultIngressPort is where the Host-routed listener lands when the sites do
// not share a guest port to borrow. It is above 1024 so the command does not
// need privileges, and it shows up in every site URL.
const defaultIngressPort uint16 = 8080

// hostResolveTimeout bounds the check for whether a site's hostname already
// resolves on this machine. The answer only decides whether an /etc/hosts hint
// is printed, so a slow resolver must not hold the preview up.
const hostResolveTimeout = 2 * time.Second

// plannedSite is one site of the preview with its host-side address settled.
type plannedSite struct {
	Name      string
	GuestPort uint16
	// URL is the address to open, and what the guest is told this site is
	// called through KVARN_PREVIEW_URL_<SITE>.
	URL string

	// Host is the hostname this site answers on in domain mode, empty
	// otherwise.
	Host string
	// resolvesLocally records that Host already resolves to a loopback address
	// on this machine, so the run knows whether to print an /etc/hosts hint.
	resolvesLocally bool

	// Listener is this site's own loopback listener in port mode, nil in
	// domain mode where one ingress listener serves every site.
	Listener net.Listener
}

// sitePlan is every site of the preview, in stable name order, with the host
// listeners already bound. Binding happens before the VM boots: a port
// collision is a configuration problem, and finding it after a boot wastes one.
type sitePlan struct {
	sites []plannedSite

	// ingress is the single Host-routed listener in domain mode, nil in port
	// mode. ingressPort is the port it bound, which appears in every site URL.
	ingress     net.Listener
	ingressPort uint16
	domain      string

	// served records that the servers have taken over the listeners, so the
	// bind-time cleanup does not close them a second time. The sites
	// themselves are kept either way: they are what the run reports on.
	served bool
}

// planSites works out how every declared site is reached from this machine and
// binds the listeners for it.
func (c *Cmd) planSites(ctx context.Context, cfg *project.Config) (*sitePlan, error) {
	names := make([]string, 0, len(cfg.Preview.Sites))
	for name := range cfg.Preview.Sites {
		names = append(names, name)
	}
	sort.Strings(names)

	if c.BaseDomain != "" {
		if len(c.Port) > 0 {
			return nil, fmt.Errorf(
				"--port and --base-domain are two different ways of reaching a site: " +
					"with a base domain every site is served by one Host-routed listener, " +
					"which --ingress-port sets the port of")
		}
		return c.bindDomain(ctx, cfg, names)
	}
	return c.bindPorts(cfg, names)
}

// bindPorts claims a loopback port for every declared site. Sites that share a
// guest port still get one host port each: without hostnames the port is what
// tells them apart.
func (c *Cmd) bindPorts(cfg *project.Config, names []string) (*sitePlan, error) {
	for name := range c.Port {
		if _, ok := cfg.Preview.Sites[name]; !ok {
			return nil, fmt.Errorf("--port names site %q, which kvarn.yml does not declare", name)
		}
	}

	plan := &sitePlan{}
	for _, name := range names {
		want := cfg.Preview.Sites[name].Port
		explicit := false
		if p, ok := c.Port[name]; ok {
			want = p
			explicit = true
		}

		ln, err := bindHostPort(want)
		if err != nil {
			plan.closeListeners()
			return nil, fmt.Errorf("bind a port for site %q: %w", name, err)
		}
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		// An explicit --port is a request, not a preference: silently serving
		// somewhere else would break whatever the caller pinned the port for.
		if explicit && port != want {
			ln.Close()
			plan.closeListeners()
			return nil, fmt.Errorf("port %d for site %q is already in use", want, name)
		}

		plan.sites = append(plan.sites, plannedSite{
			Name:      name,
			GuestPort: cfg.Preview.Sites[name].Port,
			Listener:  ln,
			URL:       fmt.Sprintf("http://localhost:%d", port),
		})
	}
	return plan, nil
}

// bindDomain resolves every site's hostname under the base domain and binds the
// one listener all of them are served from.
func (c *Cmd) bindDomain(ctx context.Context, cfg *project.Config, names []string) (*sitePlan, error) {
	ref := c.Ref
	if ref == "" {
		ref = DefaultRefLabel
	}

	plan := &sitePlan{domain: strings.Trim(c.BaseDomain, ".")}
	claimed := make(map[string]string, len(names))
	for _, name := range names {
		site := cfg.Preview.Sites[name]
		host, err := project.ResolveHost(site.Host, ref, c.BaseDomain)
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", name, err)
		}
		// Two sites answering to one name is not resolvable at request time,
		// and the orchestrator would reject it for the same reason.
		if other, ok := claimed[host]; ok {
			return nil, fmt.Errorf(
				"sites %q and %q both resolve to %s: give them different `host` patterns in kvarn.yml",
				other, name, host)
		}
		claimed[host] = name

		plan.sites = append(plan.sites, plannedSite{
			Name:      name,
			GuestPort: site.Port,
			Host:      host,
		})
	}

	ln, err := c.bindIngress(plan.sites)
	if err != nil {
		return nil, err
	}
	plan.ingress = ln
	plan.ingressPort = uint16(ln.Addr().(*net.TCPAddr).Port)

	for i := range plan.sites {
		plan.sites[i].URL = siteURL(plan.sites[i].Host, plan.ingressPort)
	}
	resolveLocally(ctx, plan.sites)

	return plan, nil
}

// bindIngress binds the listener every site is served from. An explicit
// --ingress-port has to be honoured exactly. Otherwise the port the sites share
// inside the guest is tried first, because a URL whose port matches the one the
// server listens on is the same URL inside the VM and outside it — which is
// what lets a ready check fetch the site by name.
func (c *Cmd) bindIngress(sites []plannedSite) (net.Listener, error) {
	if c.IngressPort != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.IngressPort))
		if err != nil {
			return nil, fmt.Errorf("bind ingress port %d: %w", c.IngressPort, err)
		}
		return ln, nil
	}

	if port, ok := sharedGuestPort(sites); ok {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			return ln, nil
		}
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", defaultIngressPort)); err == nil {
		return ln, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind an ingress port: %w", err)
	}
	return ln, nil
}

// sharedGuestPort returns the port every site listens on inside the guest, when
// there is exactly one.
func sharedGuestPort(sites []plannedSite) (uint16, bool) {
	if len(sites) == 0 {
		return 0, false
	}
	port := sites[0].GuestPort
	for _, s := range sites {
		if s.GuestPort != port {
			return 0, false
		}
	}
	return port, port != 0
}

// siteURL is the address a site answers on. The port is left off when it is the
// default one, so the URL reads the way it would in production.
func siteURL(host string, port uint16) string {
	if port == 80 {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// resolveLocally records which hostnames already point at loopback on this
// machine. Names under a made-up domain usually do not until somebody adds
// them to /etc/hosts, and a browser that cannot resolve the name fails in a way
// that looks nothing like the missing hosts entry it is.
func resolveLocally(ctx context.Context, sites []plannedSite) {
	ctx, cancel := context.WithTimeout(ctx, hostResolveTimeout)
	defer cancel()

	// Concurrently, so a domain nothing answers for costs one timeout for the
	// whole preview rather than one per site.
	var wg sync.WaitGroup
	for i := range sites {
		wg.Add(1)
		go func(site *plannedSite) {
			defer wg.Done()
			addrs, err := net.DefaultResolver.LookupHost(ctx, site.Host)
			if err != nil {
				return
			}
			for _, addr := range addrs {
				if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
					site.resolvesLocally = true
					return
				}
			}
		}(&sites[i])
	}
	wg.Wait()
}

// urls maps site name to the address it answers on, for the serve environment.
func (p *sitePlan) urls() map[string]string {
	out := make(map[string]string, len(p.sites))
	for _, site := range p.sites {
		out[site.Name] = site.URL
	}
	return out
}

// guestAliases are the name→address entries the guest needs so that code
// running inside the VM resolves the preview's own hostnames to the VM itself.
// Without them a server that reads its site URL back — a ready check fetching
// it, an app calling its sibling site — would try to resolve a name that only
// means something on the developer's machine.
func (p *sitePlan) guestAliases() map[string]string {
	if p.ingress == nil {
		return nil
	}
	aliases := make(map[string]string, len(p.sites))
	for _, site := range p.sites {
		aliases[site.Host] = "127.0.0.1"
	}
	return aliases
}

// closeListeners releases the bound ports. It is the cleanup path for a preview
// that never started serving; once the servers own the listeners, closing them
// is their job.
func (p *sitePlan) closeListeners() {
	if p.served {
		return
	}
	if p.ingress != nil {
		p.ingress.Close()
	}
	for _, site := range p.sites {
		if site.Listener != nil {
			site.Listener.Close()
		}
	}
	p.sites = nil
}

// serve starts carrying traffic into the guest: one forwarder per site in port
// mode, one Host-routed ingress in domain mode. onUnreachable is called the
// first time a site's traffic cannot be delivered.
func (p *sitePlan) serve(
	dial func(context.Context, uint16) (net.Conn, error),
	onUnreachable func(name string, guestPort uint16, err error),
) *servers {
	log := slog.With("component", "local-preview")
	s := &servers{}

	if p.ingress != nil {
		routes := make(map[string]ingressRoute, len(p.sites))
		for _, site := range p.sites {
			routes[preview.NormalizeHost(site.Host)] = ingressRoute{
				Site: site.Name,
				Port: site.GuestPort,
			}
		}
		s.list = append(s.list, startIngress(p.ingress, routes, dial, log, onUnreachable))
		p.served = true
		return s
	}

	for _, site := range p.sites {
		var unreachable func(error)
		if onUnreachable != nil {
			unreachable = func(err error) { onUnreachable(site.Name, site.GuestPort, err) }
		}
		s.list = append(s.list, startForward(site.Listener, site.GuestPort, dial, log, unreachable))
	}
	p.served = true
	return s
}

// canceled reports whether a delivery failure is a caller giving up rather than
// the guest failing to answer — a browser that navigated away, or the preview
// shutting down while a request was in flight. Neither says anything about
// whether the site's server is listening, and reporting them as unreachable
// contradicts a preview that is working.
func canceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || ctx.Err() != nil
}

// report prints the addresses the preview is serving on, and the guest port
// each one is carried to. The guest port is what a repository has to get right
// — a server listening somewhere else, or only on the guest's loopback, is
// invisible from here — so it is named rather than left to be guessed.
func (p *sitePlan) report(w io.Writer) {
	fmt.Fprintln(w)
	width := 0
	for _, site := range p.sites {
		if len(site.Name) > width {
			width = len(site.Name)
		}
	}
	for _, site := range p.sites {
		fmt.Fprintf(w, "  %-*s  %s  →  guest port %d\n", width, site.Name, site.URL, site.GuestPort)
	}

	if hosts := p.unresolvedHosts(); len(hosts) > 0 {
		fmt.Fprintf(w, "\n%s does not resolve here yet. Add to /etc/hosts:\n\n  127.0.0.1\t%s\n",
			pluralHosts(len(hosts)), strings.Join(hosts, " "))
	}
	if names := p.mismatchedPorts(); len(names) > 0 {
		fmt.Fprintf(w, "\nInside the VM these names resolve to the guest itself, where nothing "+
			"listens on port %d — %s bind other ports. Anything in the preview that "+
			"fetches its own site URL (a ready check, one site calling another) will "+
			"fail until the sites share one port, or --ingress-port matches theirs.\n",
			p.ingressPort, strings.Join(names, ", "))
	}

	fmt.Fprintln(w, "\nPress Ctrl-C to stop.")
}

// unresolvedHosts are the site hostnames this machine cannot resolve to
// loopback, which is what the /etc/hosts hint lists.
func (p *sitePlan) unresolvedHosts() []string {
	var out []string
	for _, site := range p.sites {
		if site.Host != "" && !site.resolvesLocally {
			out = append(out, site.Host)
		}
	}
	return out
}

// mismatchedPorts are the sites whose server listens on a port the ingress did
// not manage to borrow, so their URL only works from outside the VM.
func (p *sitePlan) mismatchedPorts() []string {
	if p.ingress == nil {
		return nil
	}
	var out []string
	for _, site := range p.sites {
		if site.GuestPort != p.ingressPort {
			out = append(out, site.Name)
		}
	}
	return out
}

func pluralHosts(n int) string {
	if n == 1 {
		return "That name"
	}
	return "Those names"
}

// servers is everything carrying host traffic into the guest, closed together.
type servers struct {
	list []io.Closer
}

func (s *servers) close() {
	for _, c := range s.list {
		c.Close()
	}
}
