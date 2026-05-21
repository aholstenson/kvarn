package repocontext

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"gopkg.in/yaml.v3"
)

// RepoContext holds project-specific instructions and skills discovered from
// the repository root.
type RepoContext struct {
	Instructions  string   // merged AGENTS.md / CLAUDE.md content
	Skills        []Skill  // discovered skills
	RecentCommits []string // recent commit subject lines for convention matching
}

// Skill represents a single skill discovered via the agentskills.io spec.
type Skill struct {
	Name        string   // from YAML frontmatter (required)
	Description string   // from YAML frontmatter (required)
	Body        string   // markdown body after frontmatter
	Dir         string   // relative path to skill directory
	Resources   []string // relative paths to bundled resource files
}

// skillFrontmatter is the YAML structure parsed from SKILL.md frontmatter.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Load discovers instruction files and skills from the given repository root.
func Load(dir string) (*RepoContext, error) {
	rc := &RepoContext{}

	instructions, err := loadInstructions(dir)
	if err != nil {
		return nil, fmt.Errorf("load instructions: %w", err)
	}
	rc.Instructions = instructions

	skills, err := discoverSkills(dir)
	if err != nil {
		return nil, fmt.Errorf("discover skills: %w", err)
	}
	rc.Skills = skills

	commits, err := loadRecentCommits(dir, 20)
	if err != nil {
		slog.Warn("failed to load recent commits", "error", err)
	}
	rc.RecentCommits = commits

	return rc, nil
}

// loadInstructions reads AGENTS.md and/or CLAUDE.md from the repo root,
// deduplicating when they point to the same file.
func loadInstructions(dir string) (string, error) {
	agentsPath := filepath.Join(dir, "AGENTS.md")
	claudePath := filepath.Join(dir, "CLAUDE.md")

	agentsInfo, agentsErr := os.Stat(agentsPath)
	claudeInfo, claudeErr := os.Stat(claudePath)

	agentsExists := agentsErr == nil
	claudeExists := claudeErr == nil

	if !agentsExists && !claudeExists {
		slog.Debug("no instruction files found")
		return "", nil
	}

	// If both exist, check if they're the same file (symlink/hardlink).
	if agentsExists && claudeExists && os.SameFile(agentsInfo, claudeInfo) {
		slog.Debug("found AGENTS.md and CLAUDE.md pointing to same file")
		content, err := os.ReadFile(agentsPath)
		if err != nil {
			return "", err
		}
		return string(content), nil
	}

	var parts []string
	if agentsExists {
		slog.Debug("found AGENTS.md")
		content, err := os.ReadFile(agentsPath)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(content))
	}
	if claudeExists {
		slog.Debug("found CLAUDE.md")
		content, err := os.ReadFile(claudePath)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(content))
	}

	return strings.Join(parts, "\n---\n"), nil
}

// loadRecentCommits reads the last n commit subject lines from the repository.
func loadRecentCommits(dir string, n int) ([]string, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}

	iter, err := repo.Log(&gogit.LogOptions{
		From: head.Hash(),
	})
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	defer iter.Close()

	var subjects []string
	err = iter.ForEach(func(c *object.Commit) error {
		if len(subjects) >= n {
			return fmt.Errorf("done")
		}
		// Take the first line of the commit message as the subject.
		msg := c.Message
		if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
			msg = msg[:idx]
		}
		msg = strings.TrimSpace(msg)
		if msg != "" {
			subjects = append(subjects, msg)
		}
		return nil
	})
	// The "done" error is our early-exit signal.
	if err != nil && err.Error() != "done" {
		return nil, fmt.Errorf("iterate commits: %w", err)
	}

	return subjects, nil
}

