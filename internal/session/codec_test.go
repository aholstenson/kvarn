package session

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("pull_request payload codec", func() {
	It("encodes the ref as a string and never writes a number", func() {
		kind, payload, durable, err := encodeEvent(PullRequestEvent{
			SessionID: "s1",
			URL:       "https://example.com/pr/42",
			Ref:       "42",
			Branch:    "kvarn/thing",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(durable).To(BeTrue())
		Expect(kind).To(Equal(kindPullRequest))

		var raw map[string]any
		Expect(json.Unmarshal(payload, &raw)).To(Succeed())
		Expect(raw).To(HaveKeyWithValue("ref", "42"))
		Expect(raw).NotTo(HaveKey("number"))
	})

	It("decodes a row written with the string ref", func() {
		payload := []byte(`{"session_id":"s1","url":"https://example.com/pr/42","ref":"42","branch":"kvarn/thing"}`)
		ev, err := decodeEvent(kindPullRequest, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev).To(Equal(PullRequestEvent{
			SessionID: "s1",
			URL:       "https://example.com/pr/42",
			Ref:       "42",
			Branch:    "kvarn/thing",
		}))
	})

	It("decodes a legacy row that carries a numeric number", func() {
		payload := []byte(`{"session_id":"s1","url":"https://example.com/pr/42","number":42,"branch":"kvarn/thing"}`)
		ev, err := decodeEvent(kindPullRequest, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev).To(Equal(PullRequestEvent{
			SessionID: "s1",
			URL:       "https://example.com/pr/42",
			Ref:       "42",
			Branch:    "kvarn/thing",
		}))
	})

	It("re-encodes a decoded legacy row as a string ref", func() {
		ev, err := decodeEvent(kindPullRequest, []byte(`{"session_id":"s1","number":7}`))
		Expect(err).NotTo(HaveOccurred())

		_, payload, _, err := encodeEvent(ev)
		Expect(err).NotTo(HaveOccurred())
		var raw map[string]any
		Expect(json.Unmarshal(payload, &raw)).To(Succeed())
		Expect(raw).To(HaveKeyWithValue("ref", "7"))
		Expect(raw).NotTo(HaveKey("number"))
	})
})
