package sandbox

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/attachment"
)

// runDirProxy records the commands and uploads prepareRunDirs makes. The
// embedded interface is nil, so a call it does not expect panics.
type runDirProxy struct {
	RunnerProxy
	execs     []*v1.ExecRequest
	uploads   []*v1.UploadFilesRequest
	exitCode  int32
	uploadErr error
}

func (p *runDirProxy) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	p.execs = append(p.execs, req)
	return &v1.ExecResponse{ExitCode: p.exitCode, Stderr: "mkdir: denied"}, nil
}

func (p *runDirProxy) UploadFiles(_ context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	p.uploads = append(p.uploads, req)
	if p.uploadErr != nil {
		return nil, p.uploadErr
	}
	return &v1.UploadFilesResponse{FilesWritten: int32(len(req.Files))}, nil
}

var _ = Describe("prepareRunDirs", func() {
	It("creates both directories as the job's user, outside the workspace", func() {
		proxy := &runDirProxy{}
		Expect(prepareRunDirs(context.Background(), proxy, nil)).To(Succeed())

		Expect(proxy.execs).To(HaveLen(1))
		Expect(proxy.execs[0].Privileged).To(BeFalse())
		Expect(proxy.execs[0].Command).To(Equal("mkdir"))
		Expect(proxy.execs[0].Args).To(Equal([]string{"-p", "--", AttachmentsDir, ScratchDir}))
		Expect(proxy.uploads).To(BeEmpty())
	})

	It("writes each attachment read-only, in an upload of its own", func() {
		proxy := &runDirProxy{}
		files := []attachment.File{
			{Name: "spec.md", Data: []byte("spec")},
			{Name: "shot.png", Data: []byte("png")},
		}
		Expect(prepareRunDirs(context.Background(), proxy, files)).To(Succeed())

		Expect(proxy.uploads).To(HaveLen(2))
		for i, up := range proxy.uploads {
			Expect(up.WorkingDir).To(Equal(AttachmentsDir))
			Expect(up.Files).To(HaveLen(1))
			Expect(up.Files[0].Path).To(Equal(files[i].Name))
			Expect(up.Files[0].Content).To(Equal(files[i].Data))
			Expect(up.Files[0].Mode).To(Equal(uint32(0o444)))
		}
	})

	It("fails when the directories cannot be made", func() {
		proxy := &runDirProxy{exitCode: 1}
		err := prepareRunDirs(context.Background(), proxy, []attachment.File{{Name: "a", Data: []byte("x")}})
		Expect(err).To(MatchError(ContainSubstring("mkdir: denied")))
		Expect(proxy.uploads).To(BeEmpty())
	})

	It("names the attachment that could not be written", func() {
		proxy := &runDirProxy{uploadErr: errors.New("disk full")}
		err := prepareRunDirs(context.Background(), proxy, []attachment.File{{Name: "spec.md", Data: []byte("x")}})
		Expect(err).To(MatchError(ContainSubstring(`write attachment "spec.md": disk full`)))
	})
})

var _ = Describe("AttachmentPath", func() {
	It("places a file in the attachments directory", func() {
		Expect(AttachmentPath("spec.md")).To(Equal("/home/kvarn/attachments/spec.md"))
	})
})
