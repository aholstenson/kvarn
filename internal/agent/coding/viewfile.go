package coding

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/attachment"
)

// viewFileMaxBytes bounds one file view_file reads. It is the same bound an
// attachment has, so a file the requester could send is a file the agent can
// look at, and it keeps the encoded copy that crosses the bridge small.
const viewFileMaxBytes = attachment.MaxFileBytes

// viewFileTimeoutSeconds bounds the read in the guest. Reading and encoding
// ten megabytes takes well under a second; the timeout is for a path that
// blocks, such as a FIFO.
const viewFileTimeoutSeconds = 60

// viewFileScript prints a file as base64 after checking that it is a regular
// file within the size bound. It uses only POSIX tools, and the base64 output
// is wrapped differently by GNU and BSD, so the host strips the line breaks.
const viewFileScript = `f=$1; max=$2
if [ ! -f "$f" ]; then echo "$f is not a regular file" >&2; exit 2; fi
size=$(wc -c < "$f") || exit 2
if [ "$size" -gt "$max" ]; then echo "$f is $size bytes; view_file reads at most $max" >&2; exit 3; fi
base64 < "$f"`

type ViewFileInput struct {
	Path string `json:"path" jsonschema:"description=Path to an image (PNG, JPEG, GIF or WebP) or a PDF. Relative to the workspace root, or absolute anywhere in the VM, such as a file in the attachments or scratch directory."`
}

// ViewFileOutput is one file read for the model to look at. Data is left out
// of JSON, because the progress stream records a tool's output as JSON and a
// copy of the image there would only fill the event log.
type ViewFileOutput struct {
	Path      string
	MediaType string
	Size      int
	Data      []byte `json:"-"`
}

type viewFileTool struct {
	toolkit *CodingToolkit
}

func (t *viewFileTool) Name() string { return "view_file" }
func (t *viewFileTool) Description() string {
	return `Look at an image (PNG, JPEG, GIF or WebP) or a PDF: an attached mockup, a screenshot you took of the running app, a chart or a page you rendered into the scratch directory. The file is added to the conversation as content you can see.

Use read_file for text. A viewed file stays in the conversation and is paid for on every later turn, so view each file once and only when you need to see it. At most 10 MiB.`
}
func (t *viewFileTool) Schema() *ViewFileInput { return &ViewFileInput{} }

// Execute reads the file as the job's own user, not as the runner's root, so
// the agent sees exactly the files its shell commands could already read.
func (t *viewFileTool) Execute(ctx context.Context, input *ViewFileInput) (*ViewFileOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	resp, err := t.toolkit.runner.Exec(ctx, &v1.ExecRequest{
		Command:        "sh",
		Args:           []string{"-c", viewFileScript, "sh", input.Path, strconv.Itoa(viewFileMaxBytes)},
		WorkingDir:     t.toolkit.workingDir,
		TimeoutSeconds: viewFileTimeoutSeconds,
	})
	if err != nil {
		return nil, err
	}
	if resp.ExitCode != 0 {
		return nil, fmt.Errorf("read %s: %s", input.Path, strings.TrimSpace(resp.Stderr))
	}
	data, err := base64.StdEncoding.DecodeString(strings.Map(dropSpace, resp.Stdout))
	if err != nil {
		return nil, fmt.Errorf("read %s: decode: %w", input.Path, err)
	}

	mediaType := attachment.DetectMediaType(data)
	switch attachment.PromptKind(mediaType) {
	case attachment.KindImage, attachment.KindPDF:
	default:
		return nil, fmt.Errorf("%s is %s; view_file shows images (PNG, JPEG, GIF or WebP) and PDFs, and read_file reads text", input.Path, mediaType)
	}

	out := &ViewFileOutput{Path: input.Path, MediaType: mediaType, Size: len(data), Data: data}
	// llms-go checks what goes into a message before it sends it, but not the
	// attachments of a tool result. A file the model cannot take is refused
	// here, as an error the agent can act on, instead of as a failed request
	// that ends the turn.
	if err := llms.CheckMessages(t.toolkit.viewModel, []*llms.Message{
		llms.NewMessage(llms.RoleUser, out.part()),
	}); err != nil {
		return nil, fmt.Errorf("the model cannot look at %s: %w", input.Path, err)
	}
	return out, nil
}

func (t *viewFileTool) Render(o *ViewFileOutput) llms.ToolResult {
	return llms.ToolResult{
		Text:        fmt.Sprintf("Showing %s (%s, %s).", o.Path, o.MediaType, formatBytes(int64(o.Size))),
		Attachments: []llms.MessagePart{o.part()},
	}
}

func (o *ViewFileOutput) part() *llms.BinaryPart {
	return llms.NewBinaryPart(o.MediaType, o.Data).WithName(path.Base(o.Path))
}

// dropSpace removes the line breaks base64 wraps its output with.
func dropSpace(r rune) rune {
	if unicode.IsSpace(r) {
		return -1
	}
	return r
}
