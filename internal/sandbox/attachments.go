package sandbox

import (
	"context"
	"fmt"
	"path"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/attachment"
)

// The directories a run has in the guest beside its workspace. Both are
// outside the worktree on purpose: change extraction stages the whole worktree
// with `git add -A`, so a file put inside it would be committed. They are
// under the kvarn home directory so the runner gives them to the kvarn user,
// like every other path it writes for a job.
const (
	// AttachmentsDir holds the files the submitter sent with the job. A task
	// that wants one in the repository copies it in, and the copy is then an
	// ordinary change.
	AttachmentsDir = "/home/kvarn/attachments"
	// ScratchDir is for throwaway output: a converted file, a rendered image,
	// a log to read back. Nothing written there becomes part of the change.
	ScratchDir = "/home/kvarn/scratch"
)

// The environment variables that name the directories above, so setup and
// validation commands can find them without hard-coding a path.
const (
	AttachmentsDirEnv = "KVARN_ATTACHMENTS_DIR"
	ScratchDirEnv     = "KVARN_SCRATCH_DIR"
)

// AttachmentPath returns where an attachment of the given name is written in
// the guest.
func AttachmentPath(name string) string {
	return path.Join(AttachmentsDir, name)
}

// attachmentFileMode makes an attachment read-only. The agent is not meant to
// change what it was sent, and a file that refuses a write says so at the
// point of the mistake.
const attachmentFileMode = 0o444

// prepareRunDirs creates the attachments and scratch directories and writes
// the attachments into the first. The directories are made as the kvarn user,
// so they belong to it from the start.
//
// Each attachment goes in its own upload. One file is at most
// attachment.MaxFileBytes, which bounds every message on the bridge; a single
// upload of all of them could be as large as attachment.MaxTotalBytes.
func prepareRunDirs(ctx context.Context, runner RunnerProxy, files []attachment.File) error {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command: "mkdir",
		Args:    []string{"-p", "--", AttachmentsDir, ScratchDir},
	})
	if err != nil {
		return fmt.Errorf("create run directories: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("create run directories (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	for _, f := range files {
		if _, err := runner.UploadFiles(ctx, &v1.UploadFilesRequest{
			WorkingDir: AttachmentsDir,
			Files: []*v1.FileContent{{
				Path:    f.Name,
				Content: f.Data,
				Mode:    attachmentFileMode,
			}},
		}); err != nil {
			return fmt.Errorf("write attachment %q: %w", f.Name, err)
		}
	}
	return nil
}

// runDirEnv returns the environment that names the run directories.
func runDirEnv() map[string]string {
	return map[string]string{
		AttachmentsDirEnv: AttachmentsDir,
		ScratchDirEnv:     ScratchDir,
	}
}
