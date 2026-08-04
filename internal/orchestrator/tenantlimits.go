package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	orchcfg "github.com/aholstenson/kvarn/internal/config/orchestrator"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// TenantLimitDefaults are the host-wide concurrency caps a project or key
// inherits when it sets none of its own.
type TenantLimitDefaults struct {
	PerProject scheduler.Limits
	PerKey     scheduler.Limits
}

// resolveTenantLimits builds the host-wide defaults from the [scheduler] table.
func resolveTenantLimits(cfg orchcfg.Scheduler) (TenantLimitDefaults, error) {
	perProject, err := parseTenantLimits(cfg.PerProject)
	if err != nil {
		return TenantLimitDefaults{}, fmt.Errorf("per_project: %w", err)
	}
	perKey, err := parseTenantLimits(cfg.PerKey)
	if err != nil {
		return TenantLimitDefaults{}, fmt.Errorf("per_key: %w", err)
	}
	return TenantLimitDefaults{PerProject: perProject, PerKey: perKey}, nil
}

func parseTenantLimits(c orchcfg.TenantLimits) (scheduler.Limits, error) {
	return buildLimits(c.MaxJobs, c.MaxCPUs, c.MaxMemory, c.MaxDisk, scheduler.Limits{})
}

// projectLimits resolves a project's caps, falling back per field to the
// host-wide default.
func projectLimits(p *project.Project, def scheduler.Limits) (scheduler.Limits, error) {
	if p == nil {
		return def, nil
	}
	l, err := buildLimits(p.MaxJobs, p.MaxCPUs, p.MaxMemory, p.MaxDisk, def)
	if err != nil {
		return scheduler.Limits{}, fmt.Errorf("project %q: %w", p.Name, err)
	}
	return l, nil
}

// keyLimits resolves an API key's caps, falling back per field to the
// host-wide default.
func keyLimits(k *apikey.APIKey, def scheduler.Limits) (scheduler.Limits, error) {
	if k == nil {
		return def, nil
	}
	l, err := buildLimits(k.MaxJobs, k.MaxCPUs, k.MaxMemory, k.MaxDisk, def)
	if err != nil {
		return scheduler.Limits{}, fmt.Errorf("key %q: %w", k.Name, err)
	}
	return l, nil
}

// resolveJobLimits resolves the project and API-key caps in force for one job.
// A key that cannot be read is not fatal: its caps are a throttle, and losing
// them should not turn a readable-config problem into a failed job. The project
// is already loaded, so its caps are always exact.
func (s *Service) resolveJobLimits(ctx context.Context, proj *project.Project, keyID string) (scheduler.Limits, scheduler.Limits, error) {
	pl, err := projectLimits(proj, s.tenantLimits.PerProject)
	if err != nil {
		return scheduler.Limits{}, scheduler.Limits{}, err
	}

	kl := s.tenantLimits.PerKey
	if keyID != "" && s.apiKeyStore != nil {
		key, err := s.apiKeyStore.Get(ctx, keyID)
		if err != nil {
			slog.WarnContext(ctx, "could not read API key limits; using host defaults",
				"key_id", keyID, "error", err)
		} else if kl, err = keyLimits(key, s.tenantLimits.PerKey); err != nil {
			return scheduler.Limits{}, scheduler.Limits{}, err
		}
	}
	return pl, kl, nil
}

// jobPriority resolves the scheduling priority for one job: the project's
// per-mode override, then the project's own value, then zero. It mirrors how
// the cost cap resolves, so a project's config reads the same way whichever
// knob is being set.
func jobPriority(p *project.Project, mode string) int {
	if p == nil {
		return 0
	}
	if j, ok := p.Jobs[mode]; ok && j.Priority != nil {
		return *j.Priority
	}
	if p.Priority != nil {
		return *p.Priority
	}
	return 0
}

// buildLimits parses one scope's raw fields, inheriting def field by field
// rather than all-or-nothing: a project that caps only its job count still
// inherits the host's memory cap.
//
// An explicitly configured zero is kept as zero — "unlimited in this
// dimension" — so a project can opt out of a host default it would otherwise
// inherit. That is why the numeric fields are pointers: absent and zero have
// to mean different things.
func buildLimits(maxJobs *int, maxCPUs *uint, maxMemory, maxDisk string, def scheduler.Limits) (scheduler.Limits, error) {
	out := def
	if maxJobs != nil {
		if *maxJobs < 0 {
			return scheduler.Limits{}, fmt.Errorf("max_jobs must be >= 0, got %d", *maxJobs)
		}
		out.MaxJobs = *maxJobs
	}
	if maxCPUs != nil {
		out.Max.CPUMillis = uint64(*maxCPUs) * 1000
	}
	if maxMemory != "" {
		n, err := projconfig.ParseSize(maxMemory)
		if err != nil {
			return scheduler.Limits{}, fmt.Errorf("max_memory: %w", err)
		}
		out.Max.MemBytes = uint64(n)
	}
	if maxDisk != "" {
		n, err := projconfig.ParseSize(maxDisk)
		if err != nil {
			return scheduler.Limits{}, fmt.Errorf("max_disk: %w", err)
		}
		out.Max.DiskBytes = uint64(n)
	}
	return out, nil
}
