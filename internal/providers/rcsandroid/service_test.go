package rcsandroid

import (
	"context"
	"sync"
	"testing"

	"github.com/zagnu-labs/rcs-messenger/internal/rcs"
)

// memResults is an in-memory send-results store, so the whole path from the
// service down to the screen can be run without a database.
type memResults struct {
	mu   sync.Mutex
	recs []rcs.SendRecord
}

func (m *memResults) Record(_ context.Context, r rcs.SendRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}

func (m *memResults) Blocked(_ context.Context, phone string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.Phone == phone && r.Outcome.Blocks() {
			return true, nil
		}
	}
	return false, nil
}

func (m *memResults) Recent(_ context.Context, _ string, _ int) ([]rcs.SendRecord, error) {
	return m.recs, nil
}

func service(p *fakePhone) (*rcs.Service, *memResults) {
	res := &memResults{}
	svc := rcs.NewService(newSender(p, newClock(), nil), rcs.Config{Enabled: true}).SetResults(res)
	return svc, res
}

func TestServiceRecordsEncryptedSuccess(t *testing.T) {
	p := newPhone(KindSMS, KindEncrypted)
	svc, res := service(p)
	r, err := svc.Send(context.Background(), "+1 (555) 123-4567", "Hey Ann")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Sent || r.Outcome != rcs.OutcomeSuccessE2EE || r.Transport != "RCS" {
		t.Fatalf("want SUCCESS_E2EE over RCS, got %+v", r)
	}
	if got := res.recs[0]; got.E2EEBefore != "on" || got.E2EEAfter != "on" || got.Capability != "yes" || got.E2EEEvidence == "" {
		t.Fatalf("record: %+v", got)
	}
}

func TestServiceRefusesPlainRCSAndRecordsWhy(t *testing.T) {
	p := newPhone(KindRCS) // an iPhone on plain RCS
	svc, res := service(p)
	_, err := svc.Send(context.Background(), "+15551234567", "Hey Ann")
	if err == nil {
		t.Fatal("plain RCS must be refused")
	}
	if len(res.recs) != 1 || res.recs[0].Outcome != rcs.OutcomeFailed || res.recs[0].Status != ReasonE2EEPending {
		t.Fatalf("record: %+v", res.recs)
	}
	if p.count("shell input text") != 0 {
		t.Fatal("typed into a plain RCS chat")
	}
	// refused, not blocked: a later retry is allowed (the phone may finish its lookup)
	if blocked, _ := res.Blocked(context.Background(), "+15551234567"); blocked {
		t.Fatal("a refusal must not block the number")
	}
}

func TestServiceBlocksANumberWhoseMessageWentOutAsSMS(t *testing.T) {
	p := newPhone(KindEncrypted)
	p.tailOverride = "Now, SMS"
	svc, res := service(p)
	r, err := svc.Send(context.Background(), "+15551234567", "Hey Ann")
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != rcs.OutcomeSuccessSMS || !r.Outcome.Blocks() {
		t.Fatalf("want SUCCESS_SMS, got %s", r.Outcome)
	}
	if _, err := svc.Send(context.Background(), "+15551234567", "again"); err == nil {
		t.Fatal("a number that got SMS must never be sent to again")
	}
	if blocked, _ := res.Blocked(context.Background(), "+15551234567"); !blocked {
		t.Fatal("the number should be blocked")
	}
}

func TestServiceReportsAFailedSend(t *testing.T) {
	p := newPhone(KindEncrypted)
	p.statuses = []string{"Not sent. Tap to try again"}
	svc, _ := service(p)
	r, err := svc.Send(context.Background(), "+15551234567", "Hey Ann")
	if err != nil {
		t.Fatal(err)
	}
	if r.Sent || r.Outcome != rcs.OutcomeFailed {
		t.Fatalf("want FAILED, got %+v", r)
	}
}
