package dispatch

import (
	"io"
	"sync"
	"sync/atomic"

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

// PendingConn is one TCP connection the runner has been asked to open inside
// the guest and carry over the bridge. The two directions are independent
// streams, so both halves live here for as long as the connection does; the
// side that owns the connection removes it when it is finished with it, not
// whichever stream ends first.
type PendingConn struct {
	// Reader yields what the client outside the VM sent. The ReadConnection
	// stream drains it into the guest socket.
	Reader io.ReadCloser
	// Writer receives what the guest server answered. The WriteConnection
	// stream fills it.
	Writer io.WriteCloser
}

// PendingRunner holds the channels used to communicate with a runner that has
// registered with the bridge service.
type PendingRunner struct {
	CommandCh chan *v1.RunnerCommand
	ResultCh  chan *v1.CommandResult
	OutputCh  chan *v1.OutputChunk
	// ProcessEventCh carries unsolicited notifications about long-lived
	// processes. They arrive on their own channel rather than on ResultCh
	// because no command is waiting for them: the command that started the
	// process was answered the moment it was running.
	ProcessEventCh chan *v1.ProcessEvent
	DoneCh         chan struct{}
	doneOnce       sync.Once
	VmInfo         *v1.VmInfo

	// RegisteredOnce gates the long-lived Register stream so that exactly one
	// caller can own it at a time. It is cleared when the stream returns so a
	// runner that crashes and is restarted by systemd can re-register cleanly.
	// Atomic rather than sync.Once because we need to *reset* it on
	// reconnect.
	RegisteredOnce atomic.Bool

	// ExpectedCID is the peer vsock CID that owns this token, recorded the
	// first time Register succeeds. Subsequent bridge RPCs that carry the
	// same token must come from the same CID — a second process inside the
	// VM that learned the token cannot impersonate the runner from a
	// different vsock socket. Zero means "not yet bound" or "peer CID was
	// not extractable from the connection" (e.g. macOS vz transport).
	ExpectedCID atomic.Uint32

	mu        sync.Mutex
	transfers map[string]*PendingTransfer
	conns     map[string]*PendingConn
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

// RegisterConn registers a proxied connection by ID, before the command that
// asks the runner to dial is sent: the runner opens its streams as soon as the
// connection is up, which can be before the command's own result gets back.
func (pr *PendingRunner) RegisterConn(id string, c *PendingConn) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.conns == nil {
		pr.conns = make(map[string]*PendingConn)
	}
	pr.conns[id] = c
}

// LookupConn returns the PendingConn for the given ID, if any.
func (pr *PendingRunner) LookupConn(id string) (*PendingConn, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	c, ok := pr.conns[id]
	return c, ok
}

// RemoveConn deletes a proxied connection by ID.
func (pr *PendingRunner) RemoveConn(id string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.conns, id)
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
		CommandCh:      make(chan *v1.RunnerCommand, 1),
		ResultCh:       make(chan *v1.CommandResult, 1),
		OutputCh:       make(chan *v1.OutputChunk, 64),
		ProcessEventCh: make(chan *v1.ProcessEvent, 16),
		DoneCh:         make(chan struct{}),
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
