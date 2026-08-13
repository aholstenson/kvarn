package preview

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store for tests. It enforces the same hostname
// uniqueness the SQLite store does, so a spec that would only fail against a
// real database fails here too.
type memStore struct {
	mu        sync.Mutex
	previews  map[string]*Preview
	hostIndex map[string]string // host -> preview ID
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{
		previews:  make(map[string]*Preview),
		hostIndex: make(map[string]string),
	}
}

// clone copies a preview so callers cannot mutate stored state by holding onto
// what they were handed — the SQLite store hands back fresh rows, and the two
// have to behave the same.
func clone(p *Preview) *Preview {
	out := *p
	out.Apps = append([]App(nil), p.Apps...)
	return &out
}

func (m *memStore) Put(_ context.Context, p *Preview) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, app := range p.Apps {
		host := NormalizeHost(app.Host)
		if owner, taken := m.hostIndex[host]; taken && owner != p.ID {
			return ErrHostTaken
		}
	}

	// Release the hostnames the previous version of this preview claimed; its
	// apps may have changed since.
	for host, owner := range m.hostIndex {
		if owner == p.ID {
			delete(m.hostIndex, host)
		}
	}

	stored := clone(p)
	for i, app := range stored.Apps {
		stored.Apps[i].Host = NormalizeHost(app.Host)
		m.hostIndex[stored.Apps[i].Host] = p.ID
	}
	m.previews[p.ID] = stored
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (*Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.previews[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

func (m *memStore) FindByHost(_ context.Context, host string) (*Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.hostIndex[NormalizeHost(host)]
	if !ok {
		return nil, ErrNotFound
	}
	p, ok := m.previews[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

func (m *memStore) List(_ context.Context) ([]*Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Preview, 0, len(m.previews))
	for _, p := range m.previews {
		out = append(out, clone(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.previews[id]; !ok {
		return ErrNotFound
	}
	delete(m.previews, id)
	for host, owner := range m.hostIndex {
		if owner == id {
			delete(m.hostIndex, host)
		}
	}
	return nil
}

func (m *memStore) TouchRequest(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.previews[id]
	if !ok {
		return nil
	}
	p.LastRequestAt = at
	return nil
}

func (m *memStore) ResetLive(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var reset []string
	for id, p := range m.previews {
		if p.State == StateStopped {
			continue
		}
		p.State = StateStopped
		p.StartedAt = time.Time{}
		p.ExpiresAt = time.Time{}
		p.SessionID = ""
		reset = append(reset, id)
	}
	sort.Strings(reset)
	return reset, nil
}

func (m *memStore) Close() error { return nil }
