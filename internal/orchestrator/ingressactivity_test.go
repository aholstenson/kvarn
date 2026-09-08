package orchestrator

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/preview"
)

// The cases here are written as the headers a real client sends, because that
// is the whole substance of the grading: what separates a person opening a page
// from that page polling afterwards is not the path or the method, it is the
// fetch metadata the browser attaches.

var _ = Describe("requestActivity", func() {
	request := func(method string, headers map[string]string) *http.Request {
		r, err := http.NewRequest(method, "http://pr-7.preview.example.com/", nil)
		Expect(err).NotTo(HaveOccurred())
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	DescribeTable("grading a request",
		func(method string, headers map[string]string, want preview.Activity) {
			Expect(requestActivity(request(method, headers))).To(Equal(want))
		},

		Entry("a browser navigating to the page", http.MethodGet, map[string]string{
			"Sec-Fetch-Dest": "document",
			"Sec-Fetch-Mode": "navigate",
			"Accept":         "text/html,application/xhtml+xml",
		}, preview.ActivityAttention),

		Entry("a page loaded in a frame", http.MethodGet, map[string]string{
			"Sec-Fetch-Dest": "iframe",
			"Sec-Fetch-Mode": "navigate",
		}, preview.ActivityAttention),

		Entry("a form submission", http.MethodPost, map[string]string{
			"Sec-Fetch-Dest": "document",
			"Sec-Fetch-Mode": "navigate",
		}, preview.ActivityAttention),

		Entry("an asset the page pulled in", http.MethodGet, map[string]string{
			"Sec-Fetch-Dest": "script",
			"Sec-Fetch-Mode": "no-cors",
		}, preview.ActivityBackground),

		Entry("a poll from a tab nobody is looking at", http.MethodGet, map[string]string{
			"Sec-Fetch-Dest": "empty",
			"Sec-Fetch-Mode": "cors",
			"Accept":         "application/json",
		}, preview.ActivityBackground),

		// The reason fetch metadata is read before the method: a poll is not
		// always a GET, and a POST that says "empty" is still a poll.
		Entry("a poll that posts, as a GraphQL client does", http.MethodPost, map[string]string{
			"Sec-Fetch-Dest": "empty",
			"Sec-Fetch-Mode": "cors",
			"Content-Type":   "application/json",
		}, preview.ActivityBackground),

		Entry("a preflight for one", http.MethodOptions, map[string]string{
			"Sec-Fetch-Dest": "empty",
			"Sec-Fetch-Mode": "cors",
		}, preview.ActivityBackground),

		// Nothing but a browser sends fetch metadata, so its absence falls back
		// on what the client asked for and what it is doing.
		Entry("a client asking for a page without fetch metadata", http.MethodGet, map[string]string{
			"Accept": "text/html,application/xhtml+xml;q=0.9",
		}, preview.ActivityAttention),

		Entry("a submission without fetch metadata", http.MethodPost, map[string]string{
			"Accept": "*/*",
		}, preview.ActivityAttention),

		Entry("an uptime monitor", http.MethodHead, map[string]string{
			"Accept":     "*/*",
			"User-Agent": "Some-Monitor/1.0",
		}, preview.ActivityBackground),

		Entry("a bare curl", http.MethodGet, map[string]string{
			"Accept": "*/*",
		}, preview.ActivityBackground),

		Entry("a request with no headers at all", http.MethodGet, nil, preview.ActivityBackground),
	)
})
