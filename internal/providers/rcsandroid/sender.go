// Package rcsandroid sends messages by driving the real Google Messages app on
// an Android phone over adb, the way the iPhone setup drives the Messages app
// on a Mac. It reads the answer to "will this go out encrypted" off the
// screen before sending, which the Messages for web protocol cannot tell.
package rcsandroid

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zagnu-labs/rcs-messenger/internal/logg"
	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsdirect"
)

type Config struct {
	Runner    Runner
	Selectors Selectors
	// PIN unlocks a phone with a numeric lock screen. Empty: a locked phone
	// is reported locked.
	PIN string
	// Addr is host:port of a Wi-Fi adb phone, used to reconnect. Empty for USB.
	Addr string

	// SettleBudget is how long a send waits for a new chat to show the padlock
	// (the phone's own lookup takes seconds to about a minute). CheckBudget is
	// the shorter wait a check makes.
	SettleBudget time.Duration
	CheckBudget  time.Duration
	// AfterBudget is how long after a send to wait for the chat to show what
	// it became.
	AfterBudget time.Duration
	PollEvery   time.Duration
	// MinGap and Jitter space sends out like a person: at least MinGap plus
	// up to Jitter since the last one.
	MinGap time.Duration
	Jitter time.Duration

	Sleep func(ctx context.Context, d time.Duration) error // test hook
	Now   func() time.Time                                 // test hook
}

