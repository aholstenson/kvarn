package orchestrator

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/preview"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// previewWatchInterval is how often WatchPreview re-reads a preview's state.
// A boot moves through a handful of phases over minutes, so polling the
// store is both simpler and sufficient: there is no event stream to subscribe
// to, and the phases themselves come from the boot's session, which the client
// can watch directly if it wants every event.
const previewWatchInterval = time.Second

// previewLogTailLines is how much of a preview's output GetPreview returns.
const previewLogTailLines = 200

// StartPreview registers a preview of a ref and boots it.
func (s *Service) StartPreview(ctx context.Context, req *connect.Request[v1.StartPreviewRequest]) (*connect.Response[v1.StartPreviewResponse], error) {
	if err := s.authorizeProject(ctx, req.Msg.Project, req.Spec().Procedure); err != nil {
		return nil, err
	}
	if err := validatePreviewTarget(req.Msg.Project, req.Msg.Ref); err != nil {
		return nil, err
	}

	p, err := s.previews.Register(ctx, req.Msg.Project, req.Msg.Ref, previewOrigin{PR: req.Msg.Pr})
	if err != nil {
		return nil, previewConnectError(err)
	}

	if _, err := s.previews.EnsureNow(ctx, p.ID); err != nil {
		return nil, previewConnectError(err)
	}

	if req.Msg.Wait {
		p, err = s.awaitPreview(ctx, p.ID)
		if err != nil {
			return nil, previewConnectError(err)
		}
	} else if updated, err := s.previews.Get(ctx, p.ID); err == nil {
		p = updated
	}

	reqid.LoggerFrom(ctx).Info("preview start requested",
		"project", req.Msg.Project, "ref", req.Msg.Ref, "state", p.State)

	return connect.NewResponse(&v1.StartPreviewResponse{Preview: previewToProto(p)}), nil
}

