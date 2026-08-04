package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// minGitMajor / minGitMinor is the oldest git kvarn runs against.
//
// 2.26 made wire protocol v2 the default, which is what keeps the mirror's
// per-job `ls-remote` a single small round trip on a repository with thousands
// of refs. It also comfortably covers `--end-of-options` (2.24), which every
// caller-supplied operand depends on.
const (
	minGitMajor = 2
	minGitMinor = 26
)

var (
	gitOnce sync.Once
	gitBin  string
	gitErr  error
)

// gitPath resolves the git binary once per process.
func gitPath() (string, error) {
	gitOnce.Do(func() {
		bin, err := exec.LookPath("git")
		if err != nil {
			gitErr = fmt.Errorf("git was not found on PATH: %w", err)
			return
		}
		gitBin = bin
	})
	return gitBin, gitErr
}

// CheckAvailable reports whether the host has a git new enough to run kvarn.
// Call it at startup so an operator learns about a missing or ancient git from
// a clear message at boot rather than from a job failing to clone.
func CheckAvailable() error {
	bin, err := gitPath()
	if err != nil {
		return err
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("run %s --version: %w", bin, err)
	}
	version := strings.TrimSpace(string(out))
	major, minor, ok := parseGitVersion(version)
	if !ok {
		// An unparseable version is not worth refusing to start over: the
		// binary exists and is most likely a vendor build with an odd string.
		return nil
	}
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Errorf("git %d.%d or newer is required, found %q at %s",
			minGitMajor, minGitMinor, version, bin)
	}
	return nil
}

// parseGitVersion pulls the major and minor out of a `git --version` line such
// as "git version 2.43.0" or "git version 2.39.5 (Apple Git-154)".
func parseGitVersion(s string) (major, minor int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return 0, 0, false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// Cmd is one git command line, split so that Run can guarantee the
// option/operand boundary rather than leaving it to each call site.
//
// The split matters. Branch names and refs reach kvarn from project config
// and, on a feedback run, from whoever opened the pull request. A branch named
// "--upload-pack=/bin/sh" handed to clone or ls-remote as a bare argument is
// remote code execution on the orchestrator host. Everything caller-supplied
// therefore goes in Operands, which Run places after `--end-of-options` so git
// cannot read it as an option however it is spelled.
type Cmd struct {
	Dir      string   // -C <dir>; empty runs in the process working directory
	Config   []string // "key=value" pairs passed as -c; never written to any file
	Env      []string // extra environment on top of the inherited set
	Sub      string   // subcommand, e.g. "clone"
	Flags    []string // options and option values chosen by kvarn, not the caller
	Operands []string // caller-supplied values (refs, branches, URLs, paths)
	Stdin    string   // fed to the command's standard input
}

// Run executes one git command and returns its standard output. Errors carry
// the command's stderr, which is where git puts everything worth reading.
func Run(ctx context.Context, c Cmd) (string, error) {
	bin, err := gitPath()
	if err != nil {
		return "", err
	}

	args := make([]string, 0, 8+len(c.Config)*2+len(c.Flags)+len(c.Operands))
	if c.Dir != "" {
		args = append(args, "-C", c.Dir)
	}
	for _, cfg := range c.Config {
		args = append(args, "-c", cfg)
	}
	args = append(args, c.Sub)
	args = append(args, c.Flags...)
	if len(c.Operands) > 0 {
		args = append(args, "--end-of-options")
		args = append(args, c.Operands...)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(baseEnv(), c.Env...)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return stdout.String(), fmt.Errorf("git %s: %w: %s", c.Sub, err, msg)
		}
		return stdout.String(), fmt.Errorf("git %s: %w", c.Sub, err)
	}
	return stdout.String(), nil
}

// baseEnv is the environment every git invocation starts from. It keeps git
// from ever blocking on a human: a terminal prompt or a GUI askpass in a
// process nobody is watching would hang a job until its context expired.
func baseEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
}
