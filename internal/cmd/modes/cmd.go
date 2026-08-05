// Package modes implements the `kvarn modes` CLI: discovering which agent
// modes a job can be started in. It reads no orchestrator — the built-in modes
// are compiled in, and a repository's own modes live in its kvarn.yml, which is
// on disk wherever the repository is checked out.
package modes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// Cmd is the parent command for `kvarn modes <subcommand>`.
type Cmd struct {
	List ListCmd `cmd:"" help:"List the modes a job can be started in."`
}

// ListCmd shows the built-in modes plus whatever the repository in --dir
// defines, resolved the same way a run resolves them.
type ListCmd struct {
	Dir  string `help:"Repository whose kvarn.yml is read for project-defined modes." default:"." type:"existingdir"`
	JSON bool   `help:"Emit JSON instead of a table." name:"json"`
}

// modeRow is one line of the listing.
type modeRow struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description"`
	Workspace   string   `json:"workspace"`
	Validation  string   `json:"validation"`
	Deliver     []string `json:"deliver"`
	Start       string   `json:"start"`
}

func (c *ListCmd) Run() error {
	cfg, err := projconfig.Load(c.Dir)
	if err != nil {
		abs, absErr := filepath.Abs(c.Dir)
		if absErr != nil {
			abs = c.Dir
		}
		return fmt.Errorf("read modes from %s: %w", abs, err)
	}

	specs := make(map[string]coding.Spec)
	if cfg != nil {
		for name, m := range cfg.Modes {
			spec := coding.Spec{
				Name:        name,
				Description: m.Description,
				Extends:     m.Extends,
				Prompt:      m.Prompt,
				Workspace:   coding.Workspace(m.Workspace),
				Validation:  coding.ValidationPolicy(m.Validation),
				Start:       coding.StartPoint(m.Start),
			}
			for _, sink := range m.Deliver {
				spec.Deliver = append(spec.Deliver, coding.Sink(sink))
			}
			for _, block := range m.Context {
				spec.Context = append(spec.Context, coding.ContextBlock(block))
			}
			specs[name] = spec
		}
	}

	reg, err := coding.Merge(specs)
	if err != nil {
		return err
	}

	rows := make([]modeRow, 0, len(reg))
	for _, name := range reg.Names() {
		m := reg[name]
		source := "project"
		if coding.IsBuiltin(name) {
			source = "built-in"
		}
		deliver := make([]string, 0, len(m.Deliver))
		for _, sink := range m.Deliver {
			deliver = append(deliver, string(sink))
		}
		rows = append(rows, modeRow{
			Name:        m.Name,
			Source:      source,
			Description: m.Description,
			Workspace:   string(m.Workspace),
			Validation:  string(m.Validation),
			Deliver:     deliver,
			Start:       string(m.Start),
		})
	}
	// Built-ins first, then the repository's own, each alphabetically: the
	// listing is read to answer "what can I pass to --mode", and the modes this
	// repository added are the interesting half.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Source != rows[j].Source {
			return rows[i].Source == "built-in"
		}
		return rows[i].Name < rows[j].Name
	})

	if c.JSON {
		return printJSON(rows)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tWORKSPACE\tVALIDATION\tSTART\tDELIVER\tDESCRIPTION")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Source, r.Workspace, r.Validation, r.Start,
			strings.Join(r.Deliver, ","), dash(r.Description))
	}
	return w.Flush()
}

// printJSON writes the listing as an indented JSON array.
func printJSON(rows []modeRow) error {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(b))
	return nil
}

// dash renders an empty optional field so columns stay aligned.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
