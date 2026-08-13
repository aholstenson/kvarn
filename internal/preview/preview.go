// Package preview holds the durable record of a preview environment: a
// long-lived VM pinned to a git ref, reachable at a stable hostname, booted on
// demand and stopped when it goes idle.
//
// A preview is the first thing the orchestrator has to *reconstruct* rather
// than finish. A job either completes or fails; a preview outlives the job that
// produced its branch, outlives individual VMs, and has to survive a restart as
// something the next request can boot again. That is what this record is for —
// the VM, the sandbox session and the route table are all rebuilt from it.
package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by Get and Delete when no preview matches.
var ErrNotFound = errors.New("preview not found")

// ErrHostTaken is returned by Put when one of the preview's hostnames is
// already claimed by a different preview. Hostnames are the routing key, so two
// previews answering to one name is not something to resolve at request time.
var ErrHostTaken = errors.New("preview hostname already claimed")

// State is where a preview is in its lifecycle.
//
// There is no terminal state. Stopped is the resting state, not the end: the
// row stays so the next request knows what to boot and under which hostnames,
// and a preview only leaves the store when somebody takes it down for good.
type State string

const (
	// StateStopped means no VM is running. The hostnames still route here; a
	// request for one starts a boot.
	StateStopped State = "stopped"
	// StateBooting means a VM is being provisioned and the serve steps have not
	// finished coming up. Requests get the holding page.
	StateBooting State = "booting"
	// StateRunning means the ready checks passed and the preview is serving.
	StateRunning State = "running"
	// StateFailed means the last boot did not come up. The row is kept so the
	// failure can be reported; the next explicit start clears it.
	StateFailed State = "failed"
)

// IsLive reports whether the state implies a VM the manager is holding
// resources for.
func (s State) IsLive() bool { return s == StateBooting || s == StateRunning }

// App is one addressable server inside a preview, with its hostname already
// resolved against the project's domain.
type App struct {
	Name string
	Host string
	Port uint16
}

// Preview is the durable record of one preview environment.
type Preview struct {
	// ID is the stable identity, assigned by the caller. It is what the boot
	// singleflight and the log buffers are keyed on.
	ID string
	// Project and Ref are what the preview is *of*. Together they are unique:
	// asking for a preview of a ref that already has one returns that one.
	Project string
	Ref     string

	State State
	// Apps are the resolved hostnames and guest ports. They are recorded rather
	// than recomputed so a restart can rebuild the route table without cloning
	// the repository first — the hostname has to answer before the boot that
	// would tell us the hostname.
	Apps []App

	// SessionID is the session the current (or most recent) boot narrated
	// itself through. It is how the holding page reports real progress.
	SessionID string
	// Error is why the last boot failed, when State is StateFailed.
	Error string

	CreatedAt time.Time
	UpdatedAt time.Time
	// StartedAt is when the current VM booted; zero when nothing is running.
	StartedAt time.Time
	// LastRequestAt is stamped by ingress on every request and is what idle
	// reaping measures.
	LastRequestAt time.Time
	// ExpiresAt is the hard deadline the current VM is stopped at regardless of
	// traffic. Zero means no cap.
	ExpiresAt time.Time
}

// Hosts returns every hostname that routes to this preview.
func (p *Preview) Hosts() []string {
	out := make([]string, 0, len(p.Apps))
	for _, a := range p.Apps {
		out = append(out, a.Host)
	}
	return out
}

// AppForHost returns the app serving a hostname, matched exactly. Ingress does
// not fall back to a default app: a request for a name nothing claims is a
// misconfiguration, and quietly serving it from some other app produces a
// preview that looks like it works and is wrong.
func (p *Preview) AppForHost(host string) (App, bool) {
	host = NormalizeHost(host)
	for _, a := range p.Apps {
		if a.Host == host {
			return a, true
		}
	}
	return App{}, false
}

// PrimaryURL is the address to hand a person who asked for "the" preview: the
// app named "web" if there is one, else the first by name.
func (p *Preview) PrimaryURL() string {
	if len(p.Apps) == 0 {
		return ""
	}
	best := p.Apps[0]
	for _, a := range p.Apps {
		if a.Name == "web" {
			best = a
			break
		}
		if a.Name < best.Name {
			best = a
		}
	}
	return "https://" + best.Host
}

// NormalizeHost reduces a Host header to the name previews are keyed on:
// lowercase, no port, no trailing dot. Everything that looks up or stores a
// hostname goes through here, so the route table cannot be defeated by a
// request that spells the name differently.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(host)
	// Strip the port. Doing it by hand rather than with net.SplitHostPort so a
	// bare hostname (the common case) does not go through an error path.
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], "]") {
		if !strings.Contains(host[:i], ":") || strings.HasPrefix(host, "[") {
			host = host[:i]
		}
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// ID builds the stable identity for a preview of a ref. It is derived rather
// than random so the same ref always reaches the same record, singleflight key
// and log buffer, across restarts.
func ID(project, ref string) string {
	return fmt.Sprintf("%s/%s", project, ref)
}

// Store is the durable half of the preview manager: which previews exist, what
// they are called, and when each was last wanted.
//
// It holds no VM state. Everything a running preview owns — the sandbox
// session, the scheduler lease, the dial path — lives in memory and is gone
// after a restart, which is exactly why the parts that must survive one are
// separated out here.
type Store interface {
	// Put inserts or replaces a preview. It returns ErrHostTaken when one of
	// the preview's hostnames belongs to a different preview.
	Put(ctx context.Context, p *Preview) error
	// Get returns the preview by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (*Preview, error)
	// FindByHost returns the preview claiming a hostname, or ErrNotFound. The
	// host is normalized before lookup.
	FindByHost(ctx context.Context, host string) (*Preview, error)
	// List returns every preview, ordered by ID.
	List(ctx context.Context) ([]*Preview, error)
	// Delete removes a preview and releases its hostnames, or ErrNotFound.
	Delete(ctx context.Context, id string) error
	// TouchRequest stamps a preview's last-request time, which is what idle
	// reaping measures. It is a dedicated operation because ingress calls it on
	// every request and must not read-modify-write the whole row to do it.
	// A preview that no longer exists is not an error: the request raced a
	// delete and there is nothing to record.
	TouchRequest(ctx context.Context, id string, at time.Time) error
	// ResetLive moves every non-stopped preview back to stopped, clearing the
	// VM-lifetime fields. It is startup reconciliation: the VMs those rows
	// referred to died with the previous process, so the rows have to say
	// "stopped" for the next request to boot them rather than route into
	// nothing. Returns the IDs it reset.
	ResetLive(ctx context.Context) ([]string, error)

	Close() error
}
