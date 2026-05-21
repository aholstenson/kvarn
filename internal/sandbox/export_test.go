package sandbox

// NewTestSession returns an empty Session for use in unit tests.
func NewTestSession() *Session {
	return &Session{}
}

// AddCloserForTest registers a closer function on the session, mirroring the
// internal addCloser method so tests can exercise Close() without a real VM.
func (s *Session) AddCloserForTest(fn func()) {
	s.addCloser(fn)
}
