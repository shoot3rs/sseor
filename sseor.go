// Package sseor provides a production-ready Server-Sent Events (SSE) manager with Redis backend
package sseor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// Context keys for user data
type ContextKey string

const (
	ClaimsContextKey  ContextKey = "claims"
	CountryContextKey ContextKey = "country"
	StateContextKey   ContextKey = "state"
	UserIDContextKey  ContextKey = "user_id"
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
	limiter  *rate.Limiter // Per-connection rate limiter
}

// HandlerFunc defines the signature for SSE route handlers
type HandlerFunc func(w http.ResponseWriter, r *http.Request, manager *Manager)

// AuthFunc defines the signature for authentication handlers
type AuthFunc func(r *http.Request) (userID string, err error)

// Config holds configuration for the Manager
type Config struct {
	RedisURL          string
	RedisPoolSize     int
	RedisMinIdleConns int
	RedisMaxRetries   int
	RateLimitRPS      int
	RateLimitBurst    int
	CORSOrigins       []string
	AuthRequired      bool
	HealthCheckPath   string
	MetricsPath       string
	LogLevel          string
	// OIDC Configuration
	OIDCIssuerURL    string
	OIDCClientID     string
	RequireNamespace bool // Require X-Country-Iso2 and X-State-Iso2 headers
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		RedisURL:          "redis://localhost:6379",
		RedisPoolSize:     50,
		RedisMinIdleConns: 10,
		RedisMaxRetries:   3,
		RateLimitRPS:      10,
		RateLimitBurst:    20,
		CORSOrigins:       []string{"*"},
		AuthRequired:      false,
		HealthCheckPath:   "/health",
		MetricsPath:       "/metrics",
		LogLevel:          "info",
		RequireNamespace:  false,
	}
}

// Metrics holds connection and performance metrics
type Metrics struct {
	ActiveConnections     int64          `json:"active_connections"`
	TotalConnections      int64          `json:"total_connections"`
	ConnectionsPerUser    map[string]int `json:"connections_per_user"`
	MessagesSent          int64          `json:"messages_sent"`
	MessagesReceived      int64          `json:"messages_received"`
	RateLimitedRequests   int64          `json:"rate_limited_requests"`
	AuthFailures          int64          `json:"auth_failures"`
	RedisConnectionErrors int64          `json:"redis_connection_errors"`
	Uptime                int64          `json:"uptime_seconds"`
}

// Claims represents OIDC token claims
type Claims struct {
	Exp            int64    `json:"exp"`
	Iat            int64    `json:"iat"`
	Jti            string   `json:"jti"`
	Iss            string   `json:"iss"`
	Aud            []string `json:"aud"`
	Id             string   `json:"sub"`
	Typ            string   `json:"typ"`
	Azp            string   `json:"azp"`
	Acr            string   `json:"acr"`
	AllowedOrigins []string `json:"allowed-origins"`

	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`

	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`

	Scope             string `json:"scope"`
	Sid               string `json:"sid,omitempty"`
	SessionState      string `json:"session_state,omitempty"`
	Country           string `json:"country,omitempty"`
	State             string `json:"state,omitempty"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name,omitempty"`
	PreferredUsername string `json:"preferred_username"`
	GivenName         string `json:"given_name,omitempty"`
	FamilyName        string `json:"family_name,omitempty"`
	Email             string `json:"email,omitempty"`
	ClientHost        string `json:"clientHost,omitempty"`
	ClientAddress     string `json:"clientAddress,omitempty"`
	ClientID          string `json:"client_id,omitempty"`
}

// ExpiresAt returns the expiration time as time.Time
func (c *Claims) ExpiresAt() time.Time {
	return time.Unix(c.Exp, 0)
}

// IssuedAt returns the issue time as time.Time
func (c *Claims) IssuedAt() time.Time {
	return time.Unix(c.Iat, 0)
}

// IsExpired checks if the token has expired
func (c *Claims) IsExpired() bool {
	return time.Now().After(c.ExpiresAt())
}

func (c *Claims) HasRole(role string) bool {
	return slices.Contains(c.RealmAccess.Roles, role)
}

