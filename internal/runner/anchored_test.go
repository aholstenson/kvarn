package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/runner"
)

var _ = Describe("Anchored editing", func() {
	var (
		h       *runner.Handler
		workDir string
		ctx     context.Context
	)

	BeforeEach(func() {
		h = runner.NewUnprivilegedHandler()
		var err error
		workDir, err = os.MkdirTemp("", "anchored-test-*")
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
	})

	AfterEach(func() {
		os.RemoveAll(workDir)
	})

	writeFile := func(name, body string) {
		Expect(os.WriteFile(filepath.Join(workDir, name), []byte(body), 0o644)).To(Succeed())
	}
	readFile := func(name string) string {
		b, err := os.ReadFile(filepath.Join(workDir, name))
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}
	doRead := func(name string) *v1.ReadFileResponse {
		resp, err := h.ReadFile(ctx, connect.NewRequest(&v1.ReadFileRequest{
			WorkingDir: workDir, Path: name,
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}
	doEdit := func(name, version string, ops []*v1.EditOperation) (*v1.EditFileResponse, error) {
		resp, err := h.EditFile(ctx, connect.NewRequest(&v1.EditFileRequest{
			WorkingDir:      workDir,
			Path:            name,
			ExpectedVersion: version,
			Operations:      ops,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	It("applies a single REPLACE op", func() {
		writeFile("f.txt", "alpha\nbeta\ngamma\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 2, Hash: r.Lines[1].Hash, Lines: []string{"BETA"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("alpha\nBETA\ngamma\n"))
	})

	It("applies multiple ops listed in ascending order", func() {
		writeFile("f.txt", "1\n2\n3\n4\n5\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: r.Lines[0].Hash, Lines: []string{"one"}},
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 3, Hash: r.Lines[2].Hash, Lines: []string{"three"}},
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 5, Hash: r.Lines[4].Hash, Lines: []string{"five"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("one\n2\nthree\n4\nfive\n"))
	})

	It("inserts after the last line", func() {
		writeFile("f.txt", "a\nb\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_INSERT_AFTER, Line: 2, Hash: r.Lines[1].Hash, Lines: []string{"c", "d"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("a\nb\nc\nd\n"))
	})

	It("replaces a range collapsing multiple lines to one", func() {
		writeFile("f.txt", "a\nb\nc\nd\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{
				Op:        v1.EditOp_EDIT_OP_REPLACE_RANGE,
				StartLine: 2, StartHash: r.Lines[1].Hash,
				EndLine: 3, EndHash: r.Lines[2].Hash,
				Lines: []string{"X"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("a\nX\nd\n"))
	})

	It("deletes a single line", func() {
		writeFile("f.txt", "a\nb\nc\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_DELETE, Line: 2, Hash: r.Lines[1].Hash},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("a\nc\n"))
	})

	It("deletes a range", func() {
		writeFile("f.txt", "a\nb\nc\nd\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{
				Op:        v1.EditOp_EDIT_OP_DELETE_RANGE,
				StartLine: 2, StartHash: r.Lines[1].Hash,
				EndLine: 3, EndHash: r.Lines[2].Hash,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("a\nd\n"))
	})

	It("rejects edits when expected_version is stale", func() {
		writeFile("f.txt", "a\nb\n")
		r := doRead("f.txt")
		Expect(os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("a\nbb\n"), 0o644)).To(Succeed())

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: r.Lines[0].Hash, Lines: []string{"A"}},
		})
		Expect(err).To(HaveOccurred())
		cerr := new(connect.Error)
		Expect(asConnectError(err, cerr)).To(BeTrue())
		Expect(cerr.Code()).To(Equal(connect.CodeFailedPrecondition))

		// Snapshot attached so the agent can re-anchor.
		details := cerr.Details()
		Expect(details).NotTo(BeEmpty())
		val, dErr := details[0].Value()
		Expect(dErr).NotTo(HaveOccurred())
		snap, ok := val.(*v1.ReadFileResponse)
		Expect(ok).To(BeTrue())
		Expect(snap.Lines).NotTo(BeEmpty())
	})

	It("rejects edits with anchor mismatch", func() {
		writeFile("f.txt", "a\nb\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: "zz", Lines: []string{"A"}},
		})
		Expect(err).To(HaveOccurred())
		cerr := new(connect.Error)
		Expect(asConnectError(err, cerr)).To(BeTrue())
		Expect(cerr.Code()).To(Equal(connect.CodeFailedPrecondition))
		Expect(cerr.Message()).To(ContainSubstring("anchor_mismatch"))
	})

	It("rejects overlapping ops", func() {
		writeFile("f.txt", "a\nb\nc\n")
		r := doRead("f.txt")

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 2, Hash: r.Lines[1].Hash, Lines: []string{"X"}},
			{Op: v1.EditOp_EDIT_OP_REPLACE_RANGE, StartLine: 1, StartHash: r.Lines[0].Hash, EndLine: 2, EndHash: r.Lines[1].Hash, Lines: []string{"Y"}},
		})
		Expect(err).To(HaveOccurred())
	})

	It("preserves CRLF newlines", func() {
		writeFile("f.txt", "alpha\r\nbeta\r\n")
		r := doRead("f.txt")
		Expect(r.Newline).To(Equal("\r\n"))

		_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
			{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: r.Lines[0].Hash, Lines: []string{"ALPHA"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(readFile("f.txt")).To(Equal("ALPHA\r\nbeta\r\n"))
	})

	It("rejects mixed-newline files", func() {
		writeFile("f.txt", "a\r\nb\nc\r\n")
		_, err := h.ReadFile(ctx, connect.NewRequest(&v1.ReadFileRequest{
			WorkingDir: workDir, Path: "f.txt",
		}))
		Expect(err).To(HaveOccurred())
	})

	It("hands out distinct hash prefixes for distinct lines", func() {
		// Build a file with enough distinct short lines that prefix collisions
		// are plausible — the runner must auto-extend the prefix length so all
		// distinct lines get distinct anchors.
		var body string
		for i := 0; i < 64; i++ {
			body += string(rune('a'+(i%26))) + string(rune('A'+(i%26))) + "\n"
		}
		writeFile("f.txt", body)
		r := doRead("f.txt")
		seen := make(map[string]string)
		for _, l := range r.Lines {
			if prev, ok := seen[l.Hash]; ok {
				Expect(prev).To(Equal(l.Content), "distinct lines must not share a hash prefix")
			}
			seen[l.Hash] = l.Content
		}
	})

	It("returns full-file version with a windowed read", func() {
		writeFile("f.txt", "a\nb\nc\nd\ne\n")
		full := doRead("f.txt")

		resp, err := h.ReadFile(ctx, connect.NewRequest(&v1.ReadFileRequest{
			WorkingDir: workDir,
			Path:       "f.txt",
			StartLine:  2,
			EndLine:    4,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Version).To(Equal(full.Version))
		Expect(resp.Msg.TotalLines).To(Equal(int32(5)))
		Expect(resp.Msg.Lines).To(HaveLen(3))
		Expect(resp.Msg.Lines[0].Line).To(Equal(int32(2)))
		Expect(resp.Msg.Lines[0].Content).To(Equal("b"))
	})

	It("serializes concurrent edits to the same path", func() {
		writeFile("f.txt", "a\nb\n")
		r := doRead("f.txt")

		var wg sync.WaitGroup
		results := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, err := doEdit("f.txt", r.Version, []*v1.EditOperation{
					{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: r.Lines[0].Hash, Lines: []string{"X"}},
				})
				results[idx] = err
			}(i)
		}
		wg.Wait()

		// Exactly one should succeed; the other should see the new version.
		var oks, fails int
		for _, e := range results {
			if e == nil {
				oks++
			} else {
				fails++
			}
		}
		Expect(oks).To(Equal(1))
		Expect(fails).To(Equal(1))
	})
})

// asConnectError walks err looking for a *connect.Error and copies it into dst.
func asConnectError(err error, dst *connect.Error) bool {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		*dst = *cerr
		return true
	}
	return false
}
