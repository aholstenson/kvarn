package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/cockroachdb/errors"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

type watcher struct {
	ch     chan Event
	ctx    context.Context
	closed bool
}

// MemoryManager is an in-memory session manager.
type MemoryManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	watchers map[string][]*watcher
}

// NewMemoryManager creates a new in-memory session manager.
func NewMemoryManager() *MemoryManager {
	return &MemoryManager{
		sessions: make(map[string]*Session),
		watchers: make(map[string][]*watcher),
	}
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (m *MemoryManager) Create(_ context.Context, projectName string, prompt string, mode string) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, errors.Wrap(err, "generate session id")
	}

	now := time.Now()
	s := &Session{
		ID:          id,
		ProjectName: projectName,
		Prompt:      prompt,
		Mode:        mode,
		State:       StatePending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	return copySession(s), nil
}

func (m *MemoryManager) Get(_ context.Context, id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[id]
	if !ok {
		return nil, errors.Newf("session %q not found", id)
	}
	return copySession(s), nil
}

func (m *MemoryManager) List(_ context.Context) ([]*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Session
	for _, s := range m.sessions {
		result = append(result, copySession(s))
	}
	return result, nil
}

func (m *MemoryManager) UpdateState(_ context.Context, id string, state State, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return errors.Newf("session %q not found", id)
	}

	s.State = state
	s.Message = message
	s.UpdatedAt = time.Now()

	m.notifyStateChange(id, s)
	return nil
}

func (m *MemoryManager) Fail(_ context.Context, id string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return errors.Newf("session %q not found", id)
	}

	s.State = StateFailed
	s.Error = err.Error()
	s.UpdatedAt = time.Now()

	m.notifyStateChange(id, s)
	return nil
}

func (m *MemoryManager) UpdateCost(_ context.Context, id string, report cost.Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return errors.Newf("session %q not found", id)
	}

	s.Cost = report
	s.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryManager) EmitEvent(_ context.Context, id string, event Event) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, ok := m.sessions[id]; !ok {
		return errors.Newf("session %q not found", id)
	}

	m.broadcast(id, event)
	return nil
}

func (m *MemoryManager) Watch(ctx context.Context, id string) (<-chan Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return nil, errors.Newf("session %q not found", id)
	}

	// If already terminal, return a closed channel with the final state.
	if s.State.IsTerminal() {
		ch := make(chan Event, 1)
		ch <- StateChangeEvent{Session: copySession(s)}
		close(ch)
		return ch, nil
	}

	ch := make(chan Event, 16)
	w := &watcher{ch: ch, ctx: ctx}
	m.watchers[id] = append(m.watchers[id], w)

	// Clean up watcher on context cancellation.
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		if !w.closed {
			w.closed = true
			m.removeWatcher(id, w)
			close(ch)
		}
	}()

	return ch, nil
}

// notifyStateChange sends a state change event to all active watchers and
// handles terminal state cleanup. Must be called with mu held.
func (m *MemoryManager) notifyStateChange(id string, s *Session) {
	event := StateChangeEvent{Session: copySession(s)}
	watchers := m.watchers[id]
	var active []*watcher
	for _, w := range watchers {
		if w.ctx.Err() != nil {
			continue
		}
		select {
		case w.ch <- event:
		default:
			// Drop if watcher is slow.
		}
		if s.State.IsTerminal() {
			w.closed = true
			close(w.ch)
		} else {
			active = append(active, w)
		}
	}
	if s.State.IsTerminal() {
		delete(m.watchers, id)
	} else {
		m.watchers[id] = active
	}
}

// broadcast sends an event to all active watchers. Must be called with mu held (read or write).
func (m *MemoryManager) broadcast(id string, event Event) {
	for _, w := range m.watchers[id] {
		if w.ctx.Err() != nil {
			continue
		}
		select {
		case w.ch <- event:
		default:
			// Drop if watcher is slow.
		}
	}
}

func (m *MemoryManager) removeWatcher(id string, target *watcher) {
	watchers := m.watchers[id]
	for i, w := range watchers {
		if w == target {
			m.watchers[id] = append(watchers[:i], watchers[i+1:]...)
			return
		}
	}
}

func copySession(s *Session) *Session {
	cp := *s
	return &cp
}
