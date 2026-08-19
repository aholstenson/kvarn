package sandbox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

const (
	// hostsStageDir and hostsStageName are where the generated block is
	// staged before it is appended to /etc/hosts. Uploading the block and
	// appending it with `cat` keeps project-supplied names out of a shell
	// command line entirely.
	hostsStageDir  = "/run"
	hostsStageName = "kvarn-hosts"

	// hostsTimeoutSeconds caps the append. It is two file operations on a
	// tmpfs, so reaching this bound means the guest is wedged.
	hostsTimeoutSeconds uint32 = 30
)

// hostAliases merges the project's declared aliases with the ones the caller
// supplied. The caller's win on a name they both map: kvarn.yml describes the
// repository's usual development names, while an alias passed in names
// something this particular run has just created.
func (o Opts) hostAliases() map[string]string {
	var merged map[string]string
	add := func(aliases map[string]string) {
		if len(aliases) == 0 {
			return
		}
		if merged == nil {
			merged = make(map[string]string, len(aliases))
		}
		for name, addr := range aliases {
			merged[name] = addr
		}
	}
	if o.Config != nil {
		add(o.Config.Network.HostAliases)
	}
	add(o.HostAliases)
	return merged
}

// exactHostAliases keeps the entries naming one literal host, which are the
// only ones expressible as /etc/hosts lines. Wildcards are answered by the VM's
// DNS forwarder instead; see project.Network.ExactHostAliases.
func exactHostAliases(aliases map[string]string) map[string]string {
	exact := make(map[string]string, len(aliases))
	for name, addr := range aliases {
		if !strings.HasPrefix(name, "*.") {
			exact[name] = addr
		}
	}
	return exact
}

// hostEntry is one name→address mapping bound for the guest's name tables.
type hostEntry struct {
	Name    string
	Address string
}

// sortedHostEntries orders a kvarn.yml `network.host_aliases` map by name.
// YAML maps have no order of their own, and the /etc/hosts block built from
// them is byte-compared in tests and read by humans inside the VM, so the order
// has to come from somewhere stable.
func sortedHostEntries(aliases map[string]string) []hostEntry {
	entries := make([]hostEntry, 0, len(aliases))
	for name, addr := range aliases {
		entries = append(entries, hostEntry{Name: name, Address: strings.TrimSpace(addr)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// renderHostsBlock builds the lines appended to the guest's /etc/hosts. The
// marker comment tells anyone reading the file inside the VM where the entries
// came from.
func renderHostsBlock(entries []hostEntry) []byte {
	var b strings.Builder
	b.WriteString("\n# kvarn: network.host_aliases from kvarn.yml\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%s\n", e.Address, e.Name)
	}
	return []byte(b.String())
}

// ConfigureHostAliases adds the run's exact host aliases — the project's
// `network.host_aliases` plus whatever the caller supplied — to the guest's
// /etc/hosts. Wildcard entries have no representation in that file and are
// answered by the VM's DNS forwarder instead.
//
// The entries are appended rather than written over the file: the boot-time
// content maps localhost and the VM's own hostname, and a program that cannot
// resolve either behaves far worse than one missing a project alias.
//
// This has to run before any step does. Podman seeds a container's hosts file
// from the VM's at creation time, so a project that brings up its own
// containers only inherits the aliases that are already in place when it starts
// them.
func ConfigureHostAliases(ctx context.Context, runner RunnerProxy, aliases map[string]string) error {
	entries := sortedHostEntries(aliases)
	if len(entries) == 0 {
		return nil
	}

	if _, err := runner.UploadFiles(ctx, &v1.UploadFilesRequest{
		WorkingDir: hostsStageDir,
		Files: []*v1.FileContent{{
			Path:    hostsStageName,
			Content: renderHostsBlock(entries),
			Mode:    0o644,
		}},
	}); err != nil {
		return fmt.Errorf("stage hosts entries: %w", err)
	}

	staged := hostsStageDir + "/" + hostsStageName
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:        fmt.Sprintf("cat %s >> /etc/hosts && rm -f %s", staged, staged),
		Privileged:     true,
		TimeoutSeconds: hostsTimeoutSeconds,
	})
	if err != nil {
		return fmt.Errorf("append hosts entries: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("append hosts entries failed (exit %d): %s",
			resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return nil
}
