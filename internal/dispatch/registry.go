package dispatch

import (
	"io"
	"sync"

	"errors"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// PendingTransfer tracks an in-flight file transfer between orchestrator and runner.
type PendingTransfer struct {
	Reader io.ReadCloser       // for downloads (orchestrator→runner)
	Writer io.WriteCloser      // for uploads (runner→orchestrator)
	Meta   *v1.FileStreamStart // metadata for the transfer
	Done   chan struct{}       // closed when the transfer completes
}

// PendingRunner holds the channels used to communicate with a runner that has
// registered with the bridge service.
type PendingRunner struct {
	CommandCh chan *v1.RunnerCommand
	ResultCh  chan *v1.CommandResult
	OutputCh  chan *v1.OutputChunk
	DoneCh    chan struct{}
	doneOnce  sync.Once
	VmInfo    *v1.VmInfo

	mu        sync.Mutex
	transfers map[string]*PendingTransfer
}

// MarkReady signals that the runner is connected. Safe to call multiple times.
func (pr *PendingRunner) MarkReady() {
	pr.doneOnce.Do(func() { close(pr.DoneCh) })
}

// RegisterTransfer registers a pending file transfer by ID.
func (pr *PendingRunner) RegisterTransfer(id string, t *PendingTransfer) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.transfers == nil {
		pr.transfers = make(map[string]*PendingTransfer)
	}
	pr.transfers[id] = t
}

// LookupTransfer returns the PendingTransfer for the given ID, if any.
func (pr *PendingRunner) LookupTransfer(id string) (*PendingTransfer, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	t, ok := pr.transfers[id]
	return t, ok
}

// RemoveTransfer deletes a pending transfer by ID.
func (pr *PendingRunner) RemoveTransfer(id string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.transfers, id)
}

// Registry tracks pending runners by their bootstrap token.
type Registry struct {
	mu      sync.Mutex
	pending map[string]*PendingRunner
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		pending: make(map[string]*PendingRunner),
	}
}

// Register creates a PendingRunner for the given token.
// Returns an error if the token is already registered.
func (r *Registry) Register(token string) (*PendingRunner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.pending[token]; exists {
		return nil, errors.New("token already registered")
	}

	pr := &PendingRunner{
		CommandCh: make(chan *v1.RunnerCommand, 1),
		ResultCh:  make(chan *v1.CommandResult, 1),
		OutputCh:  make(chan *v1.OutputChunk, 64),
		DoneCh:    make(chan struct{}),
	}
	r.pending[token] = pr
	return pr, nil
}

// Lookup returns the PendingRunner for the given token, if any.
func (r *Registry) Lookup(token string) (*PendingRunner, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr, ok := r.pending[token]
	return pr, ok
}

// Remove deletes the PendingRunner for the given token.
func (r *Registry) Remove(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, token)
}
