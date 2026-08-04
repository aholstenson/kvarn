package orchestrator

import (
	"context"
	"errors"
	"sort"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/orchestrator/auth"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/session"
)

// defaultQueueLimit bounds one page of ListQueue when the caller names none.
const defaultQueueLimit = 100

// projectVisibility returns a predicate for whether the caller may see a
// project. With auth off everything is visible; with auth on it is what the
// key's scope covers, and a missing identity sees nothing.
func (s *Service) projectVisibility(ctx context.Context) func(string) bool {
	if !s.authEnabled {
		return func(string) bool { return true }
	}
	id, ok := auth.IdentityFrom(ctx)
	return func(project string) bool {
		return ok && id.AllowsProject(project)
	}
}

// GetQueueStats reports how full the host is, which is the question a listing
// of jobs cannot answer: a job sits in the backlog either because the pipeline
// is at its bound or because the resource pool is exhausted, and the two call
// for different responses.
//
// The aggregate counters and the resource pool are host-wide — they describe
// the orchestrator, not any one project — while the per-project breakdown is
// restricted to the projects the caller's key covers.
func (s *Service) GetQueueStats(ctx context.Context, _ *connect.Request[v1.GetQueueStatsRequest]) (*connect.Response[v1.GetQueueStatsResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	// The backlog is read as rows rather than as a count because the
	// per-project split needs them anyway, and its size is bounded by
	// max_backlog.
	pending, err := s.sessionMgr.ListPending(ctx, session.PendingQuery{
		Now:     time.Now(),
		AgeStep: s.dispatcher.ageStep(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	visible := s.projectVisibility(ctx)
	perProject := map[string]*v1.QueueProjectStats{}
	stat := func(project string) *v1.QueueProjectStats {
		if !visible(project) {
			return nil
		}
		st, ok := perProject[project]
		if !ok {
			st = &v1.QueueProjectStats{Project: project}
			perProject[project] = st
		}
		return st
	}
	for _, sess := range pending {
		if st := stat(sess.ProjectName); st != nil {
			st.Backlog++
		}
	}
	for project, n := range s.dispatchedPerProject() {
		if st := stat(project); st != nil {
			st.Dispatched = int32(n)
		}
	}

	projects := make([]*v1.QueueProjectStats, 0, len(perProject))
	for _, st := range perProject {
		projects = append(projects, st)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Project < projects[j].Project })

	used, free, queueLen := s.scheduler.Snapshot()
	diskAvail, diskFloor, diskMeasured, diskOpen := s.scheduler.DiskGuard()

	stats := &v1.GetQueueStatsResponse{
		Backlog:            int32(len(pending)),
		Dispatched:         int32(s.dispatchedCount()),
		MaxBacklog:         int32(s.dispatcher.backlogBound()),
		MaxDispatched:      int32(s.dispatcher.maxDispatched()),
		AdmissionQueue:     int32(queueLen),
		Used:               capacityToProto(used),
		Free:               capacityToProto(free),
		PerProject:         projects,
		DiskAvailableBytes: diskAvail,
		DiskFloorBytes:     diskFloor,
		DiskMeasured:       diskMeasured,
		DiskGateOpen:       diskOpen,
	}
	// Reported here rather than through an RPC of its own: a job waiting on a
	// drained host and one waiting on a full one look identical in every other
	// number, and this is the field that tells them apart.
	s.drainStatsInto(stats)

	return connect.NewResponse(stats), nil
}

func capacityToProto(c scheduler.Capacity) *v1.Capacity {
	return &v1.Capacity{
		CpuMillis:   c.CPUMillis,
		MemoryBytes: c.MemBytes,
		DiskBytes:   c.DiskBytes,
	}
}

// ListQueue returns the backlog in dispatch order. That order is effective
// priority then arrival, which is neither the order ListSessions returns nor
// derivable from a session's fields alone — aging is relative to the rest of
// the backlog — so it is served here with each entry's place and the priority
// it is actually ordered by.
//
// The whole backlog is read even when the caller filters or limits, because a
// position is only meaningful against everything else waiting.
func (s *Service) ListQueue(ctx context.Context, req *connect.Request[v1.ListQueueRequest]) (*connect.Response[v1.ListQueueResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = defaultQueueLimit
	}

	query := session.PendingQuery{Now: time.Now(), AgeStep: s.dispatcher.ageStep()}
	pending, err := s.sessionMgr.ListPending(ctx, query)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	visible := s.projectVisibility(ctx)
	ceiling := session.PendingCeiling(pending)

	entries := make([]*v1.QueueEntry, 0, min(limit, len(pending)))
	for i, sess := range pending {
		if len(entries) >= limit {
			break
		}
		if req.Msg.Project != "" && sess.ProjectName != req.Msg.Project {
			continue
		}
		if !visible(sess.ProjectName) {
			continue
		}
		entries = append(entries, &v1.QueueEntry{
			Session:           sessionToProto(sess),
			Position:          int32(i + 1),
			EffectivePriority: int32(query.EffectivePriority(sess, ceiling)),
		})
	}

	return connect.NewResponse(&v1.ListQueueResponse{
		Entries: entries,
		Backlog: int32(len(pending)),
	}), nil
}
