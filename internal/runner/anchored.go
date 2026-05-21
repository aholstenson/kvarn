package runner

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unicode/utf8"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// defaultContextLines is the number of fresh tagged lines returned around each
// edit when the request doesn't specify a value.
const defaultContextLines = 5

// AnchoredErrorCode classifies failures from the anchored file editing API.
// The code travels with the error so the agent-side wrapper can render a
// useful recovery hint without parsing the message.
type AnchoredErrorCode int

const (
	ErrVersionConflict AnchoredErrorCode = iota + 1
	ErrAnchorMismatch
	ErrInvalidOperation
	ErrPathEscape
	ErrMixedNewline
	ErrFileEncoding
)

// AnchoredError carries a structured failure from the anchored file editing
// API. It serializes to a single-line connect error so the agent-side wrapper
// can parse the code and detail and produce a recovery hint.
type AnchoredError struct {
	Code   AnchoredErrorCode
	Detail string
	// Snapshot is a freshly-tagged read attached to version/anchor mismatches
	// so the model can re-anchor in one round-trip.
	Snapshot *v1.ReadFileResponse
}

func (e *AnchoredError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code.String(), e.Detail)
}

func (c AnchoredErrorCode) String() string {
	switch c {
	case ErrVersionConflict:
		return "version_conflict"
	case ErrAnchorMismatch:
		return "anchor_mismatch"
	case ErrInvalidOperation:
		return "invalid_operation"
	case ErrPathEscape:
		return "path_escape"
	case ErrMixedNewline:
		return "mixed_newline"
	case ErrFileEncoding:
		return "file_encoding"
	default:
		return "unknown"
	}
}

// toConnectError marshals an AnchoredError to a connect.Error with an
// appropriate code. The snapshot is attached as a single error detail so the
// agent can recover in one round-trip.
func (e *AnchoredError) toConnectError() error {
	var code connect.Code
	switch e.Code {
	case ErrVersionConflict, ErrAnchorMismatch:
		code = connect.CodeFailedPrecondition
	default:
		code = connect.CodeInvalidArgument
	}
	cerr := connect.NewError(code, e)
	if e.Snapshot != nil {
		if d, derr := connect.NewErrorDetail(e.Snapshot); derr == nil {
			cerr.AddDetail(d)
		}
	}
	return cerr
}

// hashFile returns the hex-encoded FNV-1a 64-bit digest of content, used as
// the full-file version handed back to clients as opaque proof-of-state. Not
// cryptographic — just an optimistic-concurrency token within a session.
func hashFile(content []byte) string {
	h := fnv.New64a()
	h.Write(content)
	return fmt.Sprintf("%016x", h.Sum64())
}

// hashLinePrefix returns the first prefixLen hex chars of FNV-1a 32-bit over
// line. Callers want a prefix to fit in 2-3 characters in the common case;
// the maximum useful length is 8 (full 32-bit hash).
func hashLinePrefix(line []byte, prefixLen int) string {
	h := fnv.New32a()
	h.Write(line)
	full := fmt.Sprintf("%08x", h.Sum32())
	if prefixLen > len(full) {
		prefixLen = len(full)
	}
	return full[:prefixLen]
}

// tagLines builds TaggedLine entries for the supplied windowed lines. Each
// TaggedLine carries the 1-indexed line number from windowStart. The prefix
// length adapts: start at 2, extend up to 8 if two distinct lines collide.
func tagLines(allLines [][]byte, windowStart int, windowLines [][]byte) ([]*v1.TaggedLine, int) {
	prefixLen := 2
	for ; prefixLen <= 8; prefixLen++ {
		if !hasPrefixCollision(allLines, prefixLen) {
			break
		}
	}
	out := make([]*v1.TaggedLine, len(windowLines))
	for i, line := range windowLines {
		out[i] = &v1.TaggedLine{
			Line:    int32(windowStart + i),
			Hash:    hashLinePrefix(line, prefixLen),
			Content: string(line),
		}
	}
	return out, prefixLen
}

// hasPrefixCollision reports whether two distinct lines in the slice share the
// same prefix hash at the given length.
func hasPrefixCollision(lines [][]byte, prefixLen int) bool {
	seen := make(map[string]int, len(lines))
	for i, line := range lines {
		h := hashLinePrefix(line, prefixLen)
		if prev, ok := seen[h]; ok {
			if string(lines[prev]) != string(line) {
				return true
			}
			continue
		}
		seen[h] = i
	}
	return false
}

// splitLines walks content and returns the lines (without their terminators),
// the newline style ("\n" or "\r\n"), and whether the file ended without a
// terminator. Mixed-newline files are rejected.
func splitLines(content []byte) (lines [][]byte, newline string, trailingNewline bool, err error) {
	if len(content) == 0 {
		return nil, "\n", false, nil
	}

	var saw, sawCRLF, sawLF bool
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			isCRLF := i > 0 && content[i-1] == '\r'
			if isCRLF {
				sawCRLF = true
				lines = append(lines, content[start:i-1])
			} else {
				sawLF = true
				lines = append(lines, content[start:i])
			}
			saw = true
			start = i + 1
		}
	}

	if sawCRLF && sawLF {
		return nil, "", false, &AnchoredError{Code: ErrMixedNewline, Detail: "file mixes \\n and \\r\\n line endings"}
	}

	if start < len(content) {
		lines = append(lines, content[start:])
		trailingNewline = false
	} else {
		trailingNewline = true
	}

	switch {
	case sawCRLF:
		newline = "\r\n"
	case saw:
		newline = "\n"
	default:
		newline = "\n"
	}
	return lines, newline, trailingNewline, nil
}

