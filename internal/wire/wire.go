// Package wire holds the event webhook contract every module implements: the signature
// header the core sends with each event (t=<unix seconds>,v1=<hex HMAC-SHA256 of
// "<t>.<body>">), the context token header and the event payload. Package signing,
// archiving and manifest reading are not here: they wait on a public package or CLI
// from the core (see package.go).
package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header names the core uses.
const (
	SignatureHeader = "X-Togavsu-Signature"
	ContextHeader   = "X-Togavsu-Context-Token"
)

// SignWebhook signs an event body. The module only verifies; the fake core in tests signs.
func SignWebhook(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), decimal)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook checks a signature header within the allowed clock skew.
func VerifyWebhook(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" {
		return errors.New("wire: malformed webhook signature")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("wire: webhook signature is outside the allowed clock skew")
	}
	want := SignWebhook(secret, time.Unix(sec, 0), body)
	if !hmac.Equal([]byte(want), []byte("t="+t+",v1="+v1)) {
		return errors.New("wire: webhook signature doesn't match")
	}
	return nil
}

// EventPayload is what the core delivers for one event.
type EventPayload struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	MatterID   string `json:"matter_id,omitempty"`
	OccurredAt string `json:"occurred_at"`
	Module     string `json:"module"`
}
