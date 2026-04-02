package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// WSEvent is a WebSocket push event.
type WSEvent struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Hub manages WebSocket connections and broadcasts.
type Hub struct {
	mu      sync.Mutex
	clients map[*wsClient]bool
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// NewHub creates a WebSocket hub.
func NewHub() *Hub {
	return &Hub{clients: make(map[*wsClient]bool)}
}

// Broadcast sends an event to all connected clients.
func (h *Hub) Broadcast(event WSEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// Client too slow, drop
			close(c.send)
			delete(h.clients, c)
		}
	}
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
	slog.Info("websocket client connected", "total", len(h.clients))
}

func (h *Hub) unregister(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		close(c.send)
		delete(h.clients, c)
	}
	slog.Info("websocket client disconnected", "total", len(h.clients))
}

// HandleWS handles WebSocket upgrade and message pumping.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // allow any origin for dashboard
	})
	if err != nil {
		slog.Error("websocket accept failed", "error", err)
		return
	}

	c := &wsClient{
		conn: conn,
		send: make(chan []byte, 64),
	}
	h.register(c)
	defer h.unregister(c)

	ctx := conn.CloseRead(context.Background())

	// Write pump
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
