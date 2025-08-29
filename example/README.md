# Transaction SSE Monitoring System

This system provides real-time transaction monitoring using Server-Sent Events (SSE) with user authentication and ownership validation.

## Architecture Overview

The system consists of:

1. **Transaction API Server** (Port 8080) - Handles transaction creation and retrieval
2. **SSE Manager Server** (Port 9090) - Manages real-time SSE connections
3. **Redis Backend** - Stores transactions and manages cross-service messaging
4. **Web Client** - HTML interface for testing and demonstration

## Features

- ✅ Real-time transaction status updates via SSE
- ✅ User authentication with OIDC/JWT token validation
- ✅ Transaction ownership validation
- ✅ Automatic webhook simulation for payment processing
- ✅ Cross-service messaging via Redis pub/sub
- ✅ Automatic reconnection with exponential backoff
- ✅ Rate limiting and connection management
- ✅ Health checks and metrics endpoints

## API Endpoints

### Transaction API (Port 8080)

#### Create Deposit
```
POST /api/v1/transactions/deposit
Content-Type: application/json
Authorization: Bearer <token> (if auth enabled)

{
    "user_id": "user123",
    "amount": 100.00
}
```

**Response:**
```json
{
    "transaction": {
        "id": "txn_1234567890_1234",
        "user_id": "user123",
        "amount": 100.00,
        "status": "pending",
        "created_at": "2025-08-29T10:00:00Z",
        "updated_at": "2025-08-29T10:00:00Z",
        "description": "Wallet deposit of 100.00",
        "reference": "REF_1234567890"
    },
    "message": "Deposit initiated successfully"
}
```

#### Get Transaction
```
GET /api/v1/transactions/{transactionId}
Authorization: Bearer <token> (if auth enabled)
```

### SSE Endpoints (Port 9090)

#### Monitor Specific Transaction
```
GET /api/v1/sse/transactions/{transactionId}
Authorization: Bearer <token> (if auth enabled)
```

This endpoint:
1. Validates user authentication
2. Retrieves the transaction
3. Verifies user ownership
4. Establishes SSE connection
5. Sends initial transaction state
6. Streams real-time updates

#### Health Check
```
GET /health
```

#### Metrics
```
GET /metrics
```

## SSE Event Types

### Connection Events
- `connected` - Connection established
- `heartbeat` - Keep-alive ping (every 30 seconds)

### Transaction Events
- `transaction.current` - Initial transaction state
- `transaction.created` - New transaction created
- `transaction.status_updated` - Status changed
- `transaction.completed` - Transaction completed successfully
- `transaction.failed` - Transaction failed

## Transaction Status Flow

```
pending → processing → completed
                   → failed
```

1. **pending** - Transaction created, awaiting processing
2. **processing** - Payment processor is handling the transaction
3. **completed** - Transaction successful
4. **failed** - Transaction failed

## Configuration

### Environment Variables

```bash
# Redis Configuration
REDIS_URL=redis://localhost:6379
REDIS_POOL_SIZE=50
REDIS_MIN_IDLE=10

# Rate Limiting
RATE_LIMIT_RPS=10
RATE_LIMIT_BURST=20

# Authentication
AUTH_REQUIRED=true
OIDC_ISSUER_URL=https://your-keycloak.com/auth/realms/your-realm
OIDC_CLIENT_ID=your-client-id

# CORS
CORS_ORIGINS=*

# Logging
LOG_LEVEL=info

# Health & Metrics
HEALTH_CHECK_PATH=/health
METRICS_PATH=/metrics
```

### Default Configuration

```go
config := sseor.DefaultConfig()
config.AuthRequired = true
config.RequireNamespace = false
```

## Authentication

The system supports multiple authentication modes:

### 1. No Authentication (Development)
```go
config.AuthRequired = false
```

### 2. OIDC/JWT Token Authentication
```go
config.OIDCIssuerURL = "https://your-keycloak.com/auth/realms/your-realm"
config.OIDCClientID = "your-client-id"
config.AuthRequired = true
```

### 3. Custom Token Validation
```go
manager.SetAuthFunc(sseor.TokenAuthFunc(func(token string) (string, error) {
    // Your custom token validation logic
    return userID, nil
}))
```

## Security Features

