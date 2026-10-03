package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("key", base64.StdEncoding.EncodeToString(pub), "+12505550100")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"data":{}}`)
	signed := func(ts time.Time, body []byte) http.Header {
		s := strconv.FormatInt(ts.Unix(), 10)
		h := http.Header{}
		h.Set("Telnyx-Timestamp", s)
		h.Set("Telnyx-Signature-Ed25519", base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(s+"|"+string(body)))))
		return h
	}

	if err := c.Verify(signed(now, body), body, now); err != nil {
		t.Errorf("valid webhook rejected: %v", err)
	}
	if err := c.Verify(signed(now, body), []byte(`{"data":{"x":1}}`), now); err == nil {
		t.Error("tampered body accepted")
	}
	if err := c.Verify(signed(now.Add(-10*time.Minute), body), body, now); err == nil {
		t.Error("stale timestamp accepted")
	}
	if err := c.Verify(http.Header{}, body, now); err == nil {
		t.Error("unsigned webhook accepted")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New("key", "not-a-key", "+12505550100"); err == nil {
		t.Error("expected error for malformed public key")
	}
}

func TestParseEvent(t *testing.T) {
	e, err := ParseEvent([]byte(`{"data":{"event_type":"message.received","payload":{"id":"m1","text":"hi","parts":1,
		"from":{"phone_number":"+14165550100"},"to":[{"status":"webhook_delivered"}],"cost":{"amount":"0.0025","currency":"USD"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Event{Type: EventReceived, ID: "m1", From: "+14165550100", Text: "hi", Status: "webhook_delivered", Parts: 1, Cost: "0.0025"}
	if e != want {
		t.Errorf("got %+v, want %+v", e, want)
	}

	e, err = ParseEvent([]byte(`{"data":{"event_type":"message.sent","payload":{"id":"m2","to":[{"status":"sent"}],"cost":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != EventSent || e.Status != "sent" || e.Cost != "" {
		t.Errorf("delivery report without cost: %+v", e)
	}
}
