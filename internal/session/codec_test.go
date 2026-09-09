package session

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("agent_turn payload codec", func() {
	It("round-trips a started turn as a durable event", func() {
		kind, payload, durable, err := encodeEvent(AgentTurnEvent{
			SessionID: "s1",
			Phase:     AgentTurnStarted,
			Step:      7,
			Model:     "anthropic/claude-sonnet-4-6",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(kindAgentTurn))
		Expect(durable).To(BeTrue())

		ev, err := decodeEvent(kind, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev).To(Equal(AgentTurnEvent{
			SessionID: "s1",
			Phase:     AgentTurnStarted,
			Step:      7,
			Model:     "anthropic/claude-sonnet-4-6",
		}))
	})

	It("round-trips an ended turn with its agent and final flag", func() {
		kind, payload, durable, err := encodeEvent(AgentTurnEvent{
			SessionID: "s1",
			AgentID:   "plan/ab12",
			Phase:     AgentTurnEnded,
			Step:      2,
			Final:     true,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(durable).To(BeTrue())

		ev, err := decodeEvent(kind, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev).To(Equal(AgentTurnEvent{
			SessionID: "s1",
			AgentID:   "plan/ab12",
			Phase:     AgentTurnEnded,
			Step:      2,
			Final:     true,
		}))
	})

	It("keeps the responding phase out of the durable log", func() {
		_, _, durable, err := encodeEvent(AgentTurnEvent{
			SessionID: "s1",
			Phase:     AgentTurnResponding,
			Step:      7,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(durable).To(BeFalse())
	})
})

var _ = Describe("agent_retry payload codec", func() {
	It("round-trips an attempt including its delay in milliseconds", func() {
		kind, payload, durable, err := encodeEvent(AgentRetryEvent{
			SessionID:   "s1",
			Step:        3,
			Attempt:     2,
			MaxAttempts: 10,
			Delay:       1500 * time.Millisecond,
			StatusCode:  503,
			Provider:    "anthropic",
			Model:       "claude-sonnet-4-6",
			Error:       "overloaded",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(kindAgentRetry))
		Expect(durable).To(BeTrue())

		var raw map[string]any
		Expect(json.Unmarshal(payload, &raw)).To(Succeed())
		Expect(raw).To(HaveKeyWithValue("delay_ms", float64(1500)))

		ev, err := decodeEvent(kind, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev).To(Equal(AgentRetryEvent{
			SessionID:   "s1",
			Step:        3,
			Attempt:     2,
			MaxAttempts: 10,
			Delay:       1500 * time.Millisecond,
			StatusCode:  503,
			Provider:    "anthropic",
			Model:       "claude-sonnet-4-6",
			Error:       "overloaded",
		}))
	})

	It("trims an error too large for the payload cap", func() {
		_, payload, _, err := encodeEvent(AgentRetryEvent{
			SessionID: "s1",
			Error:     strings.Repeat("x", payloadCap*2),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(payload)).To(BeNumerically("<=", payloadCap))

		ev, err := decodeEvent(kindAgentRetry, payload)
		Expect(err).NotTo(HaveOccurred())
		Expect(ev.(AgentRetryEvent).Error).To(HaveSuffix(truncationMarker))
	})
})

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