### User Ownership Validation
- Each SSE connection validates that the authenticated user owns the requested transaction
- Prevents users from monitoring transactions they don't own
- Returns 403 Forbidden for unauthorized access attempts

### Rate Limiting
- Global rate limiting across all connections
- Per-connection rate limiting
- Configurable limits for requests per second and burst capacity

### CORS Support
- Configurable allowed origins
- Proper preflight handling
- Credential support for authenticated requests

## Webhook Simulation

The system automatically simulates payment processor webhooks:

1. **Initial Delay** - 2-10 seconds after transaction creation
2. **Processing Status** - Transaction moves to "processing"
3. **Payment Delay** - 5-15 seconds of processing simulation
4. **Final Status** - 90% success rate, 10% failure rate

## Usage Examples

### 1. Basic Usage (No Auth)
```go
config := sseor.DefaultConfig()
config.AuthRequired = false

manager, err := sseor.NewManager(config)
// ... setup and run
```

### 2. With OIDC Authentication
```go
config := sseor.DefaultConfig()
config.AuthRequired = true
config.OIDCIssuerURL = "https://keycloak.example.com/auth/realms/app"
config.OIDCClientID = "transaction-app"

manager, err := sseor.NewManager(config)
```

### 3. Client-Side JavaScript
```javascript
// Create a transaction first
const response = await fetch('/api/v1/transactions/deposit', {
    method: 'POST',
    headers: {
        'Content-Type': 'application/json',
        'Authorization': `Bearer ${token}`
    },
    body: JSON.stringify({
        user_id: 'user123',
        amount: 100.00
    })
});

const { transaction } = await response.json();

// Connect to SSE stream for this transaction
const eventSource = new EventSource(
    `/api/v1/sse/transactions/${transaction.id}?token=${token}`
);

eventSource.addEventListener('transaction.status_updated', (event) => {
    const data = JSON.parse(event.data);
    console.log('Status update:', data.transaction.status);
});
```

## Error Handling

### SSE Connection Errors
- Automatic reconnection with exponential backoff
- Maximum retry attempts (configurable)
- Graceful degradation when connection fails

### API Error Responses
- `400 Bad Request` - Invalid request data
- `401 Unauthorized` - Authentication required/failed
- `403 Forbidden` - Access denied (not transaction owner)
- `404 Not Found` - Transaction not found
- `429 Too Many Requests` - Rate limit exceeded
- `500 Internal Server Error` - Server error

## Monitoring and Observability

### Health Check Response
```json
{
    "status": "healthy",
    "connections": 5,
    "uptime": 3600,
    "oidc_enabled": true,
    "auth_required": true,
    "namespace_required": false
}
```

### Metrics Response
```json
{
    "active_connections": 5,
    "total_connections": 25,
    "connections_per_user": {
        "user123": 2,
        "user456": 1
    },
    "messages_sent": 150,
    "messages_received": 75,
    "rate_limited_requests": 2,
    "auth_failures": 1,
    "redis_connection_errors": 0,
    "uptime_seconds": 3600
}
```

## Testing

Use the provided HTML client (`updated_transaction_client.html`) to:

1. Configure authentication settings
2. Create deposit transactions
3. Monitor real-time status updates
4. Test different scenarios (success/failure)
5. Verify reconnection behavior

## Deployment Considerations

1. **Redis Setup** - Ensure Redis is properly configured with persistence
2. **Load Balancing** - SSE connections are sticky; consider session affinity
3. **Proxy Configuration** - Disable buffering for SSE streams (nginx: `proxy_buffering off`)
4. **Resource Limits** - Monitor memory usage with many concurrent connections
5. **TLS/SSL** - Use HTTPS in production for secure token transmission

## Troubleshooting

### Common Issues

1. **CORS Errors** - Check `CORS_ORIGINS` configuration
2. **Connection Drops** - Verify proxy/load balancer settings
3. **Auth Failures** - Validate OIDC configuration and token format
4. **Redis Errors** - Check Redis connectivity and credentials
5. **Rate Limiting** - Adjust RPS limits if legitimate traffic is blocked

### Debug Mode

Enable debug logging:
```bash
LOG_LEVEL=debug
```

This provides detailed logs for:
- Connection establishment
- Authentication attempts
- Event publishing
- Redis operations
- Error conditions