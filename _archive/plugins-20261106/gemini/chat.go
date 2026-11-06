package main

// Project resolution and the streaming response writer for the Gemini channel.
//
// resolveProject wraps loadCodeAssist (the cloudaicompanionProject probe), and
// streamOpenAI reshapes the Cloud Code SSE stream into OpenAI chunks.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// the credential does not already carry one. The probe order is sandbox then
// daily, as resolveGeminiProject does.
func (s *server) resolveProject(ctx context.Context, creds credentials) (project string, probed bool, err error) {
	identity := firstNonEmpty(creds.Email, creds.Sub, creds.AccessToken)
	s.projectMu.Lock()
	if cached := s.projectCache[identity]; cached != "" {
		s.projectMu.Unlock()
		return cached, false, nil
	}
	s.projectMu.Unlock()

	if p := strings.TrimSpace(creds.CloudaicompanionProject); p != "" {
		s.projectMu.Lock()
		s.projectCache[identity] = p
		s.projectMu.Unlock()
		return p, false, nil
	}
	endpoints := []string{endpointSandbox, endpointDaily}
	project, probed, err = s.client.loadCodeAssist(ctx, creds.AccessToken, defaultAccountID, endpoints)
	if err != nil {
		return "", probed, err
	}
	s.projectMu.Lock()
	s.projectCache[identity] = project
	s.projectMu.Unlock()
	return project, probed, nil
}

// streamOpenAI translates the Cloud Code SSE stream into OpenAI chunks.
// onSignature, when set, captures thoughtSignature values for replay.
func (s *server) streamOpenAI(w http.ResponseWriter, up io.Reader, model string, onSignature func(name, argsJSON, sig string)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "writer cannot flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id":     "chatcmpl-gemini",
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	emit(map[string]any{"role": "assistant", "content": ""}, nil)
	t := newGeminiTranslator(emit)
	t.onSignature = onSignature
	if err := pumpGemini(up, t); err != nil {
		emit(map[string]any{"content": "\n[stream error: " + err.Error() + "]"}, "stop")
		return
	}
	if t.textBuf.Len() == 0 && len(t.toolCalls) == 0 {
		// A 200 with no content blocks is a broken upstream, not an answer.
		emit(map[string]any{"content": "\n[stream error: upstream returned no content]"}, "stop")
		return
	}
	emit(map[string]any{}, mapFinish(t.finish, len(t.toolCalls)))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
