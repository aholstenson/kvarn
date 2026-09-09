package runner

import (
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// invalidByteReplacement stands in for each run of bytes that is not valid
// UTF-8 in text the guest produced. The replacement character is what every
// terminal and editor shows for the same bytes, so a reader sees the output the
// way they would have seen it locally, minus the exact byte values, which the
// exit code and the surrounding text are enough to explain.
const invalidByteReplacement = "�"

// validText makes s valid UTF-8, replacing each run of bytes that is not.
//
// Everything a command prints crosses the bridge in a protobuf string field,
// and protobuf refuses to encode a string that is not valid UTF-8. Output is
// bytes, not text: a test that feeds a program Latin-1, a compiler quoting a
// binary file, a tool printing a raw escape sequence all put bytes on stdout
// that no encoding describes. Left as they are, the message carrying them
// cannot be sent, and a result that cannot be reported is a command the host
// never hears back from.
func validText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, invalidByteReplacement)
}

// sanitizeForWire makes every string field in m, at any depth, valid UTF-8, so
// the message is guaranteed to encode. It is applied to each result and output
// chunk immediately before it is sent, rather than at the points that produce
// text, because those points are many — every handler that returns output, a
// file name, a working directory — and a new one added later must not be able
// to reintroduce a message the wire rejects.
//
// Repeated strings and map values are covered; map keys are not, because no
// message on the bridge carries guest text in a map key.
func sanitizeForWire(m proto.Message) {
	sanitizeMessage(m.ProtoReflect())
}

func sanitizeMessage(m protoreflect.Message) {
	// Fields are collected first: setting a field while ranging over the
	// message is not permitted.
	var dirty []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				switch fd.Kind() {
				case protoreflect.StringKind:
					if s := list.Get(i).String(); !utf8.ValidString(s) {
						list.Set(i, protoreflect.ValueOfString(validText(s)))
					}
				case protoreflect.MessageKind, protoreflect.GroupKind:
					sanitizeMessage(list.Get(i).Message())
				}
			}
		case fd.IsMap():
			mp := v.Map()
			switch fd.MapValue().Kind() {
			case protoreflect.StringKind:
				var fix []protoreflect.MapKey
				mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
					if !utf8.ValidString(mv.String()) {
						fix = append(fix, k)
					}
					return true
				})
				for _, k := range fix {
					mp.Set(k, protoreflect.ValueOfString(validText(mp.Get(k).String())))
				}
			case protoreflect.MessageKind, protoreflect.GroupKind:
				mp.Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					sanitizeMessage(mv.Message())
					return true
				})
			}
		case fd.Kind() == protoreflect.StringKind:
			if !utf8.ValidString(v.String()) {
				dirty = append(dirty, fd)
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sanitizeMessage(v.Message())
		}
		return true
	})
	for _, fd := range dirty {
		m.Set(fd, protoreflect.ValueOfString(validText(m.Get(fd).String())))
	}
}

// incompleteRuneTail reports how many bytes at the end of b begin a UTF-8
// sequence that b does not finish. Output is read from a growing file in
// whatever slices happen to be there, and a slice can end between the bytes of
// one character; those bytes belong with the next slice, not on the wire as a
// broken character in this one.
func incompleteRuneTail(b []byte) int {
	// A sequence is at most four bytes, so only the last three can be an
	// unfinished start.
	for back := 1; back <= 3 && back <= len(b); back++ {
		c := b[len(b)-back]
		if c < utf8.RuneSelf {
			// ASCII is complete on its own; nothing before it is pending.
			return 0
		}
		if c&0xC0 == 0x80 {
			// A continuation byte: keep looking for the start it belongs to.
			continue
		}
		// A start byte: pending only if the sequence it opens needs more bytes
		// than are here. An invalid start byte is left in place for the
		// replacement to cover.
		want := 0
		switch {
		case c&0xE0 == 0xC0:
			want = 2
		case c&0xF0 == 0xE0:
			want = 3
		case c&0xF8 == 0xF0:
			want = 4
		}
		if want > back {
			return back
		}
		return 0
	}
	return 0
}