// joinLines reassembles content using the given newline. If trailingNewline is
// true the result ends with a terminator; otherwise the last line has no
// terminator (preserves files originally written without a trailing newline).
func joinLines(lines [][]byte, newline string, trailingNewline bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	total := 0
	for _, l := range lines {
		total += len(l) + len(newline)
	}
	out := make([]byte, 0, total)
	for i, l := range lines {
		out = append(out, l...)
		if i < len(lines)-1 || trailingNewline {
			out = append(out, newline...)
		}
	}
	return out
}

// writeFileAtomic writes content to path via a temp-file + rename, so an
// interrupted write never leaves the target half-overwritten.
func writeFileAtomic(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kvarn-edit-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// pathMutex returns the per-path mutex used to serialize read-validate-write
// sequences. Lookup is keyed by the resolved absolute path.
var pathMutexes sync.Map

func pathMutex(resolved string) *sync.Mutex {
	if m, ok := pathMutexes.Load(resolved); ok {
		return m.(*sync.Mutex)
	}
	m, _ := pathMutexes.LoadOrStore(resolved, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// validateFileContent runs UTF-8 and mixed-newline checks before tagging or
// editing, since the anchor scheme assumes both invariants.
func validateFileContent(content []byte) error {
	if !utf8.Valid(content) {
		return &AnchoredError{Code: ErrFileEncoding, Detail: "file is not valid UTF-8"}
	}
	return nil
}

// requestPrefixLen returns the longest hash length supplied in any operation.
// Anchors are validated against current-file prefixes at this length.
func requestPrefixLen(ops []*v1.EditOperation) int {
	max := 0
	for _, op := range ops {
		for _, h := range []string{op.Hash, op.StartHash, op.EndHash} {
			if len(h) > max {
				max = len(h)
			}
		}
	}
	if max == 0 {
		max = 2
	}
	return max
}

// editInterval describes the span a single op occupies in the original line
// numbering. INSERT_AFTER occupies the seam between N and N+1; we model it as
// a half-open zero-width interval [N+1, N+1) so adjacent inserts don't overlap.
type editInterval struct {
	start, end float64
	opIndex    int
}

// buildIntervals returns sorted intervals per operation in the original line
// numbering. An INSERT_AFTER op at line N becomes [N+0.5, N+0.5).
func buildIntervals(ops []*v1.EditOperation) ([]editInterval, error) {
	intervals := make([]editInterval, 0, len(ops))
	for i, op := range ops {
		switch op.Op {
		case v1.EditOp_EDIT_OP_REPLACE, v1.EditOp_EDIT_OP_DELETE:
			intervals = append(intervals, editInterval{start: float64(op.Line), end: float64(op.Line) + 0.001, opIndex: i})
		case v1.EditOp_EDIT_OP_REPLACE_RANGE, v1.EditOp_EDIT_OP_DELETE_RANGE:
			intervals = append(intervals, editInterval{start: float64(op.StartLine), end: float64(op.EndLine) + 0.001, opIndex: i})
		case v1.EditOp_EDIT_OP_INSERT_AFTER:
			seam := float64(op.Line) + 0.5
			intervals = append(intervals, editInterval{start: seam, end: seam, opIndex: i})
		default:
			return nil, &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d has unspecified op code", i)}
		}
	}
	sorted := make([]editInterval, len(intervals))
	copy(sorted, intervals)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].end < sorted[j].end
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].start < sorted[i-1].end {
			return nil, &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d overlaps operation %d", sorted[i].opIndex, sorted[i-1].opIndex)}
		}
	}
	return intervals, nil
}

// validateOpBounds enforces that the line numbers referenced by op are within
// [1, totalLines] and ranges are well-formed.
func validateOpBounds(op *v1.EditOperation, opIndex, totalLines int) error {
	switch op.Op {
	case v1.EditOp_EDIT_OP_REPLACE, v1.EditOp_EDIT_OP_DELETE:
		if op.Line < 1 || int(op.Line) > totalLines {
			return &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d line %d out of range [1, %d]", opIndex, op.Line, totalLines)}
		}
	case v1.EditOp_EDIT_OP_REPLACE_RANGE, v1.EditOp_EDIT_OP_DELETE_RANGE:
		if op.StartLine < 1 || op.EndLine < 1 || int(op.StartLine) > totalLines || int(op.EndLine) > totalLines {
			return &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d range [%d, %d] out of bounds [1, %d]", opIndex, op.StartLine, op.EndLine, totalLines)}
		}
		if op.StartLine > op.EndLine {
			return &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d has start_line > end_line", opIndex)}
		}
	case v1.EditOp_EDIT_OP_INSERT_AFTER:
		// 0 means insert at top of file; otherwise must be within bounds.
		if op.Line < 0 || int(op.Line) > totalLines {
			return &AnchoredError{Code: ErrInvalidOperation, Detail: fmt.Sprintf("operation %d insert_after line %d out of range [0, %d]", opIndex, op.Line, totalLines)}
		}
	}
	return nil
}

// contextWindow returns a freshly-tagged slice of lines around an anchor. The
// caller passes the original anchor line; the window is [anchor-radius, anchor+radius]
// clamped to [1, totalLines]. Returned tags reference current file lines.
func contextWindow(allLines [][]byte, anchorLine, radius int) []*v1.TaggedLine {
	if len(allLines) == 0 {
		return nil
	}
	start := anchorLine - radius
	if start < 1 {
		start = 1
	}
	end := anchorLine + radius
	if end > len(allLines) {
		end = len(allLines)
	}
	if start > end {
		return nil
	}
	window := allLines[start-1 : end]
	tags, _ := tagLines(allLines, start, window)
	return tags
}
