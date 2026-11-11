package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// APIError is an answer from a forge API that was not a success. It carries
// the status code so a caller can tell a request worth repeating from one
// that will fail the same way every time.
type APIError struct {
	// Op names the operation, for the error text.
	Op string
	// StatusCode is the HTTP status the forge answered with.
	StatusCode int
	// Body is the response body, as the forge sent it.
	Body string
	// RetryAfter is how long the forge asked the caller to wait before the
	// next request, or zero when it did not say.
	RetryAfter time.Duration
	// RateLimited records that the forge refused the request because of a
	// rate limit. GitHub answers some rate limits with 403, which otherwise
	// means a request that will never succeed.
	RateLimited bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.StatusCode, e.Body)
}

// IsTransient reports whether err is a failure that the same request can
// succeed after: the forge could not be reached, it had a problem of its own,
// or it asked the caller to slow down. Every other answer from the forge is
// a decision about the request itself, and repeating it changes nothing.
//
// An error that is not an APIError and not a cancellation is treated as a
// failure to reach the forge, because that is what every such error from a
// forge client is: a dial, a TLS handshake, or a connection that broke.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// The marker is checked first: a file-system error wrapped in Permanent
	// also satisfies net.Error, and the marker is the caller's own decision.
	if errors.Is(err, context.Canceled) || errors.Is(err, errPermanent) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.RateLimited:
			return true
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return true
		case apiErr.StatusCode == http.StatusRequestTimeout:
			return true
		case apiErr.StatusCode >= 500:
			return true
		default:
			return false
		}
	}
	return true
}

// errPermanent marks an error that never succeeds on a retry, for errors that
// are not answers from the forge (a malformed reference, for example).
var errPermanent = errors.New("permanent")

// Permanent wraps err so that IsTransient reports false for it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (%w)", err, errPermanent)
}

// RetryAfterOf returns how long the forge asked the caller to wait before the
// next request, or zero when err carries no such request.
func RetryAfterOf(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}
