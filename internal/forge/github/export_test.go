package github

import "time"

// HTTPClientTimeout returns the timeout of the HTTP client g sends with.
func HTTPClientTimeout(g *GitHub) time.Duration {
	return g.httpClient.Timeout
}
