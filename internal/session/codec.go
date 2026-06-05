package session

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

// Durable event kinds. Only these are persisted to the event log; every other
// event is broadcast live-only. Keep this set in sync with the Store
// conformance suite and the codec switch below.
const (
	kindStateChange     = "state_change"
	kindAgentMessage    = "agent_message"
	kindAgentToolUse    = "agent_tool_use"
	kindAgentToolResult = "agent_tool_result"
	kindStepResult      = "step_result"
	kindCost            = "cost"
	kindPullRequest     = "pull_request"
	kindVMInfo          = "vm_info"
)

// payloadCap bounds the serialized size of a single persisted event payload.
// Live watchers still receive the full untruncated event; only the durable
// copy is trimmed so a single huge tool result can't bloat the database.
const payloadCap = 256 * 1024

// truncationMarker is appended to a string field that was trimmed to satisfy
// payloadCap. Paired with a `truncated` marker on the payload.
const truncationMarker = "…[truncated]"

// ToMicros / FromMicros convert between time.Time and the unix-microsecond UTC
// integer the session store persists. Centralised here so both stores (and the
// sqlite subpackage) agree on the wire representation of timestamps.
func ToMicros(t time.Time) int64    { return t.UTC().UnixMicro() }
func FromMicros(us int64) time.Time { return time.UnixMicro(us).UTC() }

// Row is the persisted column-for-column representation of a Session. Both
// stores round-trip sessions through SessionToRow/RowToSession so the cost-JSON
// encoding is exercised identically everywhere.
type Row struct {
	ID             string
	ProjectName    string
	Prompt         string
	Mode           string
	State          string
	Message        string
	Error          string
	PullRequestURL string
	CostJSON       string
	CreatedAt      int64 // unix micros UTC
	UpdatedAt      int64 // unix micros UTC
}

// SessionToRow converts a Session into its persisted Row form, marshalling the
// cost report to JSON.
func SessionToRow(s *Session) (Row, error) {
	costJSON := "{}"
	if b, err := json.Marshal(s.Cost); err != nil {
		return Row{}, fmt.Errorf("marshal cost: %w", err)
	} else {
		costJSON = string(b)
	}
	return Row{
		ID:             s.ID,
		ProjectName:    s.ProjectName,
		Prompt:         s.Prompt,
		Mode:           s.Mode,
		State:          string(s.State),
		Message:        s.Message,
		Error:          s.Error,
		PullRequestURL: s.PullRequestURL,
		CostJSON:       costJSON,
		CreatedAt:      ToMicros(s.CreatedAt),
		UpdatedAt:      ToMicros(s.UpdatedAt),
	}, nil
}

// RowToSession reconstructs a Session from its persisted Row form,
// unmarshalling the cost report from JSON.
func RowToSession(r Row) (*Session, error) {
	var report cost.Report
	if r.CostJSON != "" && r.CostJSON != "{}" {
		if err := json.Unmarshal([]byte(r.CostJSON), &report); err != nil {
			return nil, fmt.Errorf("unmarshal cost: %w", err)
		}
	}
	return &Session{
		ID:             r.ID,
		ProjectName:    r.ProjectName,
		Prompt:         r.Prompt,
		Mode:           r.Mode,
		State:          State(r.State),
		Message:        r.Message,
		Error:          r.Error,
		PullRequestURL: r.PullRequestURL,
		Cost:           report,
		CreatedAt:      FromMicros(r.CreatedAt),
		UpdatedAt:      FromMicros(r.UpdatedAt),
	}, nil
}

// EncodeStateChange returns the durable (kind, payload) for a session's current
// state. Store implementations use it when synthesising the state_change events
// that ReconcileNonTerminal appends.
func EncodeStateChange(s *Session) (kind string, payload []byte, err error) {
	kind, payload, _, err = encodeEvent(StateChangeEvent{Session: s})
	return kind, payload, err
}

// ---- Event payloads ----

type stateChangePayload struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	State     string `json:"state"`
	Message   string `json:"message"`
	Error     string `json:"error,omitempty"`
}

type agentMessagePayload struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Text      string `json:"text"`
	Final     bool   `json:"final"`
}

type agentToolUsePayload struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id,omitempty"`
	ToolID        string `json:"tool_id"`
	ArgumentsJSON string `json:"arguments_json"`
}

