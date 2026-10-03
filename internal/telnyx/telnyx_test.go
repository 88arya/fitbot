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

func TestParseInbound(t *testing.T) {
	in, ok, err := ParseInbound([]byte(`{"data":{"event_type":"message.received","payload":{"id":"m1","text":"hi","from":{"phone_number":"+14165550100"}}}}`))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if in != (Inbound{ID: "m1", From: "+14165550100", Text: "hi"}) {
		t.Errorf("got %+v", in)
	}

	if _, ok, err := ParseInbound([]byte(`{"data":{"event_type":"message.finalized","payload":{}}}`)); ok || err != nil {
		t.Errorf("delivery report: ok=%v err=%v", ok, err)
	}
}