func (c *Config) defaults() {
	if c.SettleBudget <= 0 {
		c.SettleBudget = 60 * time.Second
	}
	if c.CheckBudget <= 0 {
		c.CheckBudget = 10 * time.Second
	}
	if c.AfterBudget <= 0 {
		c.AfterBudget = 20 * time.Second
	}
	if c.PollEvery <= 0 {
		c.PollEvery = 2 * time.Second
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Sender is the Android phone as the rcs service's sender.
type Sender struct {
	cfg  Config
	dev  *Device
	mu   sync.Mutex // one thing on the phone at a time
	last time.Time  // when the last send went out

	hmu    sync.Mutex
	health *rcsdirect.Health
}

func New(cfg Config) (*Sender, error) {
	cfg.defaults()
	if cfg.Runner == nil {
		cfg.Runner = ExecRunner{}
	}
	dev, err := NewDevice(cfg.Runner, cfg.Selectors, cfg.PIN)
	if err != nil {
		return nil, err
	}
	return &Sender{cfg: cfg, dev: dev}, nil
}

func (s *Sender) Enabled() bool { return s != nil }

// ready makes sure the phone is reachable, awake and unlocked.
func (s *Sender) ready(ctx context.Context) error {
	if _, err := s.dev.State(ctx); err != nil {
		if rerr := s.dev.Reconnect(ctx, s.cfg.Addr); rerr != nil {
			return err
		}
	}
	_ = s.dev.KeepAwake(ctx)
	return s.dev.Awake(ctx)
}

func (s *Sender) read(ctx context.Context) (ChatView, *Screen, error) {
	scr, err := s.dev.Dump(ctx)
	if err != nil {
		return ChatView{Kind: KindUnknown, Evidence: err.Error()}, nil, err
	}
	return s.dev.m.ReadChat(scr), scr, nil
}

// settle reads the open chat until it shows the padlock or the budget runs
// out. A brand new chat reads "Text message" for a while before the phone has
// looked the contact up, so a single early reading means nothing.
func (s *Sender) settle(ctx context.Context, budget time.Duration) (ChatView, error) {
	start := s.cfg.Now()
	var last ChatView
	for {
		v, _, err := s.read(ctx)
		if err != nil && ctx.Err() != nil {
			return v, ctx.Err()
		}
		last = v
		if v.Kind == KindEncrypted {
			return v, nil
		}
		if s.cfg.Now().Sub(start)+s.cfg.PollEvery > budget {
			return last, nil
		}
		if err := s.cfg.Sleep(ctx, s.cfg.PollEvery); err != nil {
			return last, err
		}
	}
}

func toCheck(to string, v ChatView, final bool) *rcsdirect.Check {
	c := &rcsdirect.Check{To: to, SendMode: v.Hint, E2EEEvidence: v.Evidence, ReadOnly: false}
	switch v.Kind {
	case KindEncrypted:
		c.Transport, c.E2EE, c.E2EECapability = "RCS", "on", "yes"
	case KindRCS:
		c.Transport, c.E2EE, c.E2EECapability = "RCS", "off", "not_yet"
	case KindSMS:
		c.Transport, c.E2EE, c.E2EECapability = "SMS", "off", "not_yet"
		if final {
			c.E2EECapability = "no"
		}
	default:
		c.Transport, c.E2EE = "unknown", "unknown"
	}
	c.Eligible, c.Reason = eligibility(v)
	return c
}

func (s *Sender) Check(ctx context.Context, to string) (*rcsdirect.Check, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if err := s.dev.OpenChat(ctx, to); err != nil {
		return nil, err
	}
	v, err := s.settle(ctx, s.cfg.CheckBudget)
	if err != nil {
		return nil, err
	}
	return toCheck(to, v, false), nil
}

func (s *Sender) refused(reason string, c *rcsdirect.Check) error {
	return &rcsdirect.RefusedError{Reason: reason, Check: c}
}

// pace waits so sends are spaced like a person's.
func (s *Sender) pace(ctx context.Context) error {
	if s.last.IsZero() || s.cfg.MinGap <= 0 {
		return nil
	}
	gap := s.cfg.MinGap
	if s.cfg.Jitter > 0 {
		gap += time.Duration(rand.Int64N(int64(s.cfg.Jitter)))
	}
	if wait := gap - s.cfg.Now().Sub(s.last); wait > 0 {
		return s.cfg.Sleep(ctx, wait)
	}
	return nil
}

func (s *Sender) Send(ctx context.Context, to, message string, opts rcsdirect.SendOptions) (*rcsdirect.SendResult, error) {
	// opts.RequireE2EE is deliberately not read: encryption is always required.
	s.mu.Lock()
	defer s.mu.Unlock()

	wait := opts.Wait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if !typeable(message) {
		return nil, s.refused(ReasonUnsupported, nil)
	}
	if err := s.pace(ctx); err != nil {
		return nil, err
	}
	if err := s.ready(ctx); err != nil {
		if err.Error() == ReasonPhoneLocked {
			return nil, s.refused(ReasonPhoneLocked, nil)
		}
		return nil, err
	}
	if err := s.dev.OpenChat(ctx, to); err != nil {
		return nil, err
	}

	// 1. Know before sending: settle until the app shows this chat encrypted.
	before, err := s.settle(ctx, s.cfg.SettleBudget)
	if err != nil {
		return nil, err
	}
	// After a full settle an SMS chat is final, not pending.
	chk := toCheck(to, before, true)
	if !chk.Eligible {
		return nil, s.refused(chk.Reason, chk)
	}

	// 2. Type it and prove it landed in the box exactly, before pressing send.
	_, scr, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	box, ok := s.dev.m.compose(scr)
	if !ok {
		return nil, s.refused(ReasonChatNotOpen, chk)
	}
	x, y := box.Bounds.Center()
	if err := s.dev.Tap(ctx, x, y); err != nil {
		return nil, err
	}
	if err := s.dev.ClearCompose(ctx); err != nil {
		return nil, err
	}
	if err := s.dev.TypeText(ctx, message); err != nil {
		return nil, err
	}
	_, scr, err = s.read(ctx)
	if err != nil {
		return nil, err
	}
	typed, ok := s.dev.m.compose(scr)
	if !ok || norm(typed.Text) != norm(message) {
		_ = s.dev.ClearCompose(ctx)
		return nil, s.refused(ReasonTypingMismatch, chk)
	}
	btn, ok := s.dev.m.sendButton(scr)
	if !ok {
		_ = s.dev.ClearCompose(ctx)
		return nil, s.refused(ReasonUnreadable, chk)
	}
	// The hint is hidden now that text is typed, but the send button still
	// says how it will send ("Send SMS"): the last check before the tap.
	if s.dev.m.sendSMS.MatchString(norm(btn.Desc)) {
		_ = s.dev.ClearCompose(ctx)
		bad := toCheck(to, ChatView{Kind: KindSMS, ComposeFound: true, Evidence: `send button says "` + norm(btn.Desc) + `"`}, true)
		return nil, s.refused(ReasonNotE2EE, bad)
	}

	// 3. Send, then watch the thread for the status.
	bx, by := btn.Bounds.Center()
	if err := s.dev.Tap(ctx, bx, by); err != nil {
		return nil, err
	}
	s.last = s.cfg.Now()

	res := &rcsdirect.SendResult{Check: chk, TmpID: fmt.Sprintf("android-%d", s.last.UnixMilli())}
	seen := map[SendStatus]bool{}
	deadline := s.cfg.Now().Add(wait)
	var found bool
	var st SendStatus
	var raw string
	proto := KindUnknown // what the sent message itself says it went out as
	for {
		_, scr, err = s.read(ctx)
		if err == nil {
			sm := s.dev.m.ReadSend(scr, message)
			found, st, raw = sm.Found, sm.Status, sm.Raw
			if sm.Protocol != KindUnknown {
				proto = sm.Protocol
			}
			if found && st != StatusNone && !seen[st] {
				seen[st] = true
				res.StatusHistory = append(res.StatusHistory, string(st))
			}
			if st == StatusSent || st == StatusFailed {
				break
			}
		}
		if !s.cfg.Now().Before(deadline) {
			break
		}
		if err := s.cfg.Sleep(ctx, s.cfg.PollEvery); err != nil {
			return nil, err
		}
	}
	switch {
	case st == StatusFailed:
		res.FinalStatus = "failed: " + raw
	case !found:
		// The message never reached the thread: the send tap did not register.
		_ = s.dev.ClearCompose(ctx)
		res.FinalStatus = "send_not_registered"
	case st == StatusSent:
		res.Sent, res.FinalStatus = true, raw
	default:
		// In the thread but no final status yet. It is out of the box, so it
		// may well be on its way: report it sent-but-unconfirmed so it is not
		// retried into a duplicate.
		res.Sent, res.FinalStatus = true, "sent_unconfirmed"
		if raw != "" {
			res.FinalStatus = "sent_unconfirmed: " + raw
		}
	}

	// 4. Re-read what the chat became: this is the check on the pre-send read.
	after := s.afterRead(ctx)
	res.After = toCheck(to, after, true)
	res.After.Eligible, res.After.Reason = eligibility(after)
	// The message's own label beats the chat's: if it says SMS it went out as
	// SMS, whatever the chat reads afterwards.
	switch {
	case proto == KindSMS:
		res.MessageType = 1
		res.After.Transport, res.After.E2EE = "SMS", "off"
	case proto == KindRCS || proto == KindEncrypted:
		res.MessageType = 4
	case after.Kind == KindSMS:
		res.MessageType = 1
	case after.Kind == KindRCS || after.Kind == KindEncrypted:
		res.MessageType = 4
	}
	if res.Sent && after.Kind != KindEncrypted {
		logg.Error().Str("to", to).Str("after", string(after.Kind)).Str("evidence", after.Evidence).
			Msg("Android send went out but the chat does not read as encrypted afterwards")
	}
	return res, nil
}

// afterRead reads the chat after a send until it shows a known kind.
func (s *Sender) afterRead(ctx context.Context) ChatView {
	start := s.cfg.Now()
	var v ChatView
	for {
		v, _, _ = s.read(ctx)
		if v.Kind != KindUnknown {
			return v
		}
		if s.cfg.Now().Sub(start)+s.cfg.PollEvery > s.cfg.AfterBudget {
			return v
		}
		if s.cfg.Sleep(ctx, s.cfg.PollEvery) != nil {
			return v
		}
	}
}

func (s *Sender) Thread(ctx context.Context, to string, limit int) (*rcsdirect.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if err := s.dev.OpenChat(ctx, to); err != nil {
		return nil, err
	}
	v, scr, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	out := &rcsdirect.Thread{Check: toCheck(to, v, false)}
	width := 0
	for _, n := range scr.Nodes {
		width = max(width, n.Bounds.R)
	}
	bubbles := scr.FindAll(func(n Node) bool { return s.dev.m.inPackage(n) && n.ID() == s.dev.m.BubbleID && n.Text != "" })
	sort.SliceStable(bubbles, func(i, j int) bool { return bubbles[i].Bounds.T < bubbles[j].Bounds.T })
	if limit > 0 && len(bubbles) > limit {
		bubbles = bubbles[len(bubbles)-limit:]
	}
	for _, b := range bubbles {
		sm := s.dev.m.ReadSend(scr, b.Text)
		cx, _ := b.Bounds.Center()
		tr := out.Check.Transport
		switch sm.Protocol {
		case KindSMS:
			tr = "SMS"
		case KindRCS, KindEncrypted:
			tr = "RCS"
		}
		out.Messages = append(out.Messages, rcsdirect.ThreadMessage{
			Outgoing: cx > width/2, Status: firstNonEmpty(sm.Raw, string(sm.Status)), Text: b.Text,
			Transport: tr, E2EEAtSend: out.Check.E2EE,
		})
	}
	return out, nil
}

func (s *Sender) setHealth(h *rcsdirect.Health) *rcsdirect.Health {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.health = h
	return h
}

// Health reports whether the phone is ready. It never queues behind a send:
// while the phone is busy it returns the last reading.
func (s *Sender) Health(ctx context.Context) (*rcsdirect.Health, error) {
	if !s.mu.TryLock() {
		s.hmu.Lock()
		defer s.hmu.Unlock()
		if s.health != nil {
			h := *s.health
			return &h, nil
		}
		return &rcsdirect.Health{LoggedIn: true, Connected: true, PhoneResponding: true, SessionActive: true}, nil
	}
	defer s.mu.Unlock()

	h := &rcsdirect.Health{}
	if _, err := s.dev.State(ctx); err != nil {
		if rerr := s.dev.Reconnect(ctx, s.cfg.Addr); rerr != nil {
			h.LastError = err.Error()
			return s.setHealth(h), nil
		}
	}
	h.Connected = true
	info, err := s.dev.Info(ctx)
	if err != nil {
		h.LastError = err.Error()
		return s.setHealth(h), nil
	}
	h.PhoneResponding = true
	h.LoggedIn = info.MessagesFound
	h.SessionActive = info.DefaultSMS
	var problems []string
	if !info.MessagesFound {
		problems = append(problems, "Google Messages is not installed")
	} else if !info.DefaultSMS {
		problems = append(problems, "Google Messages is not the default messaging app")
	}
	if !info.SimReady {
		problems = append(problems, "no SIM is ready")
	}
	h.LastError = strings.Join(problems, "; ")
	// Whether RCS itself is on is only known per chat, from the screen; a
	// ready SIM with Messages as the default app is as far as the phone says.
	h.SIMs = []rcsdirect.SIM{{Carrier: info.Carrier, RCSEnabled: info.SimReady && info.DefaultSMS}}
	return s.setHealth(h), nil
}