func (c *Claims) IsClientToken() bool {
	// If preferred_username starts with "service-account-" it's a client credentials token
	if len(c.PreferredUsername) >= 16 && c.PreferredUsername[:15] == "service-account" {
		return true
	}

	// If there is no email or name, and client_id is present, it's probably a client token
	if c.ClientID != "" && c.Name == "" && c.Email == "" {
		return true
	}

	return false
}

func (c *Claims) GetRole() string {
	defaultRoles := []string{
		"default-roles-shooters",
		"default-roles-gh-realm",
		"offline_access",
		"uma_authorization",
	}

	for _, role := range c.RealmAccess.Roles {
		if !slices.Contains(defaultRoles, role) {
			return role
		}
	}

	return ""
}

func (c *Claims) String() string {
	jb, _ := json.MarshalIndent(c, "", " \t")
	return string(jb)
}

// GetUserID returns the user ID from claims, preferring the sub claim
func (c *Claims) GetUserID() string {
	if c.Id != "" {
		return c.Id
	}
	if c.PreferredUsername != "" {
		return c.PreferredUsername
	}
	if c.Email != "" {
		return c.Email
	}
	return ""
}

// Manager manages SSE connections with Redis backend and production features
type Manager struct {
	config        *Config
	redisClient   *redis.Client
	connections   map[string]*Connection // connectionID -> Connection
	userConns     map[string][]string    // userID -> []connectionID
	mutex         sync.RWMutex
	router        *mux.Router
	server        *http.Server
	cleanupTicker *time.Ticker
	ctx           context.Context
	cancel        context.CancelFunc
	logger        *slog.Logger
	authFunc      AuthFunc
	metrics       *Metrics
	metricsLock   sync.RWMutex
	startTime     time.Time
	rateLimiter   *rate.Limiter // Global rate limiter
	oidcVerifier  *oidc.IDTokenVerifier
}

// NewManager creates a new SSE Manager with enhanced features
func NewManager(config *Config) (*Manager, error) {
	if config == nil {
		config = DefaultConfig()
	}

	// Validate configuration
	if err := ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Initialize structured logger
	var logger *slog.Logger
	switch config.LogLevel {
	case "debug":
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	case "warn":
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	case "error":
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	default:
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	// Parse Redis URL
	opt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redis URL: %w", err)
	}

	// Configure Redis connection pooling
	opt.PoolSize = config.RedisPoolSize
	opt.MinIdleConns = config.RedisMinIdleConns
	opt.MaxRetries = config.RedisMaxRetries
	opt.PoolTimeout = 30 * time.Second
	opt.ConnMaxIdleTime = 5 * time.Minute
	opt.ConnMaxLifetime = 30 * time.Minute

	// Create Redis client with connection pooling
	client := redis.NewClient(opt)

	// Test Redis connection
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	manager := &Manager{
		config:      config,
		redisClient: client,
		connections: make(map[string]*Connection),
		userConns:   make(map[string][]string),
		router:      mux.NewRouter(),
		ctx:         ctx,
		cancel:      cancel,
		logger:      logger,
		metrics: &Metrics{
			ConnectionsPerUser: make(map[string]int),
		},
		startTime:   time.Now(),
		rateLimiter: rate.NewLimiter(rate.Limit(config.RateLimitRPS), config.RateLimitBurst),
	}

	// Initialize OIDC verifier if configuration is provided
	if config.OIDCIssuerURL != "" && config.OIDCClientID != "" {
		if err := manager.initOIDCVerifier(); err != nil {
			return nil, fmt.Errorf("failed to initialize OIDC verifier: %w", err)
		}
		// Set default auth function to use OIDC
		manager.SetAuthFunc(manager.OIDCAuthFunc)
	}

	// Setup middleware
	manager.setupMiddleware()

	// Setup health check endpoint
	manager.setupHealthCheck()

	// Setup metrics endpoint
	manager.setupMetrics()

	// Start cleanup routine
	manager.startCleanup()

	// Subscribe to Redis for cross-service messaging
	go manager.subscribeToRedis()

	logger.Info("🚀 [SSEOR] SSE Manager initialized",
		"redis_url", config.RedisURL,
		"pool_size", config.RedisPoolSize,
		"rate_limit_rps", config.RateLimitRPS,
		"oidc_enabled", manager.oidcVerifier != nil,
	)

	return manager, nil
}

