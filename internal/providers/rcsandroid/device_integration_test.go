package rcsandroid

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsdirect"
)

// Runs against a real phone or emulator, only when asked:
//
//	RCS_ANDROID_E2E=1 RCS_ANDROID_SERIAL=emulator-5570 RCS_ANDROID_ADB=$(which adb) go test ./internal/providers/rcsandroid -run Device -v
//
// It types into the message box and clears it again but never sends anything.
func realDevice(t *testing.T) (*Device, *Sender) {
	t.Helper()
	if os.Getenv("RCS_ANDROID_E2E") == "" {
		t.Skip("set RCS_ANDROID_E2E=1 (and RCS_ANDROID_SERIAL) to run against a device")
	}
	r := ExecRunner{Bin: os.Getenv("RCS_ANDROID_ADB"), Serial: os.Getenv("RCS_ANDROID_SERIAL")}
	dev, err := NewDevice(r, DefaultSelectors(), "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Runner: r, Selectors: DefaultSelectors(), SettleBudget: 6 * time.Second, CheckBudget: 4 * time.Second, PollEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return dev, s
}

func TestDeviceTypingRoundTrips(t *testing.T) {
	dev, _ := realDevice(t)
	ctx := context.Background()
	if err := dev.Awake(ctx); err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"Hey Ann - I'm the founder of Zagnu.com, a side hustle app that lets people earn ~$120/month passively from their phone.",
		"line one\n\nline three, with \"quotes\" and a $dollar & ampersand; (parens) #hash 100% sure",
		strings.Repeat("long text goes here ", 14),
	}
	for _, text := range texts {
		if err := dev.OpenChat(ctx, "+15551234567"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		scr, err := dev.Dump(ctx)
		if err != nil {
			t.Fatal(err)
		}
		box, ok := dev.m.compose(scr)
		if !ok {
			t.Fatal("no message box on a real screen")
		}
		x, y := box.Bounds.Center()
		if err := dev.Tap(ctx, x, y); err != nil {
			t.Fatal(err)
		}
		if err := dev.ClearCompose(ctx); err != nil {
			t.Fatal(err)
		}
		if err := dev.TypeText(ctx, text); err != nil {
			t.Fatal(err)
		}
		scr, err = dev.Dump(ctx)
		if err != nil {
			t.Fatal(err)
		}
		typed, _ := dev.m.compose(scr)
		if norm(typed.Text) != norm(text) {
			t.Errorf("typed text differs on a real screen:\n got %q\nwant %q", norm(typed.Text), norm(text))
		}
		if err := dev.ClearCompose(ctx); err != nil {
			t.Fatal(err)
		}
		scr, _ = dev.Dump(ctx)
		if after, _ := dev.m.compose(scr); norm(after.Text) != "Text message" && norm(after.Text) != "RCS message" {
			t.Errorf("box was not cleared: %q", after.Text)
		}
	}
}

func TestDeviceRefusesAndSendsNothing(t *testing.T) {
	_, s := realDevice(t)
	ctx := context.Background()
	h, err := s.Health(ctx)
	if err != nil || !h.Connected || !h.PhoneResponding {
		t.Fatalf("health: %+v %v", h, err)
	}
	c, err := s.Check(ctx, "+15551234567")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("check: transport=%s e2ee=%s evidence=%s eligible=%v reason=%s", c.Transport, c.E2EE, c.E2EEEvidence, c.Eligible, c.Reason)
	if c.Eligible {
		t.Skip("this device reads the chat as encrypted; the refusal test needs a non-encrypted chat")
	}
	_, err = s.Send(ctx, "+15551234567", "must never be sent", rcsdirect.SendOptions{})
	var ref *rcsdirect.RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("a non-encrypted chat must be refused, got %v", err)
	}
	th, err := s.Thread(ctx, "+15551234567", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range th.Messages {
		if m.Text == "must never be sent" {
			t.Fatal("the refused message reached the thread")
		}
	}
}
