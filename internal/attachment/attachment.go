// Package attachment holds the rules for files a submitter sends with a job:
// where each one goes, what a name may be, how large they may get, and what
// media type each one has.
//
// The orchestrator and `kvarn local job` both accept attachments, and both
// apply Prepare, so a file one of them accepts the other accepts too.
package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	// Registered so DecodeConfig can read the header of each image type a
	// model accepts. WebP has no decoder in the standard library; its content
	// sniff is the only check it gets.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Limits on what one job may carry. A submission is held in memory while it is
// checked and stored, and the bytes stay in the session store until retention
// removes the job, so the limits bound both.
const (
	MaxFiles      = 20
	MaxFileBytes  = 10 << 20
	MaxTotalBytes = 32 << 20
	maxNameBytes  = 255
	// MaxPromptTextBytes bounds a text file that goes into the first message,
	// which is about 64k tokens. It is far below MaxFileBytes because a text
	// file in the prompt is paid for on every turn of the run; a larger one is
	// better read in windows from the VM.
	MaxPromptTextBytes = 256 << 10
)

// Delivery says where an attachment goes in a run.
type Delivery string

const (
	// DeliveryAuto puts the file in the VM, and also in the first message
	// when it is an image or a PDF the model accepts: those are the files the
	// agent cannot read with read_file. It is the choice of a submitter who
	// did not make one, so it is never the reason a run fails.
	DeliveryAuto Delivery = ""
	// DeliveryVM puts the file in the VM only.
	DeliveryVM Delivery = "vm"
	// DeliveryPrompt puts the file in the first message to the model only.
	DeliveryPrompt Delivery = "prompt"
	// DeliveryBoth puts the file in the VM and in the first message.
	DeliveryBoth Delivery = "both"
)

// InVM reports whether the file is written into the VM.
func (d Delivery) InVM() bool {
	return d == DeliveryAuto || d == DeliveryVM || d == DeliveryBoth
}

// InPrompt reports whether the submitter asked for the file to be in the
// first message. Such a file must get there, or the run fails.
func (d Delivery) InPrompt() bool {
	return d == DeliveryPrompt || d == DeliveryBoth
}

// ParseDelivery reads a delivery by name. The empty string and "auto" are
// both DeliveryAuto.
func ParseDelivery(s string) (Delivery, error) {
	switch d := Delivery(s); d {
	case DeliveryAuto, DeliveryVM, DeliveryPrompt, DeliveryBoth:
		return d, nil
	case "auto":
		return DeliveryAuto, nil
	default:
		return "", fmt.Errorf("unknown delivery %q; use auto, vm, prompt or both", s)
	}
}

// File is one attachment with its content.
type File struct {
	Name      string
	MediaType string
	Delivery  Delivery
	Data      []byte
}

// PromptCandidate reports whether the file may go into the first message:
// every file the submitter asked to put there, and every image or PDF with
// automatic delivery. Whether a candidate does go there depends on the model,
// which only the agent can tell.
func (f File) PromptCandidate() bool {
	if f.Delivery.InPrompt() {
		return true
	}
	if f.Delivery != DeliveryAuto {
		return false
	}
	kind := PromptKind(f.MediaType)
	return kind == KindImage || kind == KindPDF
}

// The kinds of file a model reads in a message.
const (
	KindImage = "image"
	KindPDF   = "pdf"
	KindText  = "text"
)

// PromptKind returns how a model reads a file of the given media type:
// KindImage, KindPDF, KindText, or "" for a type no model reads in a message.
// Text is the types a model reads as text: text/*, JSON, XML and YAML.
func PromptKind(mediaType string) string {
	base, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(base, "image/"):
		return KindImage
	case base == "application/pdf":
		return KindPDF
	case strings.HasPrefix(base, "text/"),
		base == "application/json", base == "application/xml",
		base == "application/yaml", base == "application/x-yaml",
		strings.HasSuffix(base, "+json"), strings.HasSuffix(base, "+xml"),
		strings.HasSuffix(base, "+yaml"):
		return KindText
	}
	return ""
}

// Digest returns the hex SHA-256 of data. Stored attachments are keyed by it,
// so two jobs that carry the same file share one copy of it.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Prepare checks a job's attachments and returns them with every media type
// resolved. Nothing is repaired: a submitter who sent a bad name or too many
// bytes learns it now, instead of from a run that fails later or does
// something other than what was asked.
func Prepare(files []File) ([]File, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("%d attachments; the limit is %d", len(files), MaxFiles)
	}
	out := make([]File, len(files))
	seen := make(map[string]bool, len(files))
	total := 0
	for i, f := range files {
		if err := ValidateName(f.Name); err != nil {
			return nil, err
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("attachment %q is given more than once", f.Name)
		}
		seen[f.Name] = true
		if err := checkDelivery(f.Delivery); err != nil {
			return nil, fmt.Errorf("attachment %q: %w", f.Name, err)
		}
		if len(f.Data) > MaxFileBytes {
			return nil, fmt.Errorf("attachment %q is %d bytes; the limit is %d", f.Name, len(f.Data), MaxFileBytes)
		}
		total += len(f.Data)
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("attachments are at least %d bytes together; the limit is %d", total, MaxTotalBytes)
		}
		mediaType, err := resolveMediaType(f.MediaType, f.Data)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", f.Name, err)
		}
		f.MediaType = mediaType
		if err := checkPromptFit(f); err != nil {
			return nil, fmt.Errorf("attachment %q: %w", f.Name, err)
		}
		out[i] = f
	}
	return out, nil
}