// initOIDCVerifier initializes the OIDC token verifier
func (m *Manager) initOIDCVerifier() error {
	ctx := context.Background()

	provider, err := oidc.NewProvider(ctx, m.config.OIDCIssuerURL)
	if err != nil {
		return fmt.Errorf("failed to create OIDC provider: %w", err)
	}

	oidcConfig := &oidc.Config{
		ClientID: m.config.OIDCClientID,
	}

	m.oidcVerifier = provider.Verifier(oidcConfig)

	m.logger.Info("OIDC verifier initialized",
		"issuer", m.config.OIDCIssuerURL,
		"client_id", m.config.OIDCClientID,
	)

	return nil
}

// OIDCAuthFunc is the built-in OIDC authentication function
func (m *Manager) OIDCAuthFunc(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", fmt.Errorf("authorization header required")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", fmt.Errorf("invalid authorization header format")
	}

	token := parts[1]

	// Verify the token
	idToken, err := m.oidcVerifier.Verify(r.Context(), token)
	if err != nil {
		return "", fmt.Errorf("token verification failed: %w", err)
	}

	// Parse claims
	claims := new(Claims)
	if err := idToken.Claims(claims); err != nil {
		return "", fmt.Errorf("failed to parse claims: %w", err)
	}

	// Check if token is expired
	if claims.IsExpired() {
		return "", fmt.Errorf("token has expired")
	}

	// Store claims in request context for later use
	ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
	*r = *r.WithContext(ctx)

	// Return user ID
	userID := claims.GetUserID()
	if userID == "" {
		return "", fmt.Errorf("no user ID found in token claims")
	}

	return userID, nil
}

// SetAuthFunc sets the authentication function
func (m *Manager) SetAuthFunc(authFunc AuthFunc) {
	m.authFunc = authFunc
}

// Route registers an SSE route with optional middleware
func (m *Manager) Route(path string, handler HandlerFunc) {
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, m)
	})

	// Convert to http.Handler for middleware chain
	var finalHandler http.Handler = wrappedHandler

	// Apply authentication middleware if required
	if m.config.AuthRequired && m.authFunc != nil {
		finalHandler = m.AuthMiddleware(finalHandler)
	}

	// Apply namespace middleware if required
	if m.config.RequireNamespace {
		finalHandler = m.NamespaceMiddleware(finalHandler)
	}

	// Convert back to HandlerFunc for router registration
	m.router.Handle(path, finalHandler).Methods("GET")
}

// AuthMiddleware provides authentication middleware
func (m *Manager) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.logger.Debug("👮 [AuthMiddleware]: Authenticating request")

		userID, err := m.authFunc(r)
		if err != nil {
			m.updateMetric(func(m *Metrics) { m.AuthFailures++ })
			m.logger.Warn("Authentication failed",
				"error", err,
				"client_ip", m.getClientIP(r),
			)
			http.Error(w, fmt.Sprintf("Authentication failed: %s", err), http.StatusUnauthorized)
			return
		}

		// Store user ID in context
		ctx := context.WithValue(r.Context(), UserIDContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NamespaceMiddleware provides namespace validation middleware
func (m *Manager) NamespaceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		country := r.Header.Get("X-Country-Iso2")
		state := r.Header.Get("X-State-Iso2")

		if country == "" {
			m.logger.Warn("Missing X-Country-Iso2 header", "client_ip", m.getClientIP(r))
			http.Error(w, "X-Country-Iso2 header required", http.StatusBadRequest)
			return
		}

		if state == "" {
			m.logger.Warn("Missing X-State-Iso2 header", "client_ip", m.getClientIP(r))
			http.Error(w, "X-State-Iso2 header required", http.StatusBadRequest)
			return
		}

		// Store namespace info in context
		ctx := context.WithValue(r.Context(), CountryContextKey, strings.ToUpper(country))
		ctx = context.WithValue(ctx, StateContextKey, strings.ToUpper(state))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetClaimsFromContext retrieves claims from request context
func (m *Manager) GetClaimsFromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(ClaimsContextKey).(*Claims)
	return claims, ok
}

