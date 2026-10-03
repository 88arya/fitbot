// Package telnyx sends texts through the Telnyx API and verifies its webhooks.
package telnyx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const apiURL = "https://api.telnyx.com/v2"

// maxSkew is how old a webhook timestamp may be before it is rejected as a replay.
const maxSkew = 5 * time.Minute

type Client struct {
	apiKey string
	from   string
	pubKey ed25519.PublicKey
	http   *http.Client
}

func New(apiKey, publicKey, from string) (*Client, error) {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("TELNYX_PUBLIC_KEY must be a base64 Ed25519 public key")
	}
	return &Client{
		apiKey: apiKey,
		from:   from,
		pubKey: key,
		http:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Send texts body to the E.164 number to.
func (c *Client) Send(ctx context.Context, to, body string) error {
	payload, err := json.Marshal(map[string]string{"from": c.from, "to": to, "text": body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telnyx send: %s: %s", resp.Status, msg)
	}
	return nil
}

// Verify checks a webhook's Ed25519 signature over "timestamp|body".
func (c *Client) Verify(h http.Header, body []byte, now time.Time) error {
	ts := h.Get("Telnyx-Timestamp")
	sig, err := base64.StdEncoding.DecodeString(h.Get("Telnyx-Signature-Ed25519"))
	if ts == "" || err != nil {
		return errors.New("missing or malformed signature headers")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp")
	}
	if d := now.Sub(time.Unix(secs, 0)); d > maxSkew || d < -maxSkew {
		return errors.New("timestamp outside allowed window")
	}
	if !ed25519.Verify(c.pubKey, []byte(ts+"|"+string(body)), sig) {
		return errors.New("bad signature")
	}
	return nil
}

// Inbound is an inbound text parsed from a message.received webhook.
type Inbound struct {
	ID   string
	From string
	Text string
}

// ParseInbound returns the inbound text in a webhook body, or ok=false for
// any other event type.
func ParseInbound(body []byte) (in Inbound, ok bool, err error) {
	var ev struct {
		Data struct {
			EventType string `json:"event_type"`
			Payload   struct {
				ID   string `json:"id"`
				Text string `json:"text"`
				From struct {
					PhoneNumber string `json:"phone_number"`
				} `json:"from"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return Inbound{}, false, err
	}
	if ev.Data.EventType != "message.received" {
		return Inbound{}, false, nil
	}
	p := ev.Data.Payload
	return Inbound{ID: p.ID, From: p.From.PhoneNumber, Text: p.Text}, true, nil
}
