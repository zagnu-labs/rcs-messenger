package rcsandroid

import (
	"regexp"
	"sort"
	"strings"
)

// Kind is what the open chat is, as the Messages app itself shows it.
type Kind string

const (
	KindUnknown   Kind = "unknown"   // the screen did not say (selectors off, or still loading)
	KindSMS       Kind = "sms"       // hint reads "Text message"
	KindRCS       Kind = "rcs"       // hint reads "RCS message", no encryption shown
	KindEncrypted Kind = "encrypted" // RCS with the padlock, or the "end-to-end encrypted" line
)

// ChatView is one reading of an open chat.
type ChatView struct {
	Kind         Kind
	Hint         string // the empty message box's hint text
	Evidence     string // what decided Kind, for the log and the send record
	ComposeFound bool
	ComposeText  string // what is typed in the box ("" when the box shows its hint)
}

var spaceRe = regexp.MustCompile(`\s+`)

func norm(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

func (m *matcher) inPackage(n Node) bool { return n.Package == "" || n.Package == m.Package }

func (m *matcher) compose(s *Screen) (Node, bool) {
	if n, ok := s.Find(func(n Node) bool { return m.inPackage(n) && n.ID() == m.ComposeID }); ok {
		return n, true
	}
	return s.Find(func(n Node) bool { return m.inPackage(n) && n.Class == "android.widget.EditText" })
}

func (m *matcher) sendButton(s *Screen) (Node, bool) {
	return s.Find(func(n Node) bool {
		return m.inPackage(n) && n.Enabled && (n.ID() == m.SendID || (m.SendDesc != "" && m.sendDesc.MatchString(norm(n.Desc))))
	})
}

// ReadChat reads what the open chat is. The message box hint is the app's
// own live answer, so it wins over the thread line: an old "end-to-end
// encrypted" line can sit further up a chat that has since dropped to SMS.
func (m *matcher) ReadChat(s *Screen) ChatView {
	v := ChatView{Kind: KindUnknown}
	c, ok := m.compose(s)
	if !ok {
		v.Evidence = "no message box on screen"
		return v
	}
	v.ComposeFound = true
	if hint := norm(c.Text); m.hintSMS.MatchString(hint) || m.hintRCS.MatchString(hint) {
		v.Hint = hint
	} else if hint2 := norm(c.Desc); m.hintSMS.MatchString(hint2) || m.hintRCS.MatchString(hint2) {
		v.Hint = hint2
	} else {
		v.ComposeText = c.Text
	}

	banner, hasBanner := s.Find(func(n Node) bool {
		return m.inPackage(n) && (m.banner.MatchString(norm(n.Text)) || m.banner.MatchString(norm(n.Desc)))
	})
	lock, hasLock := s.Find(func(n Node) bool {
		return m.inPackage(n) && n.Class != "android.widget.EditText" &&
			(m.lock.MatchString(n.ID()) || m.lock.MatchString(norm(n.Desc)))
	})

	switch {
	case m.hintSMS.MatchString(v.Hint):
		v.Kind, v.Evidence = KindSMS, `message box hint "`+v.Hint+`"`
	case m.hintRCS.MatchString(v.Hint) && hasLock:
		v.Kind, v.Evidence = KindEncrypted, `message box hint "`+v.Hint+`" with padlock ("`+firstNonEmpty(lock.Desc, lock.ID())+`")`
	case m.hintRCS.MatchString(v.Hint) && hasBanner:
		v.Kind, v.Evidence = KindEncrypted, `message box hint "`+v.Hint+`" and thread line "`+norm(firstNonEmpty(banner.Text, banner.Desc))+`"`
	case m.hintRCS.MatchString(v.Hint):
		v.Kind, v.Evidence = KindRCS, `message box hint "`+v.Hint+`", no padlock`
	case hasBanner:
		// Text is typed in the box, so the hint is not readable; the thread
		// line alone is weaker evidence than the hint, so say so.
		v.Kind, v.Evidence = KindEncrypted, `thread line "`+norm(firstNonEmpty(banner.Text, banner.Desc))+`" (hint hidden by typed text)`
	default:
		v.Evidence = "hint not readable"
	}
	return v
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// Refusal reasons. A refusal always means nothing was sent.
const (
	ReasonE2EEPending    = "e2ee_pending" // RCS but no padlock yet; retryable (an iPhone on plain RCS stays here)
	ReasonNotE2EE        = "not_e2ee"     // the chat is SMS
	ReasonUnreadable     = "screen_unreadable"
	ReasonChatNotOpen    = "chat_not_open" // the app did not show a chat
	ReasonPhoneLocked    = "phone_locked"
	ReasonUnsupported    = "unsupported_text"
	ReasonTypingMismatch = "typing_mismatch"
)

// eligibility is the one send rule: send only when the app shows this chat
// as end-to-end encrypted. Everything else is refused.
func eligibility(v ChatView) (bool, string) {
	switch {
	case !v.ComposeFound:
		return false, ReasonChatNotOpen
	case v.Kind == KindEncrypted:
		return true, ""
	case v.Kind == KindRCS:
		return false, ReasonE2EEPending
	case v.Kind == KindSMS:
		return false, ReasonNotE2EE
	}
	return false, ReasonUnreadable
}

// SendStatus is what the thread shows for our message.
type SendStatus string

const (
	StatusNone    SendStatus = ""
	StatusSending SendStatus = "sending"
	StatusSent    SendStatus = "sent"
	StatusFailed  SendStatus = "failed"
)

// SentMessage is our just-sent message as the thread shows it.
type SentMessage struct {
	Found    bool
	Status   SendStatus
	Raw      string // the status text or description tail the status came from
	Protocol Kind   // what the message itself says it went out as; KindUnknown if it says nothing
	Desc     string
}

func contains(outer, inner Rect) bool {
	return outer.L <= inner.L && outer.T <= inner.T && outer.R >= inner.R && outer.B >= inner.B
}

// ReadSend finds our just-sent message in the thread, its status, and the
// protocol the message itself carries. The newest outgoing copy wins: a
// thread can hold the same text from the other side too (the emulator echoes
// sends back), and outgoing messages sit on the right of the screen.
func (m *matcher) ReadSend(s *Screen, text string) SentMessage {
	want := norm(text)
	width := 0
	for _, n := range s.Nodes {
		width = max(width, n.Bounds.R)
	}
	var bubble Node
	var found, outgoing bool
	for _, n := range s.Nodes {
		if !m.inPackage(n) || n.ID() != m.BubbleID || norm(n.Text) != want {
			continue
		}
		cx, _ := n.Bounds.Center()
		out := width == 0 || cx > width/2
		if out || !outgoing { // an outgoing copy beats any incoming one; later beats earlier
			if out || !found {
				bubble, found, outgoing = n, true, out
			}
		}
	}
	if !found {
		return SentMessage{Protocol: KindUnknown}
	}
	res := SentMessage{Found: true, Protocol: KindUnknown}

	// The message's own description: "You said <text> ., Now, SMS".
	if wrap, ok := s.Find(func(n Node) bool {
		return m.inPackage(n) && n.ID() == m.MessageContentID && contains(n.Bounds, bubble.Bounds)
	}); ok {
		res.Desc = norm(wrap.Desc)
		tail := res.Desc
		if i := strings.LastIndex(tail, " ., "); i >= 0 {
			tail = tail[i+4:]
		}
		switch {
		case m.msgSMS.MatchString(tail):
			res.Protocol = KindSMS
		case m.msgEnc.MatchString(tail):
			res.Protocol = KindEncrypted
		case m.msgRCS.MatchString(tail):
			res.Protocol = KindRCS
		}
		switch {
		case m.failed.MatchString(tail):
			res.Status, res.Raw = StatusFailed, tail
		case m.sent.MatchString(tail):
			res.Status, res.Raw = StatusSent, tail
		case m.sending.MatchString(tail):
			res.Status, res.Raw = StatusSending, tail
		}
	}

	// The small status line under the message, when the app shows one.
	below := s.FindAll(func(n Node) bool {
		return m.inPackage(n) && n.Bounds.T >= bubble.Bounds.T && n.Text != "" &&
			(m.sent.MatchString(norm(n.Text)) || m.failed.MatchString(norm(n.Text)) || m.sending.MatchString(norm(n.Text)))
	})
	sort.SliceStable(below, func(i, j int) bool { return below[i].Bounds.T < below[j].Bounds.T })
	if len(below) > 0 {
		t := norm(below[0].Text)
		switch {
		case m.failed.MatchString(t):
			res.Status, res.Raw = StatusFailed, t
		case m.sent.MatchString(t) && res.Status != StatusFailed:
			res.Status, res.Raw = StatusSent, t
		case m.sending.MatchString(t) && res.Status == StatusNone:
			res.Status, res.Raw = StatusSending, t
		}
	}
	return res
}
