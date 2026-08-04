package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Instruments bundles the orchestrator-level instruments. A nil Instruments
// is acceptable everywhere it's read; the helper methods are nil-safe so
// callers don't have to gate every Record/Add behind a check.
type Instruments struct {
	meter metric.Meter

	JobsStarted     metric.Int64Counter
	JobsCompleted   metric.Int64Counter
	JobDuration     metric.Float64Histogram
	AuthAttempts    metric.Int64Counter
	AdmissionWait   metric.Float64Histogram
	AdmissionDenies metric.Int64Counter
	sessionsActive  metric.Int64ObservableGauge
	schedCPUUsed    metric.Int64ObservableGauge
	schedCPUAvail   metric.Int64ObservableGauge
	schedMemUsed    metric.Int64ObservableGauge
	schedMemAvail   metric.Int64ObservableGauge
	schedDiskUsed   metric.Int64ObservableGauge
	schedDiskAvail  metric.Int64ObservableGauge
	schedQueue      metric.Int64ObservableGauge
	schedBacklog    metric.Int64ObservableGauge
	schedHostDisk   metric.Int64ObservableGauge
	schedPaused     metric.Int64ObservableGauge

	regs []metric.Registration
}

// SessionCounter returns the current count of active sessions. Supplied at
// NewInstruments time so the gauge can be polled by the SDK.
type SessionCounter func(context.Context) (int64, error)

// SchedulerSampler returns the current scheduler usage snapshot.
type SchedulerSampler func() SchedulerSample

// SchedulerSample is the point-in-time view the gauge callbacks emit.
type SchedulerSample struct {
	CPUMillisUsed, CPUMillisTotal int64
	MemBytesUsed, MemBytesTotal   int64
	DiskBytesUsed, DiskBytesTotal int64
	// QueueDepth is how many jobs are waiting for capacity in the in-memory
	// pipeline, and BacklogDepth how many are persisted but not yet dispatched
	// into it. The pair distinguishes a busy host — pipeline full, backlog
	// empty — from one that is not keeping up at all. A negative BacklogDepth
	// means the store could not be counted and is not reported.
	QueueDepth   int64
	BacklogDepth int64
	// HostDiskFreeBytes is the disk guard's last measurement of real free
	// space, and HostDiskMeasured says whether one has been taken yet. The
	// pool's disk accounting is overcommitted, so this is the number that says
	// whether the host is actually filling up.
	HostDiskFreeBytes int64
	HostDiskMeasured  bool
	// AdmissionPaused reports the guard holding admission closed, which looks
	// from every other metric like a queue that stopped moving for no reason.
	AdmissionPaused bool
}

