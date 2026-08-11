// Package version implements the `kvarn version` CLI: reporting which build of
// the CLI is running. Everything it prints is compiled in, so it answers even
// when no orchestrator is reachable — which is the case it exists for, since
// "which binary am I actually running" is the first question a bug report needs
// answered.
package version

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"text/tabwriter"

	"github.com/aholstenson/kvarn/internal/buildinfo"
)

// Cmd prints the running build's version and the rest of its build identity.
type Cmd struct {
	Short bool `help:"Print just the version string." short:"s"`
	JSON  bool `help:"Emit JSON instead of a table." name:"json"`
}

// Info is one build's identity, as printed and as serialised by --json.
type Info struct {
	Version string `json:"version"`
	// Revision is the git commit stamped into the binary by the toolchain,
	// suffixed with "-dirty" when it was built from a modified tree. Empty when
	// the build carries no VCS stamp (`go build -buildvcs=false`, or a build
	// from an unpacked source archive).
	Revision string `json:"revision,omitempty"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
	// ImageConstraint is the range of VM image versions this build boots, i.e.
	// what `kvarn run`/`test` resolve against when no explicit --version is
	// given.
	ImageConstraint string `json:"image_constraint"`
}

func (c *Cmd) Run() error {
	info := Collect()
	switch {
	case c.JSON:
		return printJSON(os.Stdout, info)
	case c.Short:
		_, err := fmt.Fprintln(os.Stdout, info.Version)
		return err
	default:
		return render(os.Stdout, info)
	}
}

// Collect assembles the running build's identity from the values linked into
// the binary.
func Collect() Info {
	return Info{
		Version:         buildinfo.Version,
		Revision:        revision(),
		Go:              runtime.Version(),
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		ImageConstraint: buildinfo.ImageConstraint,
	}
}

// revision reads the VCS stamp the Go toolchain records at build time. A local
// `go build` in the checkout has it even when no -ldflags version was injected,
// which is what makes a "dev" build identifiable.
func revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "-dirty"
	}
	return rev
}

// render writes the human-readable listing: a headline naming the binary and
// its version, then the details that distinguish two builds carrying the same
// version string.
func render(w io.Writer, info Info) error {
	if _, err := fmt.Fprintf(w, "kvarn %s\n\n", info.Version); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if info.Revision != "" {
		fmt.Fprintf(tw, "revision\t%s\n", info.Revision)
	}
	fmt.Fprintf(tw, "go\t%s\n", info.Go)
	fmt.Fprintf(tw, "platform\t%s\n", info.Platform)
	fmt.Fprintf(tw, "vm images\t%s\n", info.ImageConstraint)
	return tw.Flush()
}

// printJSON writes the identity as an indented JSON object. HTML escaping is
// off so the image constraint's comparison operators survive as `<` and `>`
// instead of turning into unicode escapes.
func printJSON(w io.Writer, info Info) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(info); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}
