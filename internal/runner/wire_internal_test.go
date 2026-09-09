package runner

import (
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Command output is bytes, and protobuf string fields are not: a message that
// carries a byte no encoding describes cannot be encoded at all. These pin the
// boundary that keeps such output deliverable.
var _ = Describe("wire sanitizing", func() {
	const invalid = "warning: \xb1 is not a character\n"

	It("makes a result with invalid UTF-8 output encodable", func() {
		result := &v1.CommandResult{
			CommandId: "cmd-1",
			Result: &v1.CommandResult_SessionExec{SessionExec: &v1.SessionExecResponse{
				Stdout: invalid,
				Stderr: "\xff\xfe",
			}},
		}

		// The premise: without the boundary the report fails.
		_, err := proto.Marshal(result)
		Expect(err).To(HaveOccurred())

		sanitizeForWire(result)

		_, err = proto.Marshal(result)
		Expect(err).NotTo(HaveOccurred())
		exec := result.GetSessionExec()
		Expect(exec.Stdout).To(Equal("warning: � is not a character\n"))
		Expect(exec.Stderr).To(Equal("�"))
	})

	It("reaches strings in repeated nested messages", func() {
		result := &v1.CommandResult{
			CommandId: "cmd-2",
			Result: &v1.CommandResult_EditFile{EditFile: &v1.EditFileResponse{
				Context: []*v1.TaggedLine{
					{Line: 1, Hash: "calfskin", Content: "fine"},
					{Line: 2, Hash: "marble", Content: "bad \xb1"},
				},
			}},
		}

		sanitizeForWire(result)

		_, err := proto.Marshal(result)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.GetEditFile().Context[1].Content).To(Equal("bad �"))
		Expect(result.GetEditFile().Context[0].Content).To(Equal("fine"))
	})

	It("reaches repeated strings", func() {
		req := &v1.EditOperation{Lines: []string{"ok", "\xb1"}}

		sanitizeForWire(req)

		Expect(req.Lines).To(Equal([]string{"ok", "�"}))
	})

	It("leaves valid text untouched", func() {
		result := &v1.CommandResult{
			CommandId: "cmd-3",
			Error:     "händelse: 日本語 ✓",
		}

		sanitizeForWire(result)

		Expect(result.Error).To(Equal("händelse: 日本語 ✓"))
	})
})

var _ = Describe("incompleteRuneTail", func() {
	It("reports nothing for ASCII and complete characters", func() {
		Expect(incompleteRuneTail([]byte("hello"))).To(Equal(0))
		Expect(incompleteRuneTail([]byte("héllo"))).To(Equal(0))
		Expect(incompleteRuneTail([]byte("日本語"))).To(Equal(0))
		Expect(incompleteRuneTail([]byte("😀"))).To(Equal(0))
		Expect(incompleteRuneTail(nil)).To(Equal(0))
	})

	It("counts the bytes of an unfinished character at the end", func() {
		e := []byte("é")   // 2 bytes
		ja := []byte("日")  // 3 bytes
		emo := []byte("😀") // 4 bytes

		Expect(incompleteRuneTail(append([]byte("x"), e[:1]...))).To(Equal(1))
		Expect(incompleteRuneTail(append([]byte("x"), ja[:1]...))).To(Equal(1))
		Expect(incompleteRuneTail(append([]byte("x"), ja[:2]...))).To(Equal(2))
		Expect(incompleteRuneTail(append([]byte("x"), emo[:1]...))).To(Equal(1))
		Expect(incompleteRuneTail(append([]byte("x"), emo[:2]...))).To(Equal(2))
		Expect(incompleteRuneTail(append([]byte("x"), emo[:3]...))).To(Equal(3))
	})

	It("leaves bytes that can never complete for the replacement", func() {
		// A lone continuation byte, and an invalid start byte, are not the
		// beginning of anything.
		Expect(incompleteRuneTail([]byte{'x', 0x80})).To(Equal(0))
		Expect(incompleteRuneTail([]byte{'x', 0xb1})).To(Equal(0))
		Expect(incompleteRuneTail([]byte{'x', 0xff})).To(Equal(0))
	})
})