// discoverSkills scans .claude/skills/ and .agents/skills/ for skill directories.
func discoverSkills(dir string) ([]Skill, error) {
	scanPaths := []string{
		filepath.Join(dir, ".claude", "skills"),
		filepath.Join(dir, ".agents", "skills"),
	}

	// Deduplicate scan paths that resolve to the same directory.
	type scanDir struct {
		path string
		info fs.FileInfo
	}
	var uniqueDirs []scanDir
	for _, p := range scanPaths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}

		duplicate := false
		for _, u := range uniqueDirs {
			if os.SameFile(u.info, info) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			uniqueDirs = append(uniqueDirs, scanDir{path: p, info: info})
		}
	}

	seen := make(map[string]bool)
	var skills []Skill

	for _, sd := range uniqueDirs {
		entries, err := os.ReadDir(sd.path)
		if err != nil {
			slog.Warn("failed to read skills directory", "path", sd.path, "error", err)
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			skillDir := filepath.Join(sd.path, entry.Name())
			skillFile := filepath.Join(skillDir, "SKILL.md")

			if _, err := os.Stat(skillFile); err != nil {
				continue
			}

			content, err := os.ReadFile(skillFile)
			if err != nil {
				slog.Warn("failed to read SKILL.md", "path", skillFile, "error", err)
				continue
			}

			fm, body, err := parseFrontmatter(content)
			if err != nil {
				slog.Error("failed to parse SKILL.md frontmatter", "path", skillFile, "error", err)
				continue
			}

			if fm.Description == "" {
				slog.Warn("skipping skill with missing description", "path", skillFile)
				continue
			}

			name := fm.Name
			if name == "" {
				name = entry.Name()
			} else if name != entry.Name() {
				slog.Warn("skill name does not match directory name", "name", name, "dir", entry.Name())
			}

			if seen[name] {
				slog.Warn("duplicate skill name, using first found", "name", name)
				continue
			}
			seen[name] = true

			relDir, _ := filepath.Rel(dir, skillDir)

			skill := Skill{
				Name:        name,
				Description: fm.Description,
				Body:        body,
				Dir:         relDir,
				Resources:   enumerateResources(skillDir),
			}
			skills = append(skills, skill)
			slog.Debug("loaded skill", "name", name, "dir", relDir)
		}
	}

	return skills, nil
}

// parseFrontmatter extracts YAML frontmatter and body from SKILL.md content.
func parseFrontmatter(content []byte) (*skillFrontmatter, string, error) {
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		// No frontmatter, treat entire content as body.
		return &skillFrontmatter{}, string(content), nil
	}

	// Find the end of the opening ---.
	rest := trimmed[3:]
	idx := bytes.Index(rest, []byte("\n"))
	if idx < 0 {
		return &skillFrontmatter{}, string(content), nil
	}
	rest = rest[idx+1:]

	// Find closing ---.
	closeIdx := bytes.Index(rest, []byte("\n---"))
	if closeIdx < 0 {
		return &skillFrontmatter{}, string(content), nil
	}

	yamlContent := rest[:closeIdx]
	body := string(rest[closeIdx+4:])
	body = strings.TrimLeft(body, "\r\n")

	var fm skillFrontmatter
	if err := yaml.Unmarshal(yamlContent, &fm); err != nil {
		// Attempt fallback: wrap values in quotes for unquoted colons.
		fixed := fixUnquotedColons(string(yamlContent))
		if err2 := yaml.Unmarshal([]byte(fixed), &fm); err2 != nil {
			return nil, "", fmt.Errorf("parse YAML frontmatter: %w", err)
		}
	}

	return &fm, body, nil
}

// fixUnquotedColons wraps YAML values containing colons in double quotes.
func fixUnquotedColons(yamlStr string) string {
	var lines []string
	for _, line := range strings.Split(yamlStr, "\n") {
		colonIdx := strings.Index(line, ":")
		if colonIdx >= 0 {
			key := line[:colonIdx]
			value := strings.TrimSpace(line[colonIdx+1:])
			if value != "" && !strings.HasPrefix(value, "\"") && !strings.HasPrefix(value, "'") && strings.Contains(value, ":") {
				value = `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
				line = key + ": " + value
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// enumerateResources lists files in scripts/, references/, and assets/ subdirectories.
func enumerateResources(skillDir string) []string {
	resourceDirs := []string{"scripts", "references", "assets"}
	var resources []string

	for _, sub := range resourceDirs {
		subDir := filepath.Join(skillDir, sub)
		entries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			resources = append(resources, filepath.Join(sub, entry.Name()))
		}
	}

	return resources
}
