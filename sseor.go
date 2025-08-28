// Package sseor provides a simple Server-Sent Events (SSE) manager with Redis backend
package sseor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// Event represents an SSE event that can be sent to clients
type Event struct {
	ID    string                 `json:"id,omitempty"`
	Type  string                 `json:"type"`
	Data  map[string]interface{} `json:"data"`
	Retry int                    `json:"retry,omitempty"` // Retry time in milliseconds
}

// Connection represents an active SSE connection
type Connection struct {
	ID       string
	UserID   string
	Writer   http.ResponseWriter
	Request  *http.Request
	Done     chan bool
	LastSeen time.Time
}

// HandlerFunc defines the signature for SSE route handlers
type HandlerFunc func(w http.ResponseWriter, r *http.Request, manager *Manager)

// Manager manages SSE connections with Redis backend
type Manager struct {
	redisClient   *redis.Client
	connections   map[string]*Connection // connectionID -> Connection
	userConns     map[string][]string    // userID -> []connectionID
	mutex         sync.RWMutex
	router        *mux.Router
	server        *http.Server
	cleanupTicker *time.Ticker
	ctx           context.Context
	cancel        context.CancelFunc
}

// NewManager creates a new SSE Manager with Redis backend
func NewManager(redisURL string) (*Manager, error) {
	// Parse Redis URL
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redis URL: %w", err)
	}

	// Create Redis client
	client := redis.NewClient(opt)

	// Test Redis connection
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	manager := &Manager{
		redisClient: client,
		connections: make(map[string]*Connection),
		userConns:   make(map[string][]string),
		router:      mux.NewRouter(),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Start cleanup routine
	manager.startCleanup()

	// Subscribe to Redis for cross-service messaging
	go manager.subscribeToRedis()

	return manager, nil
}

// Route registers an SSE route with a handler
func (m *Manager) Route(path string, handler HandlerFunc) {
	m.router.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, m)
	}).Methods("GET")
}

// ListenAndServe starts the HTTP server
func (m *Manager) ListenAndServe(addr string) error {
	m.server = &http.Server{
		Addr:    addr,
		Handler: m.router,
		// Remove timeouts for SSE connections - they need to be long-lived
		ReadTimeout:       0,                // No read timeout
		WriteTimeout:      0,                // No write timeout
		IdleTimeout:       0,                // No idle timeout
		ReadHeaderTimeout: 10 * time.Second, // Only timeout for reading headers
	}

	return m.server.ListenAndServe()
}

// HandleSSE handles incoming SSE connections
func (m *Manager) HandleSSE(w http.ResponseWriter, r *http.Request, userID string) error {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Disable any potential reverse proxy buffering
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Generate unique connection ID
	connID := fmt.Sprintf("%s_%d", userID, time.Now().UnixNano())

	// Create connection
	conn := &Connection{
		ID:       connID,
		UserID:   userID,
		Writer:   w,
		Request:  r,
		Done:     make(chan bool),
		LastSeen: time.Now(),
	}

	// Add connection to manager
	m.addConnection(conn)
	defer m.removeConnection(connID)

	// Send initial connection event
	initialEvent := Event{
		Type: "connected",
		Data: map[string]interface{}{
			"connection_id": connID,
			"user_id":       userID,
			"timestamp":     time.Now().Unix(),
		},
	}

	m.sendEventToConnection(conn, initialEvent)

	// Store connection in Redis for cross-service awareness
	m.storeConnectionInRedis(conn)
	defer m.removeConnectionFromRedis(connID)

	// Keep connection alive
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}

	// Flush initial response
	flusher.Flush()

	// Send periodic heartbeat every 30 seconds instead of keeping connection busy
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	// Update last seen every 5 minutes in Redis
	redisTicker := time.NewTicker(5 * time.Minute)
	defer redisTicker.Stop()

	for {
		select {
		case <-conn.Done:
			return nil
		case <-r.Context().Done():
			return nil
		case <-m.ctx.Done():
			return nil
		case <-heartbeatTicker.C:
			// Update last seen time
			conn.LastSeen = time.Now()

			// Send heartbeat
			heartbeat := Event{
				Type: "heartbeat",
				Data: map[string]interface{}{
					"timestamp": time.Now().Unix(),
				},
			}
			if err := m.sendEventToConnection(conn, heartbeat); err != nil {
				log.Printf("Failed to send heartbeat to %s: %v", connID, err)
				return err
			}
			flusher.Flush()
		case <-redisTicker.C:
			// Refresh connection info in Redis
			m.storeConnectionInRedis(conn)
		}
	}
}

// Publish sends an event to all connections for a specific user
func (m *Manager) Publish(ctx context.Context, userID string, event Event) error {
	// Add timestamp if not provided
	if event.Data == nil {
		event.Data = make(map[string]interface{})
	}
	if _, exists := event.Data["timestamp"]; !exists {
		event.Data["timestamp"] = time.Now().Unix()
	}

	// Send to local connections
	m.sendToLocalConnections(userID, event)

	// Publish to Redis for other service instances
	return m.publishToRedis(userID, event)
}

// PublishToAll sends an event to all connected users
func (m *Manager) PublishToAll(ctx context.Context, event Event) error {
	m.mutex.RLock()
	userIDs := make([]string, 0, len(m.userConns))
	for userID := range m.userConns {
		userIDs = append(userIDs, userID)
	}
	m.mutex.RUnlock()

	// Send to all local users
	for _, userID := range userIDs {
		m.sendToLocalConnections(userID, event)
	}

	// Publish to Redis for other service instances
	return m.publishToRedis("*", event)
}

// GetConnectionCount returns the number of active connections for a user
func (m *Manager) GetConnectionCount(userID string) int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.userConns[userID])
}