type agentToolResultPayload struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	ToolID    string `json:"tool_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	Truncated bool   `json:"truncated,omitempty"`
}

type stepResultPayload struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Phase     int    `json:"phase"`
	ExitCode  int32  `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Passed    bool   `json:"passed"`
	Skipped   bool   `json:"skipped"`
	Truncated bool   `json:"truncated,omitempty"`
}

type costPayload struct {
	SessionID string      `json:"session_id"`
	Kind      int         `json:"kind"`
	Report    cost.Report `json:"report"`
	Limit     cost.Limit  `json:"limit"`
}

type pullRequestPayload struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Number    int    `json:"number"`
	Branch    string `json:"branch"`
}

type vmInfoPayload struct {
	SessionID   string `json:"session_id"`
	CpuCount    int32  `json:"cpu_count"`
	CpuModel    string `json:"cpu_model"`
	MemTotalMB  int64  `json:"mem_total_mb"`
	MemAvailMB  int64  `json:"mem_avail_mb"`
	DiskUsedMB  int64  `json:"disk_used_mb"`
	DiskTotalMB int64  `json:"disk_total_mb"`
}

// encodeEvent maps a session Event to its durable (kind, payload). The boolean
// return is false for events that are broadcast live-only (ephemeral) — the
// caller must not persist those. Payloads are capped at payloadCap.
func encodeEvent(e Event) (kind string, payload []byte, durable bool, err error) {
	switch ev := e.(type) {
	case StateChangeEvent:
		p := stateChangePayload{
			SessionID: ev.Session.ID,
			Project:   ev.Session.ProjectName,
			State:     string(ev.Session.State),
			Message:   ev.Session.Message,
			Error:     ev.Session.Error,
		}
		b, err := json.Marshal(p)
		return kindStateChange, b, true, err
	case AgentMessageEvent:
		p := agentMessagePayload{
			SessionID: ev.SessionID,
			AgentID:   ev.AgentID,
			Text:      ev.Text,
			Final:     ev.Final,
		}
		b, err := marshalCapped(&p, func(remaining int) {
			p.Text = trimField(p.Text, remaining)
		})
		return kindAgentMessage, b, true, err
	case AgentToolUseEvent:
		p := agentToolUsePayload{
			SessionID:     ev.SessionID,
			AgentID:       ev.AgentID,
			ToolID:        ev.ToolID,
			ArgumentsJSON: ev.ArgumentsJSON,
		}
		b, err := marshalCapped(&p, func(remaining int) {
			p.ArgumentsJSON = trimField(p.ArgumentsJSON, remaining)
		})
		return kindAgentToolUse, b, true, err
	case AgentToolResultEvent:
		p := agentToolResultPayload{
			SessionID: ev.SessionID,
			AgentID:   ev.AgentID,
			ToolID:    ev.ToolID,
			Result:    ev.Result,
			IsError:   ev.IsError,
		}
		b, err := marshalCapped(&p, func(remaining int) {
			p.Result = trimField(p.Result, remaining)
			p.Truncated = true
		})
		return kindAgentToolResult, b, true, err
	case StepResultEvent:
		p := stepResultPayload{
			SessionID: ev.SessionID,
			Name:      ev.Name,
			Phase:     int(ev.Phase),
			ExitCode:  ev.ExitCode,
			Stdout:    ev.Stdout,
			Stderr:    ev.Stderr,
			Passed:    ev.Passed,
			Skipped:   ev.Skipped,
		}
		// Trim stdout first, then stderr if still over budget; split the
		// remaining allowance between them.
		b, err := marshalCapped(&p, func(remaining int) {
			half := remaining / 2
			p.Stdout = trimField(p.Stdout, half)
			p.Stderr = trimField(p.Stderr, remaining-half)
			p.Truncated = true
		})
		return kindStepResult, b, true, err
	case CostEvent:
		p := costPayload{
			SessionID: ev.SessionID,
			Kind:      int(ev.Kind),
			Report:    ev.Report,
			Limit:     ev.Limit,
		}
		b, err := json.Marshal(p)
		return kindCost, b, true, err
	case PullRequestEvent:
		p := pullRequestPayload{
			SessionID: ev.SessionID,
			URL:       ev.URL,
			Number:    ev.Number,
			Branch:    ev.Branch,
		}
		b, err := json.Marshal(p)
		return kindPullRequest, b, true, err
	case VmInfoEvent:
		p := vmInfoPayload{
			SessionID:   ev.SessionID,
			CpuCount:    ev.CpuCount,
			CpuModel:    ev.CpuModel,
			MemTotalMB:  ev.MemTotalMB,
			MemAvailMB:  ev.MemAvailMB,
			DiskUsedMB:  ev.DiskUsedMB,
			DiskTotalMB: ev.DiskTotalMB,
		}
		b, err := json.Marshal(p)
		return kindVMInfo, b, true, err
	default:
		return "", nil, false, nil
	}
}