// NewInstruments constructs the orchestrator instrument set against m. A nil
// meter falls back to a no-op so callers can wire instruments unconditionally.
func NewInstruments(m metric.Meter, sessions SessionCounter, sched SchedulerSampler) (*Instruments, error) {
	if m == nil {
		m = noop.NewMeterProvider().Meter("kvarn")
	}
	ins := &Instruments{meter: m}

	var err error
	if ins.JobsStarted, err = m.Int64Counter("kvarn.jobs.started",
		metric.WithDescription("Jobs accepted by the orchestrator")); err != nil {
		return nil, fmt.Errorf("jobs.started: %w", err)
	}
	if ins.JobsCompleted, err = m.Int64Counter("kvarn.jobs.completed",
		metric.WithDescription("Jobs that reached a terminal state")); err != nil {
		return nil, fmt.Errorf("jobs.completed: %w", err)
	}
	if ins.JobDuration, err = m.Float64Histogram("kvarn.job.duration_seconds",
		metric.WithDescription("End-to-end runJob duration"),
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("job.duration: %w", err)
	}
	if ins.AuthAttempts, err = m.Int64Counter("kvarn.auth.attempts",
		metric.WithDescription("API key authentication attempts")); err != nil {
		return nil, fmt.Errorf("auth.attempts: %w", err)
	}
	if ins.AdmissionWait, err = m.Float64Histogram("kvarn.scheduler.admission_wait_seconds",
		metric.WithDescription("Time a job spent queued for capacity before starting"),
		metric.WithUnit("s")); err != nil {
		return nil, fmt.Errorf("scheduler.admission_wait: %w", err)
	}
	if ins.AdmissionDenies, err = m.Int64Counter("kvarn.scheduler.admission_denied",
		metric.WithDescription("Jobs refused admission, by reason")); err != nil {
		return nil, fmt.Errorf("scheduler.admission_denied: %w", err)
	}

	if sessions != nil {
		if ins.sessionsActive, err = m.Int64ObservableGauge("kvarn.sessions.active",
			metric.WithDescription("Sessions currently tracked by the session manager")); err != nil {
			return nil, fmt.Errorf("sessions.active: %w", err)
		}
		reg, err := m.RegisterCallback(func(ctx context.Context, obs metric.Observer) error {
			n, err := sessions(ctx)
			if err != nil {
				return nil
			}
			obs.ObserveInt64(ins.sessionsActive, n)
			return nil
		}, ins.sessionsActive)
		if err != nil {
			return nil, fmt.Errorf("register sessions callback: %w", err)
		}
		ins.regs = append(ins.regs, reg)
	}

	if sched != nil {
		if ins.schedCPUUsed, err = m.Int64ObservableGauge("kvarn.scheduler.cpu_used",
			metric.WithDescription("CPU millis in use in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.cpu_used: %w", err)
		}
		if ins.schedCPUAvail, err = m.Int64ObservableGauge("kvarn.scheduler.cpu_available",
			metric.WithDescription("CPU millis available in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.cpu_available: %w", err)
		}
		if ins.schedMemUsed, err = m.Int64ObservableGauge("kvarn.scheduler.memory_used",
			metric.WithDescription("Memory bytes in use in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.memory_used: %w", err)
		}
		if ins.schedMemAvail, err = m.Int64ObservableGauge("kvarn.scheduler.memory_available",
			metric.WithDescription("Memory bytes available in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.memory_available: %w", err)
		}
		if ins.schedDiskUsed, err = m.Int64ObservableGauge("kvarn.scheduler.disk_used",
			metric.WithDescription("Disk bytes in use in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.disk_used: %w", err)
		}
		if ins.schedDiskAvail, err = m.Int64ObservableGauge("kvarn.scheduler.disk_available",
			metric.WithDescription("Disk bytes available in the admission pool")); err != nil {
			return nil, fmt.Errorf("scheduler.disk_available: %w", err)
		}
		if ins.schedQueue, err = m.Int64ObservableGauge("kvarn.scheduler.queue_depth",
			metric.WithDescription("Jobs waiting for admission")); err != nil {
			return nil, fmt.Errorf("scheduler.queue_depth: %w", err)
		}
		if ins.schedBacklog, err = m.Int64ObservableGauge("kvarn.scheduler.backlog_depth",
			metric.WithDescription("Jobs persisted in the durable backlog, not yet dispatched")); err != nil {
			return nil, fmt.Errorf("scheduler.backlog_depth: %w", err)
		}
		if ins.schedHostDisk, err = m.Int64ObservableGauge("kvarn.scheduler.host_disk_free",
			metric.WithDescription("Measured free space on the filesystem VM disks are allocated on")); err != nil {
			return nil, fmt.Errorf("scheduler.host_disk_free: %w", err)
		}
		if ins.schedPaused, err = m.Int64ObservableGauge("kvarn.scheduler.admission_paused",
			metric.WithDescription("1 while the host disk guard is holding admission closed")); err != nil {
			return nil, fmt.Errorf("scheduler.admission_paused: %w", err)
		}
		observed := []metric.Observable{
			ins.schedCPUUsed, ins.schedCPUAvail,
			ins.schedMemUsed, ins.schedMemAvail,
			ins.schedDiskUsed, ins.schedDiskAvail,
			ins.schedQueue, ins.schedBacklog, ins.schedPaused,
			ins.schedHostDisk,
		}
		reg, err := m.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
			s := sched()
			obs.ObserveInt64(ins.schedCPUUsed, s.CPUMillisUsed)
			obs.ObserveInt64(ins.schedCPUAvail, s.CPUMillisTotal-s.CPUMillisUsed)
			obs.ObserveInt64(ins.schedMemUsed, s.MemBytesUsed)
			obs.ObserveInt64(ins.schedMemAvail, s.MemBytesTotal-s.MemBytesUsed)
			obs.ObserveInt64(ins.schedDiskUsed, s.DiskBytesUsed)
			obs.ObserveInt64(ins.schedDiskAvail, s.DiskBytesTotal-s.DiskBytesUsed)
			obs.ObserveInt64(ins.schedQueue, s.QueueDepth)
			if s.BacklogDepth >= 0 {
				obs.ObserveInt64(ins.schedBacklog, s.BacklogDepth)
			}
			obs.ObserveInt64(ins.schedPaused, boolGauge(s.AdmissionPaused))
			// Reporting a zero before the first sample would read as a full
			// disk, which is the one thing this gauge is watched for.
			if s.HostDiskMeasured {
				obs.ObserveInt64(ins.schedHostDisk, s.HostDiskFreeBytes)
			}
			return nil
		}, observed...)
		if err != nil {
			return nil, fmt.Errorf("register scheduler callback: %w", err)
		}
		ins.regs = append(ins.regs, reg)
	}

	return ins, nil
}