// GetUserIDFromContext retrieves user ID from request context
func (m *Manager) GetUserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(UserIDContextKey).(string)
	return userID, ok
}

// GetNamespaceFromContext retrieves namespace info from request context
func (m *Manager) GetNamespaceFromContext(ctx context.Context) (country, state string, ok bool) {
	c, cOk := ctx.Value(CountryContextKey).(string)
	s, sOk := ctx.Value(StateContextKey).(string)
	return c, s, cOk && sOk
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

	m.logger.Info("Starting SSE server", "addr", addr)
	return m.server.ListenAndServe()
}

// HandleSSE handles incoming SSE connections with authentication and rate limiting
func (m *Manager) HandleSSE(w http.ResponseWriter, r *http.Request, userID string) error {
	// Global rate limiting
	if !m.rateLimiter.Allow() {
		m.updateMetric(func(m *Metrics) { m.RateLimitedRequests++ })
		m.logger.Warn("Rate limit exceeded", "client_ip", m.getClientIP(r))
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return fmt.Errorf("rate limit exceeded")
	}

	// If userID is empty, try to get it from context (set by middleware)
	if userID == "" {
		if contextUserID, ok := m.GetUserIDFromContext(r.Context()); ok {
			userID = contextUserID
		} else if m.config.AuthRequired {
			m.logger.Warn("No user ID provided and none found in context")
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return fmt.Errorf("no user ID available")
		} else {
			// Generate anonymous user ID if auth is not required
			userID = fmt.Sprintf("anonymous_%d", time.Now().UnixNano())
		}
	}

	// Set comprehensive CORS headers
	origin := r.Header.Get("Origin")
	if m.isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control, Authorization, Content-Type, X-Country-Iso2, X-State-Iso2")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Max-Age", "86400")

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Generate unique connection ID
	connID := fmt.Sprintf("%s_%d", userID, time.Now().UnixNano())

	// Create per-connection rate limiter
	connLimiter := rate.NewLimiter(rate.Limit(m.config.RateLimitRPS), m.config.RateLimitBurst)

	// Create connection
	conn := &Connection{
		ID:       connID,
		UserID:   userID,
		Writer:   w,
		Request:  r,
		Done:     make(chan bool),
		LastSeen: time.Now(),
		limiter:  connLimiter,
	}

	// Add connection to manager
	m.addConnection(conn)
	defer m.removeConnection(connID)

	// Update metrics
	m.updateMetric(func(m *Metrics) {
		m.TotalConnections++
		m.ActiveConnections++
		m.ConnectionsPerUser[userID]++
	})

	// Get additional context data for logging
	var logData []interface{}
	logData = append(logData,
		"connection_id", connID,
		"user_id", userID,
		"client_ip", m.getClientIP(r),
		"user_agent", r.UserAgent(),
	)

	// Add namespace info if available
	if country, state, ok := m.GetNamespaceFromContext(r.Context()); ok {
		logData = append(logData, "country", country, "state", state)
	}

	// Add claims info if available
	if claims, ok := m.GetClaimsFromContext(r.Context()); ok {
		logData = append(logData,
			"preferred_username", claims.PreferredUsername,
			"email", claims.Email,
			"roles", claims.RealmAccess.Roles,
		)
	}

	m.logger.Info("New SSE connection", logData...)

	// Prepare initial event data
	initialEventData := map[string]interface{}{
		"connection_id": connID,
		"user_id":       userID,
		"timestamp":     time.Now().Unix(),
	}

	// Add namespace info to initial event if available
	if country, state, ok := m.GetNamespaceFromContext(r.Context()); ok {
		initialEventData["country"] = country
		initialEventData["state"] = state
	}

	// Send initial connection event
	initialEvent := Event{
		Type: "connected",
		Data: initialEventData,
	}

	if err := m.sendEventToConnection(conn, initialEvent); err != nil {
		return err
	}

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

	// Send periodic heartbeat every 30 seconds
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	// Update last seen every 5 minutes in Redis
	redisTicker := time.NewTicker(5 * time.Minute)
	defer redisTicker.Stop()

	for {
		select {
		case <-conn.Done:
			m.logger.Debug("Connection done", "connection_id", connID)
			return nil
		case <-r.Context().Done():
			m.logger.Debug("Request context done", "connection_id", connID)
			return nil
		case <-m.ctx.Done():
			m.logger.Debug("Manager context done", "connection_id", connID)
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
				m.logger.Error("Failed to send heartbeat",
					"connection_id", connID,
					"error", err,
				)
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

	// Update metrics
	m.updateMetric(func(m *Metrics) { m.MessagesSent++ })

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

	// Update metrics
	m.updateMetric(func(m *Metrics) { m.MessagesSent += int64(len(userIDs)) })

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

// GetMetrics returns current metrics
func (m *Manager) GetMetrics() Metrics {
	m.metricsLock.RLock()
	defer m.metricsLock.RUnlock()

	// Update uptime
	m.metrics.Uptime = int64(time.Since(m.startTime).Seconds())

	// Create a copy to avoid race conditions
	metricsCopy := *m.metrics
	metricsCopy.ConnectionsPerUser = make(map[string]int)
	for k, v := range m.metrics.ConnectionsPerUser {
		metricsCopy.ConnectionsPerUser[k] = v
	}

	return metricsCopy
}

// Close gracefully shuts down the manager
func (m *Manager) Close() error {
	m.logger.Info("Shutting down SSE Manager")
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
		m.logger.Error("Failed to close redis client", "error", err)
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

// Private methods (rest of the implementation continues with the existing methods...)
// [The rest of the private methods remain the same as in your original code]

func (m *Manager) setupMiddleware() {
	// CORS middleware would be applied here if needed
	// Currently commented out as in original
}

func (m *Manager) setupHealthCheck() {
	m.router.HandleFunc(m.config.HealthCheckPath, func(w http.ResponseWriter, r *http.Request) {
		// Check Redis connection
		if err := m.redisClient.Ping(r.Context()).Err(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"error":  err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "healthy",
			"connections":        m.GetTotalConnections(),
			"uptime":             int64(time.Since(m.startTime).Seconds()),
			"oidc_enabled":       m.oidcVerifier != nil,
			"auth_required":      m.config.AuthRequired,
			"namespace_required": m.config.RequireNamespace,
		})
	}).Methods("GET")
}

func (m *Manager) setupMetrics() {
	m.router.HandleFunc(m.config.MetricsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.GetMetrics())
	}).Methods("GET")
}

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

	// Update metrics
	m.updateMetric(func(m *Metrics) {
		m.ActiveConnections--
		if m.ConnectionsPerUser[conn.UserID] > 0 {
			m.ConnectionsPerUser[conn.UserID]--
		}
		if m.ConnectionsPerUser[conn.UserID] == 0 {
			delete(m.ConnectionsPerUser, conn.UserID)
		}
	})

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

	m.logger.Debug("Connection removed", "connection_id", connID, "user_id", conn.UserID)
}

