package rcsdirect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	sendTimeout    = 3 * time.Minute
	maxBody        = 1 << 20
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: sendTimeout},
	}
}

func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

type SIM struct {
	ParticipantID string `json:"participantId"`
	Carrier       string `json:"carrier"`
	Number        string `json:"number"`
	RCSEnabled    bool   `json:"rcsEnabled"`
}

type Check struct {
	To             string `json:"to"`
	ConversationID string `json:"conversationId"`
	Transport      string `json:"transport"`
	SendMode       string `json:"sendMode"`
	E2EE           string `json:"e2ee"`
	E2EEEvidence   string `json:"e2eeEvidence,omitempty"`
	E2EEEvidenceAt string `json:"e2eeEvidenceAt,omitempty"`
	ReadOnly       bool   `json:"readOnly"`
	SIM            *SIM   `json:"sim,omitempty"`
	// ConversationTypeProbe is the sender's experimental GET_CONVERSATION_TYPE
	// output. Its fields are unnamed on the wire; nothing is decided on them.
	ConversationTypeProbe *ConversationTypeProbe `json:"conversationTypeProbe,omitempty"`
	ConversationFields    []string               `json:"conversationFields,omitempty"`
	// E2EECapability is what the phone's capability lookup says about the
	// contact: "yes" (encrypted RCS), "not_yet" (nothing recorded; the probe
	// starts the lookup, and it is also what an iPhone reads), or "no" (a
	// recorded lookup without it). Empty when the probe failed. It is known
	// before the chat is finalized, which E2EE is not. See rcsphone.Capability.
	E2EECapability string `json:"e2eeCapability,omitempty"`
	Eligible       bool   `json:"eligible"`
	Reason         string `json:"reason,omitempty"`
}

type ConversationTypeProbe struct {
	Type          int32       `json:"type"`
	Bool1         bool        `json:"bool1"`
	Number2       int32       `json:"number2"`
	Capability    *Capability `json:"capability,omitempty"`
	UnknownFields []string    `json:"unknownFields,omitempty"`
	Error         string      `json:"error,omitempty"`
}

// Capability is the decoded field 8 of the type probe, see rcsphone.Capability.
type Capability struct {
	Flag1       uint64 `json:"flag1"`
	Flag2       uint64 `json:"flag2"`
	Version     string `json:"version,omitempty"`
	RefreshedAt string `json:"refreshedAt,omitempty"`
}

// Nudge actions: the calls the Messages web client makes when a thread is
// opened, tried one at a time against a chat the phone has not finalized.
// See rcsphone.Agent.Nudge.
const (
	NudgeGetConversation = "get_conversation"
	NudgeMarkRead        = "mark_read"
	NudgeTyping          = "typing"
	NudgeAll             = "all"
)

// NudgeSteps are the single actions, in the order NudgeAll runs them.
var NudgeSteps = []string{NudgeGetConversation, NudgeMarkRead, NudgeTyping}

func ValidNudge(action string) bool {
	if action == NudgeAll {
		return true
	}
	for _, a := range NudgeSteps {
		if a == action {
			return true
		}
	}
	return false
}

