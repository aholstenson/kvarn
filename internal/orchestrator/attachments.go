package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/attachment"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/session"
)

// maxStartJobRequestBytes bounds one request to the orchestrator service. The
// largest legitimate request is a submission carrying the most attachments it
// may, and the JSON codec writes bytes as base64, which is a third larger
// than the content. The rest of the room is for the prompt and the inline
// mode and validation definitions.
const maxStartJobRequestBytes = attachment.MaxTotalBytes/3*4 + 16<<20

// attachmentsFromProto reads a submission's attachments. The checks on their
// names, sizes and media types are attachment.Prepare's; this only maps the
// wire form.
func attachmentsFromProto(in []*v1.Attachment) ([]attachment.File, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]attachment.File, len(in))
	for i, a := range in {
		delivery, err := deliveryFromProto(a.GetDelivery())
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", a.GetName(), err)
		}
		out[i] = attachment.File{
			Name:      a.GetName(),
			MediaType: a.GetMediaType(),
			Delivery:  delivery,
			Data:      a.GetData(),
		}
	}
	return out, nil
}

func deliveryFromProto(d v1.AttachmentDelivery) (attachment.Delivery, error) {
	switch d {
	case v1.AttachmentDelivery_ATTACHMENT_DELIVERY_UNSPECIFIED:
		return attachment.DeliveryAuto, nil
	case v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM:
		return attachment.DeliveryVM, nil
	case v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT:
		return attachment.DeliveryPrompt, nil
	case v1.AttachmentDelivery_ATTACHMENT_DELIVERY_BOTH:
		return attachment.DeliveryBoth, nil
	default:
		return "", fmt.Errorf("unknown delivery %d", d)
	}
}

func deliveryToProto(d attachment.Delivery) v1.AttachmentDelivery {
	switch d {
	case attachment.DeliveryVM:
		return v1.AttachmentDelivery_ATTACHMENT_DELIVERY_VM
	case attachment.DeliveryPrompt:
		return v1.AttachmentDelivery_ATTACHMENT_DELIVERY_PROMPT
	case attachment.DeliveryBoth:
		return v1.AttachmentDelivery_ATTACHMENT_DELIVERY_BOTH
	default:
		return v1.AttachmentDelivery_ATTACHMENT_DELIVERY_UNSPECIFIED
	}
}

// attachmentInfosToProto describes stored attachments on the wire. The
// content never goes out on a session read: a listing of fifty jobs would
// otherwise carry every file they were sent.
func attachmentInfosToProto(in []session.Attachment) []*v1.AttachmentInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]*v1.AttachmentInfo, len(in))
	for i, a := range in {
		out[i] = &v1.AttachmentInfo{
			Name:      a.Name,
			MediaType: a.MediaType,
			Size:      a.Size,
			Sha256:    a.SHA256,
			Delivery:  deliveryToProto(a.Delivery),
		}
	}
	return out
}

// sameAttachments reports whether a submission carries the attachments a
// claimed session stored. Order counts, because it is the order the agent is
// told about them in. The content is compared by digest.
func sameAttachments(claimed []session.Attachment, files []attachment.File) bool {
	if len(claimed) != len(files) {
		return false
	}
	for i, f := range files {
		c := claimed[i]
		if c.Name != f.Name || c.MediaType != f.MediaType || c.Delivery != f.Delivery ||
			c.Size != int64(len(f.Data)) || c.SHA256 != attachment.Digest(f.Data) {
			return false
		}
	}
	return true
}

// loadAttachments reads the content of stored attachments back into files.
// keep selects which of them to read, so a run reads only the files it
// delivers; it sees each file without its content. Nil keeps all of them.
func loadAttachments(ctx context.Context, mgr session.Manager, stored []session.Attachment, keep func(attachment.File) bool) ([]attachment.File, error) {
	var out []attachment.File
	for _, a := range stored {
		described := attachment.File{Name: a.Name, MediaType: a.MediaType, Delivery: a.Delivery}
		if keep != nil && !keep(described) {
			continue
		}
		data, err := mgr.AttachmentData(ctx, a.SHA256)
		if err != nil {
			return nil, fmt.Errorf("read attachment %q: %w", a.Name, err)
		}
		out = append(out, attachment.File{
			Name:      a.Name,
			MediaType: a.MediaType,
			Delivery:  a.Delivery,
			Data:      data,
		})
	}
	return out, nil
}

// attachmentRefs describes stored attachments the way the agent is told about
// them: each file the run writes into the VM with its path there, and whether
// it is also in the first message. inPrompt is the plan's result.
func attachmentRefs(stored []session.Attachment, inPrompt []attachment.File) []coding.AttachmentRef {
	if len(stored) == 0 {
		return nil
	}
	prompted := make(map[string]bool, len(inPrompt))
	for _, f := range inPrompt {
		prompted[f.Name] = true
	}
	out := make([]coding.AttachmentRef, len(stored))
	for i, a := range stored {
		out[i] = coding.AttachmentRef{
			Name:      a.Name,
			MediaType: a.MediaType,
			Size:      a.Size,
			InPrompt:  prompted[a.Name],
		}
		if a.Delivery.InVM() {
			out[i].Path = sandbox.AttachmentPath(a.Name)
		}
	}
	return out
}

// checkPromptAttachments refuses, at submission, a file the submitter asked
// into the prompt that the agent's model does not accept. Files with
// automatic delivery are not checked: the run leaves out what the model
// refuses, so they cannot fail it.
func (s *Service) checkPromptAttachments(ctx context.Context, files []attachment.File) error {
	planner, ok := s.agent.(agent.PromptAttachmentPlanner)
	if !ok {
		return nil
	}
	var asked []attachment.File
	for _, f := range files {
		if f.Delivery.InPrompt() {
			asked = append(asked, f)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	_, err := planner.PlanPromptAttachments(ctx, asked)
	if err == nil {
		return nil
	}
	// A file the model refuses is the submitter's to fix. Anything else, such
	// as a model configuration that does not load, is not, and the same
	// request may succeed once it is fixed.
	var inputErr *llms.InputError
	if errors.As(err, &inputErr) {
		return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	return connect.NewError(connect.CodeUnavailable,
		"check attachments against the agent's model: "+err.Error()).WithCause(err)
}

// planPromptAttachments reads the run's prompt candidates and asks the agent
// which of them go into the first message. It runs before the VM boots, so a
// file the model no longer accepts — agents.toml may have changed since the
// submission was checked — fails the run before it costs anything.
//
// An agent that cannot plan gets the files the submitter asked for and none
// of the automatic ones, since nothing can tell whether its model reads them.
func (s *Service) planPromptAttachments(ctx context.Context, stored []session.Attachment) ([]attachment.File, error) {
	candidates, err := loadAttachments(ctx, s.sessionMgr, stored, attachment.File.PromptCandidate)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	planner, ok := s.agent.(agent.PromptAttachmentPlanner)
	if !ok {
		var asked []attachment.File
		for _, f := range candidates {
			if f.Delivery.InPrompt() {
				asked = append(asked, f)
			}
		}
		return asked, nil
	}
	return planner.PlanPromptAttachments(ctx, candidates)
}