// awaitPreview blocks until the preview stops booting, so `--wait` returns a
// preview that is either serving or has said why it is not.
func (s *Service) awaitPreview(ctx context.Context, id string) (*preview.Preview, error) {
	ticker := time.NewTicker(previewWatchInterval)
	defer ticker.Stop()
	for {
		p, err := s.previews.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if p.State != preview.StateBooting {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// StopPreview takes a preview's VM down, and forgets the preview entirely when
// the request asks for it.
func (s *Service) StopPreview(ctx context.Context, req *connect.Request[v1.StopPreviewRequest]) (*connect.Response[v1.StopPreviewResponse], error) {
	if err := s.authorizeProject(ctx, req.Msg.Project, req.Spec().Procedure); err != nil {
		return nil, err
	}
	if err := validatePreviewTarget(req.Msg.Project, req.Msg.Ref); err != nil {
		return nil, err
	}

	id := preview.ID(req.Msg.Project, req.Msg.Ref)
	p, err := s.previews.Get(ctx, id)
	if err != nil {
		return nil, previewConnectError(err)
	}

	if req.Msg.Remove {
		if err := s.previews.Remove(ctx, id); err != nil {
			return nil, previewConnectError(err)
		}
		// The record is gone; report it as it was last seen, stopped.
		p.State = preview.StateStopped
		return connect.NewResponse(&v1.StopPreviewResponse{Preview: previewToProto(p)}), nil
	}

	if err := s.previews.Stop(ctx, id, "stopped by request"); err != nil {
		return nil, previewConnectError(err)
	}
	if updated, err := s.previews.Get(ctx, id); err == nil {
		p = updated
	}
	return connect.NewResponse(&v1.StopPreviewResponse{Preview: previewToProto(p)}), nil
}

// ListPreviews returns the previews the caller may see.
func (s *Service) ListPreviews(ctx context.Context, req *connect.Request[v1.ListPreviewsRequest]) (*connect.Response[v1.ListPreviewsResponse], error) {
	// A listing filtered to one project is authorized against it; an unfiltered
	// one is filtered down to what the caller's key covers, so a scoped key
	// never learns another project's branch names from the preview list.
	if req.Msg.Project != "" {
		if err := s.authorizeProject(ctx, req.Msg.Project, req.Spec().Procedure); err != nil {
			return nil, err
		}
	}

	previews, err := s.previews.List(ctx)
	if err != nil {
		return nil, previewConnectError(err)
	}

	out := make([]*v1.Preview, 0, len(previews))
	for _, p := range previews {
		if req.Msg.Project != "" && p.Project != req.Msg.Project {
			continue
		}
		if req.Msg.Project == "" {
			if err := s.authorizeProject(ctx, p.Project, req.Spec().Procedure); err != nil {
				continue
			}
		}
		out = append(out, previewToProto(p))
	}
	return connect.NewResponse(&v1.ListPreviewsResponse{Previews: out}), nil
}

// GetPreview returns one preview plus the tail of its output.
func (s *Service) GetPreview(ctx context.Context, req *connect.Request[v1.GetPreviewRequest]) (*connect.Response[v1.GetPreviewResponse], error) {
	if err := s.authorizeProject(ctx, req.Msg.Project, req.Spec().Procedure); err != nil {
		return nil, err
	}
	if err := validatePreviewTarget(req.Msg.Project, req.Msg.Ref); err != nil {
		return nil, err
	}

	id := preview.ID(req.Msg.Project, req.Msg.Ref)
	p, err := s.previews.Get(ctx, id)
	if err != nil {
		return nil, previewConnectError(err)
	}

	return connect.NewResponse(&v1.GetPreviewResponse{
		Preview: previewToProto(p),
		Logs:    s.previews.Logs(id, previewLogTailLines),
	}), nil
}

// WatchPreview streams a preview's state until it stops changing or the client
// goes away.
func (s *Service) WatchPreview(ctx context.Context, req *connect.Request[v1.WatchPreviewRequest], stream *connect.ServerStream[v1.PreviewUpdate]) error {
	if err := s.authorizeProject(ctx, req.Msg.Project, req.Spec().Procedure); err != nil {
		return err
	}
	if err := validatePreviewTarget(req.Msg.Project, req.Msg.Ref); err != nil {
		return err
	}

	id := preview.ID(req.Msg.Project, req.Msg.Ref)
	ticker := time.NewTicker(previewWatchInterval)
	defer ticker.Stop()

	var lastState preview.State
	var lastPhase string
	for {
		p, err := s.previews.Get(ctx, id)
		if err != nil {
			return previewConnectError(err)
		}
		phase := s.previewPhase(ctx, p)

		if p.State != lastState || phase != lastPhase {
			if err := stream.Send(&v1.PreviewUpdate{Preview: previewToProto(p), Phase: phase}); err != nil {
				return err
			}
			lastState, lastPhase = p.State, phase
		}

		// Booting is the only state that resolves on its own; anything else is
		// where the preview will stay until somebody acts on it.
		if p.State != preview.StateBooting {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// previewPhase is the human-readable step a booting preview is on, read from
// the session its boot reports through.
func (s *Service) previewPhase(ctx context.Context, p *preview.Preview) string {
	if p.SessionID == "" || s.sessionMgr == nil {
		return previewPhaseFallback(p.State)
	}
	sess, err := s.sessionMgr.Get(ctx, p.SessionID)
	if err != nil || sess == nil {
		return previewPhaseFallback(p.State)
	}
	if sess.Message != "" {
		return sess.Message
	}
	return previewPhaseLabel(sess.State)
}

// validatePreviewTarget checks the two fields every preview RPC identifies a
// preview by.
func validatePreviewTarget(project, ref string) error {
	if project == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("project is required"))
	}
	if ref == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("ref is required"))
	}
	return nil
}

// previewConnectError maps the manager's errors onto RPC codes.
func previewConnectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, preview.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrPreviewsDisabled):
		return connect.NewError(connect.CodeUnimplemented, err)
	case errors.Is(err, ErrAtCapacity):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, ErrPreviewDraining):
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, preview.ErrHostTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		var connectErr *connect.Error
		if errors.As(err, &connectErr) {
			return err
		}
		return connect.NewError(connect.CodeInternal, err)
	}
}

// previewToProto renders a preview for the wire.
func previewToProto(p *preview.Preview) *v1.Preview {
	if p == nil {
		return nil
	}
	out := &v1.Preview{
		Id:        p.ID,
		Project:   p.Project,
		Ref:       p.Ref,
		State:     string(p.State),
		SessionId: p.SessionID,
		Error:     p.Error,
		Url:       p.PrimaryURL(),
	}
	for _, site := range p.Sites {
		out.Sites = append(out.Sites, &v1.PreviewSite{
			Name: site.Name,
			Host: site.Host,
			Port: uint32(site.Port),
			Url:  "https://" + site.Host,
		})
	}
	if !p.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(p.CreatedAt)
	}
	if !p.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(p.UpdatedAt)
	}
	if !p.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(p.StartedAt)
	}
	if !p.LastRequestAt.IsZero() {
		out.LastRequestAt = timestamppb.New(p.LastRequestAt)
	}
	if !p.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(p.ExpiresAt)
	}
	return out
}