// GetTotalConnections returns total number of active connections
func (m *Manager) GetTotalConnections() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.connections)
}

// Close gracefully shuts down the manager
func (m *Manager) Close() error {
	m.cancel()

	if m.cleanupTicker != nil {
		m.cleanupTicker.Stop()
	}

	// Close all connections
	m.mutex.Lock()
	for _, conn := range m.connections {
		close(conn.Done)
	}
	m.mutex.Unlock()

	// Close Redis client
	if err := m.redisClient.Close(); err != nil {
		return fmt.Errorf("failed to close redis client: %w", err)
	}

	// Shutdown HTTP server
	if m.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.server.Shutdown(ctx)
	}

	return nil
}

// Private methods

func (m *Manager) addConnection(conn *Connection) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.connections[conn.ID] = conn
	m.userConns[conn.UserID] = append(m.userConns[conn.UserID], conn.ID)
}

func (m *Manager) removeConnection(connID string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	conn, exists := m.connections[connID]
	if !exists {
		return
	}

	// Remove from connections map
	delete(m.connections, connID)

	// Remove from user connections
	userConns := m.userConns[conn.UserID]
	for i, id := range userConns {
		if id == connID {
			m.userConns[conn.UserID] = append(userConns[:i], userConns[i+1:]...)
			break
		}
	}

	// Clean up empty user entry
	if len(m.userConns[conn.UserID]) == 0 {
		delete(m.userConns, conn.UserID)
	}

	// Close the done channel
	select {
	case <-conn.Done:
		// Already closed
	default:
		close(conn.Done)
	}
}

func (m *Manager) sendEventToConnection(conn *Connection, event Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("failed to marshal event data: %w", err)
	}

	var eventStr strings.Builder

	if event.ID != "" {
		eventStr.WriteString(fmt.Sprintf("id: %s\n", event.ID))
	}

	if event.Type != "" {
		eventStr.WriteString(fmt.Sprintf("event: %s\n", event.Type))
	}

	eventStr.WriteString(fmt.Sprintf("data: %s\n", string(data)))

	if event.Retry > 0 {
		eventStr.WriteString(fmt.Sprintf("retry: %d\n", event.Retry))
	}

	eventStr.WriteString("\n")

	_, err = conn.Writer.Write([]byte(eventStr.String()))
	if err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}

	if flusher, ok := conn.Writer.(http.Flusher); ok {
		flusher.Flush()
	}

	return nil
}

func (m *Manager) sendToLocalConnections(userID string, event Event) {
	m.mutex.RLock()
	connIDs := make([]string, len(m.userConns[userID]))
	copy(connIDs, m.userConns[userID])
	m.mutex.RUnlock()

	for _, connID := range connIDs {
		m.mutex.RLock()
		conn, exists := m.connections[connID]
		m.mutex.RUnlock()

		if !exists {
			continue
		}

		if err := m.sendEventToConnection(conn, event); err != nil {
			log.Printf("Failed to send event to connection %s: %v", connID, err)
			m.removeConnection(connID)
		}
	}
}

func (m *Manager) publishToRedis(userID string, event Event) error {
	message := map[string]interface{}{
		"user_id": userID,
		"event":   event,
	}

	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal redis message: %w", err)
	}

	return m.redisClient.Publish(m.ctx, "sse_events", string(data)).Err()
}

func (m *Manager) subscribeToRedis() {
	pubsub := m.redisClient.Subscribe(m.ctx, "sse_events")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-m.ctx.Done():
			return
		case msg := <-ch:
			var message struct {
				UserID string `json:"user_id"`
				Event  Event  `json:"event"`
			}

			if err := json.Unmarshal([]byte(msg.Payload), &message); err != nil {
				log.Printf("Failed to unmarshal redis message: %v", err)
				continue
			}

			if message.UserID == "*" {
				// Broadcast to all users
				m.mutex.RLock()
				userIDs := make([]string, 0, len(m.userConns))
				for userID := range m.userConns {
					userIDs = append(userIDs, userID)
				}
				m.mutex.RUnlock()

				for _, userID := range userIDs {
					m.sendToLocalConnections(userID, message.Event)
				}
			} else {
				// Send to specific user
				m.sendToLocalConnections(message.UserID, message.Event)
			}
		}
	}
}

func (m *Manager) storeConnectionInRedis(conn *Connection) {
	key := fmt.Sprintf("sse_conn:%s", conn.ID)
	data := map[string]interface{}{
		"user_id":   conn.UserID,
		"conn_id":   conn.ID,
		"timestamp": time.Now().Unix(),
	}

	jsonData, _ := json.Marshal(data)
	// Store for 30 minutes instead of 5 to handle longer connections
	m.redisClient.SetEx(m.ctx, key, string(jsonData), 30*time.Minute)
}

func (m *Manager) removeConnectionFromRedis(connID string) {
	key := fmt.Sprintf("sse_conn:%s", connID)
	m.redisClient.Del(m.ctx, key)
}

func (m *Manager) startCleanup() {
	// Run cleanup less frequently to avoid disrupting active connections
	m.cleanupTicker = time.NewTicker(10 * time.Minute)
	go func() {
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-m.cleanupTicker.C:
				m.cleanupStaleConnections()
			}
		}
	}()
}

func (m *Manager) cleanupStaleConnections() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Only consider connections stale after 45 minutes of inactivity
	staleThreshold := time.Now().Add(-45 * time.Minute)
	var staleConnIDs []string

	for connID, conn := range m.connections {
		if conn.LastSeen.Before(staleThreshold) {
			staleConnIDs = append(staleConnIDs, connID)
		}
	}

	for _, connID := range staleConnIDs {
		log.Printf("Cleaning up stale connection: %s", connID)
		m.removeConnection(connID)
		m.removeConnectionFromRedis(connID)
	}
}
