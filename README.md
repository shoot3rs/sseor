# SSEOR - Simple Server-Sent Events with Redis

A lightweight Go package for managing Server-Sent Events (SSE) connections with Redis backend for cross-service messaging.

## Features

- ✅ Simple and clean API
- ✅ Redis backend for scalability
- ✅ Cross-service message broadcasting
- ✅ Automatic connection cleanup
- ✅ Heartbeat mechanism
- ✅ User-based message targeting
- ✅ Graceful shutdown
- ✅ Connection pooling and management

## Installation

```bash
go mod init your-sse-service
go get github.com/go-redis/redis/v8
go get github.com/gorilla/mux
```

## Quick Start

```go
package main

import (
	"log"
	"os"

	"github.com/shoot3rs/sseor"
)

func main() {
	// Initialize SSE manager
	sse, err := sseor.NewManager("redis://localhost:6379/5")
	if err != nil {
		log.Fatalf("Failed to create SSE manager: %v", err)
	}
	defer sse.Close()

	// Create your handler
	transactionHandler := NewTransactionHandler()

	// Setup SSE route
	sse.Route("/api/v1/sse/transactions", transactionHandler.Handle)

	// Start server
	log.Println("🚀 [SSEOR]: Server started on port 9090")
	if err := sse.ListenAndServe(":9090"); err != nil {
		log.Println("👮 [SSEOR]: failed to start server:", err)
		os.Exit(1)
	}
}

```

## API Reference

### Manager Methods

- `NewManager(redisURL string) (*Manager, error)` - Create new SSE manager
- `Route(path string, handler HandlerFunc)` - Register SSE route
- `ListenAndServe(addr string) error` - Start HTTP server
- `HandleSSE(w, r, userID string) error` - Handle SSE connection
- `Publish(ctx, userID string, event Event) error` - Send event to user
- `PublishToAll(ctx context.Context, event Event) error` - Broadcast to all
- `GetConnectionCount(userID string) int` - Get user's active connections
- `GetTotalConnections() int` - Get total active connections
- `Close() error` - Graceful shutdown

### Event Structure

```go
type Event struct {
    ID    string                 `json:"id,omitempty"`
    Type  string                 `json:"type"`
    Data  map[string]interface{} `json:"data"`
    Retry int                    `json:"retry,omitempty"`
}
```

## Usage Examples

### 1. Basic Handler

```go
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request, manager *sseor.Manager) {
    userID := r.URL.Query().Get("user_id")
    if userID == "" {
        http.Error(w, "user_id required", http.StatusBadRequest)
        return
    }
    
    if err := manager.HandleSSE(w, r, userID); err != nil {
        log.Printf("SSE error: %v", err)
    }
}
```

### 2. Publishing Events

```go
// Send to specific user
event := sseor.Event{
    Type: "transaction.updated",
    Data: map[string]interface{}{
        "transaction_id": "txn_123",
        "status": "completed",
        "amount": 500.00,
    },
}

if err := manager.Publish(ctx, "user123", event); err != nil {
    log.Printf("Failed to publish event: %v", err)
}

// Broadcast to all users
if err := manager.PublishToAll(ctx, event); err != nil {
    log.Printf("Failed to broadcast event: %v", err)
}
```

### 3. Client-Side JavaScript

```javascript
const eventSource = new EventSource('http://localhost:9090/api/v1/sse/transactions?user_id=user123');

eventSource.addEventListener('transaction.updated', function(event) {
    const data = JSON.parse(event.data);
    console.log('Transaction update:', data);
});

eventSource.onerror = function(event) {
    console.error('SSE error:', event);
};
```

## Configuration

### Redis Connection

The Redis URL supports various formats:
- `redis://localhost:6379/0`
- `redis://user:password@localhost:6379/0`
- `rediss://localhost:6379/0` (SSL)

### Environment Variables

```bash
REDIS_URL=redis://localhost:6379/5
SERVER_PORT=9090
LOG_LEVEL=info
```

## Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Service A     │    │   Service B     │    │   Service C     │
│  (SSE Manager)  │    │  (SSE Manager)  │    │  (Publisher)    │
└─────────┬───────┘    └─────────┬───────┘    └─────────┬───────┘
          │                      │                      │
          └──────────────────────┼──────────────────────┘
                                 │
                    ┌─────────────┴─────────────┐
                    │      Redis PubSub         │
                    │   (Cross-service msg)     │
                    └───────────────────────────┘
```

## Testing

1. Start Redis:
   ```bash
   docker run -d -p 6379:6379 redis:alpine
   ```

2. Run the example:
   ```bash
   go run main.go
   ```

3. Open `client_example.html` in your browser

4. Test the flow:
    - Connect to SSE
    - Trigger a deposit
    - Watch real-time updates

## Production Considerations

- Use connection pooling for Redis
- Implement proper authentication
- Add rate limiting
- Monitor connection counts
- Use HTTPS in production
- Configure proper CORS headers
- Implement reconnection logic on client-side
- Add structured logging
- Set up health checks

## License

MIT License