func (m *Manager) sendEventToConnection(conn *Connection, event Event) error {
	// Check per-connection rate limit
	if !conn.limiter.Allow() {
		m.updateMetric(func(m *Metrics) { m.RateLimitedRequests++ })
		return fmt.Errorf("connection rate limit exceeded")
	}

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
			m.logger.Error("Failed to send event to connection",
				"connection_id", connID,
				"user_id", userID,
				"error", err,
			)
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

	err = m.redisClient.Publish(m.ctx, "sse_events", string(data)).Err()
	if err != nil {
		m.updateMetric(func(m *Metrics) { m.RedisConnectionErrors++ })
		m.logger.Error("Failed to publish to Redis", "error", err)
	}
	return err
}

func (m *Manager) subscribeToRedis() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
			pubsub := m.redisClient.Subscribe(m.ctx, "sse_events")
			ch := pubsub.Channel()

			m.logger.Info("Subscribed to Redis SSE events")

			func() {
				defer pubsub.Close()

				for {
					select {
					case <-m.ctx.Done():
						return
					case msg, ok := <-ch:
						if !ok {
							m.logger.Warn("Redis subscription channel closed")
							return
						}

						m.updateMetric(func(m *Metrics) { m.MessagesReceived++ })

						var message struct {
							UserID string `json:"user_id"`
							Event  Event  `json:"event"`
						}

						if err := json.Unmarshal([]byte(msg.Payload), &message); err != nil {
							m.logger.Error("Failed to unmarshal redis message", "error", err)
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
			}()

			// If we get here, the subscription was closed. Wait and retry.
			m.updateMetric(func(m *Metrics) { m.RedisConnectionErrors++ })
			m.logger.Warn("Redis subscription lost, retrying in 5 seconds")
			time.Sleep(5 * time.Second)
		}
	}
}