type NudgeStep struct {
	Action string   `json:"action"`
	Result []string `json:"result,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type Nudge struct {
	To             string      `json:"to"`
	ConversationID string      `json:"conversationId"`
	Before         *Check      `json:"before"`
	Steps          []NudgeStep `json:"steps"`
	After          *Check      `json:"after"`
	Changed        bool        `json:"changed"`
}

type SendResult struct {
	Sent          bool     `json:"sent"`
	MessageID     string   `json:"messageId,omitempty"`
	TmpID         string   `json:"tmpId"`
	FinalStatus   string   `json:"finalStatus"`
	StatusHistory []string `json:"statusHistory"`
	MessageType   int64    `json:"messageType"`
	Check         *Check   `json:"check"`
	// After is the chat re-read once the send went out. Sending is what
	// finalizes a new chat, so this is where its E2EE first becomes known.
	// Nil when the chat could not be re-read.
	After *Check `json:"after,omitempty"`
}

func (r *SendResult) Transport() string {
	if r == nil {
		return "unknown"
	}
	switch r.MessageType {
	case 1:
		return "SMS"
	case 2, 3:
		return "MMS"
	case 4:
		return "RCS"
	}
	return "unknown"
}

func (r *SendResult) DeliveredAsRCS() bool {
	return r != nil && r.Sent && r.MessageType == 4
}

type Health struct {
	LoggedIn        bool   `json:"loggedIn"`
	Connected       bool   `json:"connected"`
	PhoneResponding bool   `json:"phoneResponding"`
	SessionActive   bool   `json:"sessionActive"`
	SIMs            []SIM  `json:"sims"`
	LastError       string `json:"lastError"`
}

func (h *Health) Ready() bool {
	if h == nil || !h.LoggedIn || !h.Connected || !h.PhoneResponding || !h.SessionActive {
		return false
	}
	for _, s := range h.SIMs {
		if s.RCSEnabled {
			return true
		}
	}
	return false
}

var ErrNotConfigured = errors.New("rcs sender has no base URL configured")

var transientReasons = map[string]bool{
	"phone_not_responding": true,
	"session_not_active":   true,
	// The phone has not looked the contact up yet (rcs_pending), or has but
	// has not said whether the chat is encrypted (e2ee_pending); a later
	// attempt can find that it has.
	"rcs_pending":  true,
	"e2ee_pending": true,
	// The Android phone's screen was locked or not on a chat: a later
	// attempt can find it unlocked and ready.
	"phone_locked":  true,
	"chat_not_open": true,
}

type RefusedError struct {
	Reason string
	Check  *Check
}

func (e *RefusedError) Error() string { return "rcs refused: " + e.Reason }

func (e *RefusedError) Permanent() bool { return !transientReasons[e.Reason] }

type badRequestError struct{ msg string }

func (e *badRequestError) Error() string   { return e.msg }
func (e *badRequestError) Permanent() bool { return true }

type SendOptions struct {
	RequireE2EE *bool
	Wait        time.Duration
}

func (c *Client) Send(ctx context.Context, to, message string, opts SendOptions) (*SendResult, error) {
	body := map[string]any{"to": to, "message": message}
	if opts.RequireE2EE != nil {
		body["requireE2ee"] = *opts.RequireE2EE
	}
	if opts.Wait > 0 {
		body["waitSeconds"] = int(opts.Wait.Seconds())
	}

	var out SendResult
	if err := c.post(ctx, "/send", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Check(ctx context.Context, to string) (*Check, error) {
	var out Check
	if err := c.post(ctx, "/check", map[string]any{"to": to}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var out Health
	if err := c.get(ctx, "/health", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type ThreadMessage struct {
	MessageID    string `json:"messageId"`
	Timestamp    string `json:"timestamp"`
	Outgoing     bool   `json:"outgoing"`
	Status       string `json:"status"`
	Type         int64  `json:"type"`
	Transport    string `json:"transport"`
	E2EEAtSend   string `json:"e2eeAtSend"`
	E2EEEvidence string `json:"e2eeEvidence,omitempty"`
	Text         string `json:"text,omitempty"`
}

type Thread struct {
	Check    *Check          `json:"check"`
	Messages []ThreadMessage `json:"messages"`
}

func (c *Client) Thread(ctx context.Context, to string, limit int) (*Thread, error) {
	q := url.Values{}
	q.Set("to", to)
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out Thread
	if err := c.get(ctx, "/thread?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	return http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rcs sender unreachable: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("reading rcs sender response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusConflict:
		var refused struct {
			Reason string `json:"reason"`
			Check  *Check `json:"check"`
		}
		_ = json.Unmarshal(raw, &refused)
		if refused.Reason == "" {
			refused.Reason = "refused"
		}
		return &RefusedError{Reason: refused.Reason, Check: refused.Check}

	case resp.StatusCode == http.StatusBadRequest:
		return &badRequestError{msg: "rcs sender rejected the request: " + errorText(raw)}

	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("rcs sender returned %d: %s", resp.StatusCode, errorText(raw))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("rcs sender sent malformed JSON: %w", err)
	}
	return nil
}

func errorText(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return body.Error
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}
