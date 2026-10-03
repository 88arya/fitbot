package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/88arya/spotter/internal/llm"
	"github.com/88arya/spotter/internal/telnyx"
)

const systemPrompt = `You are Spotter, an accountability assistant that talks to people over SMS.
Keep replies short and plain: one or two sentences, no markdown, no emoji.`

const fallbackReply = "Sorry, I can't think right now. Try again in a few minutes."

// SMS holds what the Telnyx webhook needs. A nil LLM sends the fallback reply.
type SMS struct {
	Telnyx         *telnyx.Client
	LLM            *llm.Client
	AllowedSenders []string
}

func (s *SMS) handleTelnyx(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Telnyx.Verify(r.Header, body, time.Now()); err != nil {
		slog.Warn("rejected telnyx webhook", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	in, ok, err := telnyx.ParseInbound(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Acknowledge every verified event so Telnyx does not retry it.
	w.WriteHeader(http.StatusOK)
	if !ok {
		return
	}
	if !slices.Contains(s.AllowedSenders, in.From) {
		slog.Info("ignored text from unlisted sender", "from", in.From)
		return
	}

	slog.Info("text received", "id", in.ID, "from", in.From)
	go s.reply(context.WithoutCancel(r.Context()), in)
}

func (s *SMS) reply(ctx context.Context, in telnyx.Inbound) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	text := fallbackReply
	if s.LLM != nil {
		start := time.Now()
		out, usage, err := s.LLM.Complete(ctx, []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: in.Text},
		})
		if err != nil {
			slog.Error("llm call failed", "id", in.ID, "err", err)
		} else {
			text = out
			slog.Info("llm call", "id", in.ID, "model", s.LLM.Model(), "latency_ms", time.Since(start).Milliseconds(),
				"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
		}
	}

	if err := s.Telnyx.Send(ctx, in.From, text); err != nil {
		slog.Error("reply failed", "id", in.ID, "to", in.From, "err", err)
		return
	}
	slog.Info("reply sent", "id", in.ID, "to", in.From)
}