func checkDelivery(d Delivery) error {
	switch d {
	case DeliveryAuto, DeliveryVM, DeliveryPrompt, DeliveryBoth:
		return nil
	default:
		return fmt.Errorf("unknown delivery %q", d)
	}
}

// checkPromptFit refuses a file asked into the first message that no model
// could read there. Whether the run's model accepts it is a separate check,
// because that depends on the model configuration and not on the file alone.
func checkPromptFit(f File) error {
	if !f.Delivery.InPrompt() {
		return nil
	}
	switch PromptKind(f.MediaType) {
	case KindImage, KindPDF:
		return nil
	case KindText:
		if len(f.Data) > MaxPromptTextBytes {
			return fmt.Errorf("a text file in the prompt is at most %d bytes, and this one is %d; deliver it to the VM", MaxPromptTextBytes, len(f.Data))
		}
		return nil
	default:
		return fmt.Errorf("a %s file cannot go into the prompt; deliver it to the VM", f.MediaType)
	}
}

// DetectMediaType returns the media type of data from its content alone,
// without parameters, such as "image/png" or "text/plain".
func DetectMediaType(data []byte) string {
	base, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return base
}

// ParseArg reads a CLI `--attach` value: a path, optionally after a delivery
// and "=", such as "prompt=shot.png". A prefix that is not a delivery name is
// part of the path, so "./vm=x" names a file called "vm=x".
func ParseArg(arg string) (Delivery, string, error) {
	prefix, rest, ok := strings.Cut(arg, "=")
	if !ok {
		return DeliveryAuto, arg, nil
	}
	d, err := ParseDelivery(prefix)
	if err != nil {
		return DeliveryAuto, arg, nil
	}
	if rest == "" {
		return "", "", fmt.Errorf("attachment %q names no file", arg)
	}
	return d, rest, nil
}

// ValidateName holds a name to one path element that every place it is
// written can carry as it is: a file in the guest, a line in the task
// message, a CLI argument. A name with a separator would place the file
// somewhere other than the attachments directory.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("attachment name is empty")
	}
	if len(name) > maxNameBytes {
		return fmt.Errorf("attachment name %q is %d bytes; the limit is %d", name, len(name), maxNameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("attachment name %q is not valid UTF-8", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("attachment name %q is not a file name", name)
	}
	for _, r := range name {
		if r == '/' || r == '\\' {
			return fmt.Errorf("attachment name %q contains a path separator; give the file name only", name)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("attachment name %q contains a control character", name)
		}
	}
	return nil
}

// checkedTypes are the media types a model reads as something other than
// text. A file declared as one of them, or detected as one, must be one,
// because a provider refuses a request that carries a mislabelled image and
// the run would fail far from the cause.
var checkedTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// resolveMediaType returns the media type a file is stored with: the declared
// one when there is one, otherwise the one detected from the content. The
// detection reads the content only, never the name, so the result is the same
// on every host.
func resolveMediaType(declared string, data []byte) (string, error) {
	detected := http.DetectContentType(data)
	detectedBase, _, _ := mime.ParseMediaType(detected)

	mediaType := detected
	base := detectedBase
	if strings.TrimSpace(declared) != "" {
		parsed, params, err := mime.ParseMediaType(declared)
		if err != nil {
			return "", fmt.Errorf("media type %q: %w", declared, err)
		}
		base = parsed
		mediaType = mime.FormatMediaType(parsed, params)
		if mediaType == "" {
			return "", fmt.Errorf("media type %q cannot be written back", declared)
		}
	}

	if (checkedTypes[base] || checkedTypes[detectedBase]) && base != detectedBase {
		return "", fmt.Errorf("declared as %s, but the content is %s", base, detectedBase)
	}
	if strings.HasPrefix(base, "image/") && base != "image/webp" && checkedTypes[base] {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("content is not a readable %s: %w", base, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return "", fmt.Errorf("image has no pixels (%dx%d)", cfg.Width, cfg.Height)
		}
	}
	return mediaType, nil
}

// ReadArgs reads local files as attachments named by their base names, for
// the CLI commands that take `--attach [DELIVERY=]PATH` (see ParseArg). A
// file over MaxFileBytes is refused before it is read, so a wrong path to a
// disk image costs a stat and not the memory to hold it.
func ReadArgs(args []string) ([]File, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]File, 0, len(args))
	for _, arg := range args {
		delivery, p, err := ParseArg(arg)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("attachment: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("attachment %s is not a regular file", p)
		}
		if info.Size() > MaxFileBytes {
			return nil, fmt.Errorf("attachment %s is %d bytes; the limit is %d", p, info.Size(), MaxFileBytes)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("attachment: %w", err)
		}
		out = append(out, File{Name: filepath.Base(p), Delivery: delivery, Data: data})
	}
	return out, nil
}
