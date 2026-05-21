package transfer

import (
	"context"
	"io"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// FileUploader is the subset of RunnerProxy needed for batch file transfer.
type FileUploader interface {
	UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error)
}

// Uploader extends FileUploader with streaming and exec capabilities.
type Uploader interface {
	FileUploader
	StreamToGuest(ctx context.Context, destPath string, src io.Reader, size int64) error
	Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error)
}

// Transferer uploads local files to a remote VM via an Uploader.
type Transferer interface {
	Upload(ctx context.Context, uploader Uploader, localDir string, remoteDir string) error
}
