package wire

import (
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	secret := "whsec-" + now.Format("150405")
	body := []byte(`{"id":"evt_1","action":"invoice.sent"}`)
	h := SignWebhook(secret, now, body)
	if err := VerifyWebhook(secret, h, body, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if VerifyWebhook(secret, h, []byte(`{"id":"evt_2"}`), now, 5*time.Minute) == nil {
		t.Fatal("altered body accepted")
	}
	if VerifyWebhook(secret, h, body, now.Add(6*time.Minute), 5*time.Minute) == nil {
		t.Fatal("old signature accepted")
	}
	if VerifyWebhook("other", h, body, now, 5*time.Minute) == nil || VerifyWebhook(secret, "garbage", body, now, 5*time.Minute) == nil {
		t.Fatal("bad secret or header accepted")
	}
}