func (m *Manager) storeConnectionInRedis(conn *Connection) {
	key := fmt.Sprintf("sse_conn:%s", conn.ID)
	data := map[string]interface{}{
		"user_id":    conn.UserID,
		"conn_id":    conn.ID,
		"timestamp":  time.Now().Unix(),
		"client_ip":  m.getClientIP(conn.Request),
		"user_agent": conn.Request.UserAgent(),
	}

	// Add namespace info if available
	if country, state, ok := m.GetNamespaceFromContext(conn.Request.Context()); ok {
		data["country"] = country
		data["state"] = state
	}

	// Add claims info if available
	if claims, ok := m.GetClaimsFromContext(conn.Request.Context()); ok {
		data["preferred_username"] = claims.PreferredUsername
		data["email"] = claims.Email
		data["roles"] = claims.RealmAccess.Roles
	}

	jsonData, _ := json.Marshal(data)
	// Store for 30 minutes
	if err := m.redisClient.SetEx(m.ctx, key, string(jsonData), 30*time.Minute).Err(); err != nil {
		m.updateMetric(func(m *Metrics) { m.RedisConnectionErrors++ })
		m.logger.Error("Failed to store connection in Redis", "error", err)
	}
}

func (m *Manager) removeConnectionFromRedis(connID string) {
	key := fmt.Sprintf("sse_conn:%s", connID)
	if err := m.redisClient.Del(m.ctx, key).Err(); err != nil {
		m.logger.Error("Failed to remove connection from Redis", "error", err)
	}
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
		m.logger.Info("Cleaning up stale connection", "connection_id", connID)
		m.removeConnection(connID)
		m.removeConnectionFromRedis(connID)
	}
}

func (m *Manager) updateMetric(updateFunc func(*Metrics)) {
	m.metricsLock.Lock()
	updateFunc(m.metrics)
	m.metricsLock.Unlock()
}

func (m *Manager) getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	if strings.Contains(ip, ":") {
		host, _, _ := strings.Cut(ip, ":")
		return host
	}
	return ip
}

func (m *Manager) isAllowedOrigin(origin string) bool {
	if len(m.config.CORSOrigins) == 0 {
		return false
	}

	for _, allowed := range m.config.CORSOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

// Utility functions for common authentication patterns

// TokenAuthFunc creates an authentication function that validates Bearer tokens
func TokenAuthFunc(validateToken func(token string) (userID string, err error)) AuthFunc {
	return func(r *http.Request) (string, error) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			return "", fmt.Errorf("missing authorization header")
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			return "", fmt.Errorf("invalid authorization header format")
		}

		return validateToken(parts[1])
	}
}

// QueryParamAuthFunc creates an authentication function that validates query parameters
func QueryParamAuthFunc(paramName string, validateParam func(value string) (userID string, err error)) AuthFunc {
	return func(r *http.Request) (string, error) {
		value := r.URL.Query().Get(paramName)
		if value == "" {
			return "", fmt.Errorf("missing %s query parameter", paramName)
		}

		return validateParam(value)
	}
}

// Example usage and middleware helpers

