// Package nodestream defines the authenticated node WebSocket control frames.
package nodestream

type Message struct {
	Type string  `json:"type"`
	Data Payload `json:"data"`
}

type Payload struct {
	SessionID      string `json:"session_id,omitempty"`
	Data           string `json:"data,omitempty"`
	Message        string `json:"message,omitempty"`
	Cols           int    `json:"cols,omitempty"`
	Rows           int    `json:"rows,omitempty"`
	DisableExecute bool   `json:"disable_execute,omitempty"`
}