// decodeEvent reconstructs a session Event from its persisted (kind, payload).
// Used for replaying history to watchers and for polling.
func decodeEvent(kind string, payload []byte) (Event, error) {
	switch kind {
	case kindStateChange:
		var p stateChangePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return StateChangeEvent{Session: &Session{
			ID:          p.SessionID,
			ProjectName: p.Project,
			State:       State(p.State),
			Message:     p.Message,
			Error:       p.Error,
		}}, nil
	case kindAgentMessage:
		var p agentMessagePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return AgentMessageEvent{SessionID: p.SessionID, AgentID: p.AgentID, Text: p.Text, Final: p.Final}, nil
	case kindAgentToolUse:
		var p agentToolUsePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return AgentToolUseEvent{SessionID: p.SessionID, AgentID: p.AgentID, ToolID: p.ToolID, ArgumentsJSON: p.ArgumentsJSON}, nil
	case kindAgentToolResult:
		var p agentToolResultPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return AgentToolResultEvent{SessionID: p.SessionID, AgentID: p.AgentID, ToolID: p.ToolID, Result: p.Result, IsError: p.IsError}, nil
	case kindStepResult:
		var p stepResultPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return StepResultEvent{
			SessionID: p.SessionID,
			Name:      p.Name,
			Phase:     StepPhase(p.Phase),
			ExitCode:  p.ExitCode,
			Stdout:    p.Stdout,
			Stderr:    p.Stderr,
			Passed:    p.Passed,
			Skipped:   p.Skipped,
		}, nil
	case kindCost:
		var p costPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return CostEvent{SessionID: p.SessionID, Kind: CostUpdateKind(p.Kind), Report: p.Report, Limit: p.Limit}, nil
	case kindPullRequest:
		var p pullRequestPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return PullRequestEvent{SessionID: p.SessionID, URL: p.URL, Number: p.Number, Branch: p.Branch}, nil
	case kindVMInfo:
		var p vmInfoPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return VmInfoEvent{
			SessionID:   p.SessionID,
			CpuCount:    p.CpuCount,
			CpuModel:    p.CpuModel,
			MemTotalMB:  p.MemTotalMB,
			MemAvailMB:  p.MemAvailMB,
			DiskUsedMB:  p.DiskUsedMB,
			DiskTotalMB: p.DiskTotalMB,
		}, nil
	default:
		return nil, fmt.Errorf("unknown event kind %q", kind)
	}
}

// marshalCapped marshals v; if the result exceeds payloadCap it invokes trim
// with the byte budget the variable-length field(s) may occupy, then
// re-marshals. trim mutates v in place (it captures pointers to v's fields).
func marshalCapped(v any, trim func(remaining int)) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) <= payloadCap {
		return b, nil
	}
	// Budget for the trimmable field(s) = cap minus the fixed overhead
	// (everything else in the serialized form). Reserve slack for the marker
	// and JSON escaping growth.
	overhead := len(b) - trimmableLen(v)
	remaining := payloadCap - overhead - 1024
	if remaining < 0 {
		remaining = 0
	}
	trim(remaining)
	b, err = json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// trimmableLen returns the combined length of the large free-text fields a
// payload type carries, so marshalCapped can size its trim budget.
func trimmableLen(v any) int {
	switch p := v.(type) {
	case *agentMessagePayload:
		return len(p.Text)
	case *agentToolUsePayload:
		return len(p.ArgumentsJSON)
	case *agentToolResultPayload:
		return len(p.Result)
	case *stepResultPayload:
		return len(p.Stdout) + len(p.Stderr)
	default:
		return 0
	}
}

// trimField truncates s to at most max bytes (snapped to a UTF-8 boundary) and
// appends the truncation marker. Returns s unchanged when it already fits.
func trimField(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= len(truncationMarker) {
		return truncationMarker
	}
	cut := max - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}
