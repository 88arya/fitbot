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

const defaultAPIURL = "https://api.telnyx.com/v2"

// maxSkew is how old a webhook timestamp may be before it is rejected as a replay.
const maxSkew = 5 * time.Minute

type Client struct {
	// APIURL is the Telnyx API base URL. Tests point it at a fake server.
	APIURL string
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
		APIURL: defaultAPIURL,
		apiKey: apiKey,
		from:   from,
		pubKey: key,
		http:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Send texts body to the E.164 number to and returns Telnyx's message ID.
func (c *Client) Send(ctx context.Context, to, body string) (string, error) {
	payload, err := json.Marshal(map[string]string{"from": c.from, "to": to, "text": body})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(msg)}
	}

	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("telnyx send: decoding response: %w", err)
	}
	return out.Data.ID, nil
}

// APIError is a send Telnyx answered with an error status. Most mean the
// text was not sent; a gateway error may come after it was.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telnyx send: %s: %s", e.Status, e.Body)
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

// Event types Fitbot handles. Telnyx sends message.sent and
// message.finalized as delivery reports for outbound texts.
const (
	EventReceived  = "message.received"
	EventSent      = "message.sent"
	EventFinalized = "message.finalized"
)

// Event is the part of a messaging webhook Fitbot uses.
type Event struct {
	Type string
	// ID is Telnyx's message ID.
	ID   string
	From string
	Text string
	// Status is the recipient's delivery status, like "delivered".
	Status string
	Parts  int
	// Cost is what Telnyx charged in USD, or "" when not reported yet.
	Cost string
}

func ParseEvent(body []byte) (Event, error) {
	var ev struct {
		Data struct {
			EventType string `json:"event_type"`
			Payload   struct {
				ID    string `json:"id"`
				Text  string `json:"text"`
				Parts int    `json:"parts"`
				From  struct {
					PhoneNumber string `json:"phone_number"`
				} `json:"from"`
				To []struct {
					Status string `json:"status"`
				} `json:"to"`
				Cost *struct {
					Amount   string `json:"amount"`
					Currency string `json:"currency"`
				} `json:"cost"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, err
	}
	p := ev.Data.Payload
	e := Event{
		Type:  ev.Data.EventType,
		ID:    p.ID,
		From:  p.From.PhoneNumber,
		Text:  p.Text,
		Parts: p.Parts,
	}
	if len(p.To) > 0 {
		e.Status = p.To[0].Status
	}
	if p.Cost != nil && p.Cost.Currency == "USD" {
		e.Cost = p.Cost.Amount
	}
	return e, nil
}
