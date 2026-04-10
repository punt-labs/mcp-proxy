package reconnect

import "encoding/json"

// handshake caches the MCP lifecycle frames so they can be replayed on reconnect.
// Daemons that open a fresh ServerSession per WebSocket connection require an
// initialize/notifications/initialized exchange before accepting tool calls.
type handshake struct {
	initRequest []byte          // verbatim "initialize" request from client
	initID      json.RawMessage // the "id" field from the initialize request
	initialized []byte          // verbatim "notifications/initialized" notification
}

// sniff inspects a JSON-RPC line and caches it if it is part of the MCP handshake.
// Only the top-level "method" and "id" fields are parsed.
func (h *handshake) sniff(line []byte) {
	var env struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &env) != nil {
		return
	}

	switch env.Method {
	case "initialize":
		if len(env.ID) == 0 || string(env.ID) == "null" {
			return // malformed — don't cache
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		h.initRequest = cp
		h.initID = env.ID
	case "notifications/initialized":
		cp := make([]byte, len(line))
		copy(cp, line)
		h.initialized = cp
	}
}

// ready reports whether both handshake frames have been cached.
func (h *handshake) ready() bool {
	return h.initRequest != nil && h.initialized != nil
}

// cached reports whether at least the initialize request has been cached.
func (h *handshake) cached() bool {
	return h.initRequest != nil
}