// RecordJobStart bumps kvarn.jobs.started; nil-safe.
func (i *Instruments) RecordJobStart(ctx context.Context, project, mode string) {
	if i == nil {
		return
	}
	i.JobsStarted.Add(ctx, 1, metric.WithAttributes(
		attrStr("project", project), attrStr("mode", mode),
	))
}

// RecordJobEnd bumps kvarn.jobs.completed and records duration; nil-safe.
func (i *Instruments) RecordJobEnd(ctx context.Context, project, mode, outcome string, durationSeconds float64) {
	if i == nil {
		return
	}
	attrs := metric.WithAttributes(
		attrStr("project", project), attrStr("mode", mode), attrStr("outcome", outcome),
	)
	i.JobsCompleted.Add(ctx, 1, attrs)
	i.JobDuration.Record(ctx, durationSeconds, attrs)
}

// RecordAuth bumps kvarn.auth.attempts; nil-safe.
func (i *Instruments) RecordAuth(ctx context.Context, outcome, reason string) {
	if i == nil {
		return
	}
	i.AuthAttempts.Add(ctx, 1, metric.WithAttributes(
		attrStr("outcome", outcome), attrStr("reason", reason),
	))
}

// RecordAdmissionWait records how long a job waited for capacity; nil-safe.
func (i *Instruments) RecordAdmissionWait(ctx context.Context, project, mode string, seconds float64) {
	if i == nil {
		return
	}
	i.AdmissionWait.Record(ctx, seconds, metric.WithAttributes(
		attrStr("project", project), attrStr("mode", mode),
	))
}

// RecordAdmissionDenied bumps kvarn.scheduler.admission_denied; nil-safe.
func (i *Instruments) RecordAdmissionDenied(ctx context.Context, project, reason string) {
	if i == nil {
		return
	}
	i.AdmissionDenies.Add(ctx, 1, metric.WithAttributes(
		attrStr("project", project), attrStr("reason", reason),
	))
}

func boolGauge(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// Close unregisters observable callbacks. Safe to call multiple times.
func (i *Instruments) Close() {
	if i == nil {
		return
	}
	for _, r := range i.regs {
		_ = r.Unregister()
	}
	i.regs = nil
}
