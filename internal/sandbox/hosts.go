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

// hostEntry is one name→address mapping bound for the guest's name tables.
type hostEntry struct {
	Name    string
	Address string
}

// sortedHostEntries orders a kvarn.yml `network.host_aliases` map by name.
// YAML maps have no order of their own, and both consumers — the /etc/hosts
// block and podman's --add-host flags — are byte-compared in tests and read by
// humans inside the VM, so the order has to come from somewhere stable.
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

// ConfigureHostAliases adds the project's exact `network.host_aliases` entries
// to the guest's /etc/hosts. Wildcard entries have no representation in that
// file and are answered by the VM's DNS forwarder; see
// project.Network.ExactHostAliases.
//
// The entries are appended rather than written over the file: the boot-time
// content maps localhost and the VM's own hostname, and a program that cannot
// resolve either behaves far worse than one missing a project alias.
//
// This has to run before the container starts. Podman seeds a container's
// hosts file from the VM's at creation time, so an entry added afterwards
// would reach the VM but not the image-mode shell where the steps actually
// run. ContainerProxy.Start passes the same entries as --add-host for the
// same reason, which makes the container's copy explicit rather than
// dependent on podman's base_hosts_file default.
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
