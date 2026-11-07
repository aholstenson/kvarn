package coding

import (
	"context"
	"fmt"
	"log/slog"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/attachment"
)

var _ agent.PromptAttachmentPlanner = (*CodingAgent)(nil)

// PlanPromptAttachments decides which files go into the first message, by the
// same input check llms-go runs before it sends a request. The model is the
// one the main loop runs on in the current configuration, so a check at
// submission and the run that follows it agree unless agents.toml changes in
// between; the run checks again for that reason.
func (a *CodingAgent) PlanPromptAttachments(ctx context.Context, candidates []attachment.File) ([]attachment.File, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	models, err := a.resolver.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve agent models: %w", err)
	}
	return planPromptAttachments(models.Classes[ModelMain], candidates)
}

// planPromptAttachments checks each candidate alone, so that a file with
// automatic delivery the model refuses is left out instead of failing the
// files around it, and then checks the chosen set together, which is where a
// limit on the number of images applies.
func planPromptAttachments(model llms.Model, candidates []attachment.File) ([]attachment.File, error) {
	var chosen []attachment.File
	for _, f := range candidates {
		err := llms.CheckMessages(model, []*llms.Message{
			llms.NewMessage(llms.RoleUser, attachmentPart(f)),
		})
		switch {
		case err == nil:
			chosen = append(chosen, f)
		case f.Delivery.InPrompt():
			return nil, fmt.Errorf("attachment %q cannot go into the prompt: %w", f.Name, err)
		default:
			slog.Info("attachment left out of the prompt; the model does not accept it",
				"attachment", f.Name, "media_type", f.MediaType, "reason", err)
		}
	}
	if len(chosen) == 0 {
		return nil, nil
	}
	if err := llms.CheckMessages(model, []*llms.Message{firstMessage("", chosen)}); err != nil {
		return nil, fmt.Errorf("attachments cannot go into the prompt together: %w", err)
	}
	return chosen, nil
}

// attachmentPart is how one file goes into a message. The name travels with
// it, so the model can match the file to its line in the task message.
func attachmentPart(f attachment.File) llms.MessagePart {
	return llms.NewBinaryPart(f.MediaType, f.Data).WithName(f.Name)
}

// firstMessage is the task message of a run: the files first, then the text
// that refers to them, which is the order the providers recommend for images.
func firstMessage(prompt string, files []attachment.File) *llms.Message {
	parts := make([]llms.MessagePart, 0, len(files)+1)
	for _, f := range files {
		parts = append(parts, attachmentPart(f))
	}
	if prompt != "" || len(files) == 0 {
		parts = append(parts, llms.NewTextPart(prompt))
	}
	return llms.NewMessage(llms.RoleUser, parts...)
}