// RateLimitMiddleware creates a middleware for additional rate limiting
func (m *Manager) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.rateLimiter.Allow() {
			m.updateMetric(func(m *Metrics) { m.RateLimitedRequests++ })
			m.logger.Warn("Global rate limit exceeded",
				"client_ip", m.getClientIP(r),
				"path", r.URL.Path,
			)
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware creates a structured logging middleware
func (m *Manager) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap the ResponseWriter to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)

		m.logger.Info("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
			"client_ip", m.getClientIP(r),
			"user_agent", r.UserAgent(),
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// GetRedisStats returns Redis connection pool statistics
func (m *Manager) GetRedisStats() map[string]interface{} {
	stats := m.redisClient.PoolStats()
	return map[string]interface{}{
		"hits":        stats.Hits,
		"misses":      stats.Misses,
		"timeouts":    stats.Timeouts,
		"total_conns": stats.TotalConns,
		"idle_conns":  stats.IdleConns,
		"stale_conns": stats.StaleConns,
	}
}

// ValidateConfig validates the configuration
func ValidateConfig(config *Config) error {
	if config.RedisURL == "" {
		return fmt.Errorf("redis URL cannot be empty")
	}

	if config.RedisPoolSize <= 0 {
		return fmt.Errorf("redis pool size must be positive")
	}

	if config.RateLimitRPS <= 0 {
		return fmt.Errorf("rate limit RPS must be positive")
	}

	if config.RateLimitBurst <= 0 {
		return fmt.Errorf("rate limit burst must be positive")
	}

	// Validate OIDC config if authentication is required
	if config.AuthRequired {
		if config.OIDCIssuerURL != "" && config.OIDCClientID == "" {
			return fmt.Errorf("OIDC client ID is required when issuer URL is provided")
		}
		if config.OIDCClientID != "" && config.OIDCIssuerURL == "" {
			return fmt.Errorf("OIDC issuer URL is required when client ID is provided")
		}
	}

	return nil
}

// ConfigFromEnv creates configuration from environment variables
func ConfigFromEnv() *Config {
	config := DefaultConfig()

	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		config.RedisURL = redisURL
	}

	if poolSize := os.Getenv("REDIS_POOL_SIZE"); poolSize != "" {
		if size, err := strconv.Atoi(poolSize); err == nil {
			config.RedisPoolSize = size
		}
	}

	if minIdle := os.Getenv("REDIS_MIN_IDLE"); minIdle != "" {
		if idle, err := strconv.Atoi(minIdle); err == nil {
			config.RedisMinIdleConns = idle
		}
	}

	if rps := os.Getenv("RATE_LIMIT_RPS"); rps != "" {
		if rate, err := strconv.Atoi(rps); err == nil {
			config.RateLimitRPS = rate
		}
	}

	if burst := os.Getenv("RATE_LIMIT_BURST"); burst != "" {
		if b, err := strconv.Atoi(burst); err == nil {
			config.RateLimitBurst = b
		}
	}

	if origins := os.Getenv("CORS_ORIGINS"); origins != "" {
		config.CORSOrigins = strings.Split(origins, ",")
		for i, origin := range config.CORSOrigins {
			config.CORSOrigins[i] = strings.TrimSpace(origin)
		}
	}

	if auth := os.Getenv("AUTH_REQUIRED"); auth != "" {
		config.AuthRequired = strings.ToLower(auth) == "true"
	}

	if namespace := os.Getenv("REQUIRE_NAMESPACE"); namespace != "" {
		config.RequireNamespace = strings.ToLower(namespace) == "true"
	}

	if healthPath := os.Getenv("HEALTH_CHECK_PATH"); healthPath != "" {
		config.HealthCheckPath = healthPath
	}

	if metricsPath := os.Getenv("METRICS_PATH"); metricsPath != "" {
		config.MetricsPath = metricsPath
	}

	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		config.LogLevel = strings.ToLower(logLevel)
	}

	// OIDC configuration
	if issuer := os.Getenv("OIDC_ISSUER_URL"); issuer != "" {
		config.OIDCIssuerURL = issuer
	}

	if clientID := os.Getenv("OIDC_CLIENT_ID"); clientID != "" {
		config.OIDCClientID = clientID
	}

	return config
}
