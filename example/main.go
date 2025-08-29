package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"github.com/shoot3rs/sseor"
)

// Transaction represents a wallet deposit transaction
type Transaction struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Amount      float64   `json:"amount"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Description string    `json:"description,omitempty"`
	Reference   string    `json:"reference,omitempty"`
}

// TransactionStatus constants
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// DepositRequest represents the deposit request payload
type DepositRequest struct {
	Amount float64 `json:"amount"`
}

// DepositResponse represents the deposit response
type DepositResponse struct {
	Transaction *Transaction `json:"transaction"`
	Message     string       `json:"message"`
}

// TransactionService handles transaction operations
type TransactionService struct {
	redisClient *redis.Client
	sseManager  *sseor.Manager
}

// NewTransactionService creates a new transaction service
func NewTransactionService(redisClient *redis.Client, sseManager *sseor.Manager) *TransactionService {
	return &TransactionService{
		redisClient: redisClient,
		sseManager:  sseManager,
	}
}

// CreateDeposit creates a new deposit transaction
func (ts *TransactionService) CreateDeposit(ctx context.Context, userID string, req *DepositRequest) (*Transaction, error) {
	// Generate unique transaction ID
	transactionID := fmt.Sprintf("txn_%d_%d", time.Now().UnixNano(), rand.Intn(10000))

	// Create transaction
	transaction := &Transaction{
		ID:          transactionID,
		UserID:      userID,
		Amount:      req.Amount,
		Status:      StatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Description: fmt.Sprintf("Wallet deposit of %.2f", req.Amount),
		Reference:   fmt.Sprintf("REF_%d", time.Now().UnixNano()),
	}

	// Store transaction in Redis
	transactionJSON, err := json.Marshal(transaction)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal transaction: %w", err)
	}

	key := fmt.Sprintf("transaction:%s", transactionID)
	if err := ts.redisClient.SetEx(ctx, key, string(transactionJSON), 24*time.Hour).Err(); err != nil {
		return nil, fmt.Errorf("failed to store transaction in redis: %w", err)
	}

	// Publish transaction created event
	transactionUserKey := fmt.Sprintf("%s:transaction:%s", userID, transactionID)
	event := sseor.Event{
		Type: "transaction.created",
		Data: map[string]interface{}{
			"transaction": transaction,
			"message":     "Transaction created successfully",
		},
	}
	_ = ts.sseManager.Publish(ctx, transactionUserKey, event)

	// Start webhook simulation in background
	go ts.simulateWebhook(transactionID)

	return transaction, nil
}

// GetTransaction retrieves a transaction by ID
func (ts *TransactionService) GetTransaction(ctx context.Context, transactionID string) (*Transaction, error) {
	key := fmt.Sprintf("transaction:%s", transactionID)
	result, err := ts.redisClient.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("transaction not found")
		}
		return nil, fmt.Errorf("failed to get transaction from redis: %w", err)
	}

	var transaction Transaction
	if err := json.Unmarshal([]byte(result), &transaction); err != nil {
		return nil, fmt.Errorf("failed to unmarshal transaction: %w", err)
	}

	return &transaction, nil
}

// UpdateTransactionStatus updates the transaction status
func (ts *TransactionService) UpdateTransactionStatus(ctx context.Context, transactionID, status string) error {
	// Get existing transaction
	transaction, err := ts.GetTransaction(ctx, transactionID)
	if err != nil {
		return err
	}

	// Update status and timestamp
	transaction.Status = status
	transaction.UpdatedAt = time.Now()

	// Store updated transaction
	transactionJSON, err := json.Marshal(transaction)
	if err != nil {
		return fmt.Errorf("failed to marshal transaction: %w", err)
	}

	key := fmt.Sprintf("transaction:%s", transactionID)
	if err := ts.redisClient.SetEx(ctx, key, string(transactionJSON), 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to update transaction in redis: %w", err)
	}

	// Publish status update event
	eventType := "transaction.status_updated"
	if status == StatusCompleted {
		eventType = "transaction.completed"
	} else if status == StatusFailed {
		eventType = "transaction.failed"
	}

	transactionUserKey := fmt.Sprintf("%s:transaction:%s", transaction.UserID, transactionID)
	event := sseor.Event{
		Type: eventType,
		Data: map[string]interface{}{
			"transaction": transaction,
			"message":     "Transaction created successfully",
		},
	}
	_ = ts.sseManager.Publish(ctx, transactionUserKey, event)

	return nil
}

// simulateWebhook simulates payment processor webhook responses
func (ts *TransactionService) simulateWebhook(transactionID string) {
	ctx := context.Background()

	// Simulate processing delay (2-10 seconds)
	processingDelay := time.Duration(2+rand.Intn(8)) * time.Second
	time.Sleep(processingDelay)

	// Update to processing status
	if err := ts.UpdateTransactionStatus(ctx, transactionID, StatusProcessing); err != nil {
		log.Printf("Failed to update transaction to processing: %v", err)
		return
	}

	// Simulate payment processing (5-15 seconds)
	paymentDelay := time.Duration(5+rand.Intn(10)) * time.Second
	time.Sleep(paymentDelay)

	// Simulate success/failure (90% success rate)
	finalStatus := StatusCompleted
	if rand.Float32() < 0.1 {
		finalStatus = StatusFailed
	}

	if err := ts.UpdateTransactionStatus(ctx, transactionID, finalStatus); err != nil {
		log.Printf("Failed to update transaction to final status: %v", err)
	}
}

// TransactionServer handles HTTP endpoints
type TransactionServer struct {
	transactionService *TransactionService
	sseManager         *sseor.Manager
}

// NewTransactionServer creates a new transaction server
func NewTransactionServer(transactionService *TransactionService, sseManager *sseor.Manager) *TransactionServer {
	return &TransactionServer{
		transactionService: transactionService,
		sseManager:         sseManager,
	}
}

// HandleDeposit handles deposit creation
func (ts *TransactionServer) HandleDeposit(w http.ResponseWriter, r *http.Request) {
	var req DepositRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.Amount <= 0 {
		http.Error(w, "amount must be greater than 0", http.StatusBadRequest)
		return
	}

	// Get user ID from context (always required now)
	userID, ok := ts.sseManager.GetUserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Create deposit
	transaction, err := ts.transactionService.CreateDeposit(r.Context(), userID, &req)
	if err != nil {
		log.Printf("Failed to create deposit: %v", err)
		http.Error(w, "Failed to create deposit", http.StatusInternalServerError)
		return
	}

	// Return response
	response := DepositResponse{
		Transaction: transaction,
		Message:     "Deposit initiated successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// HandleTransactionSSE handles SSE connections for specific transactions
func (ts *TransactionServer) HandleTransactionSSE(w http.ResponseWriter, r *http.Request, manager *sseor.Manager) {
	// Get transaction ID from URL path
	vars := mux.Vars(r)
	transactionID := vars["transactionId"]

	if transactionID == "" {
		http.Error(w, "transaction ID is required", http.StatusBadRequest)
		return
	}

	// Handle authentication from query parameters for EventSource compatibility
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		// Try to get from query parameter
		if authParam := r.URL.Query().Get("authorization"); authParam != "" {
			r.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authParam))
		}
	}

	// Handle country/state from query parameters
	if country := r.URL.Query().Get("country"); country != "" {
		r.Header.Set("X-Country-Iso2", country)
	}
	if state := r.URL.Query().Get("state"); state != "" {
		r.Header.Set("X-State-Iso2", state)
	}

	// Get user ID from context (authentication is handled by middleware)
	userID, ok := manager.GetUserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "user authentication required", http.StatusUnauthorized)
		return
	}

	// Get and validate transaction ownership
	transaction, err := ts.transactionService.GetTransaction(r.Context(), transactionID)
	if err != nil {
		if err.Error() == "transaction not found" {
			http.Error(w, "Transaction not found", http.StatusNotFound)
		} else {
			log.Printf("Failed to get transaction: %v", err)
			http.Error(w, "Failed to get transaction", http.StatusInternalServerError)
		}
		return
	}

	// Validate user ownership
	if transaction.UserID != userID {
		http.Error(w, "Access denied: You don't own this transaction", http.StatusForbidden)
		return
	}

	// Handle SSE connection - use transaction-specific user key
	transactionUserKey := fmt.Sprintf("%s:transaction:%s", userID, transactionID)

	// Send initial transaction data after connection is established
	go func() {
		time.Sleep(100 * time.Millisecond)
		initialEvent := sseor.Event{
			Type: "transaction.current",
			Data: map[string]interface{}{
				"transaction": transaction,
				"message":     "Current transaction status",
			},
		}
		_ = manager.Publish(context.Background(), transactionUserKey, initialEvent)
	}()

	// Handle the SSE connection
	if err := manager.HandleSSE(w, r, transactionUserKey); err != nil {
		log.Printf("SSE connection error for transaction %s: %v", transactionID, err)
	}
}

// HandleGetTransaction handles GET requests for transaction details
func (ts *TransactionServer) HandleGetTransaction(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	transactionID := vars["transactionId"]

	if transactionID == "" {
		http.Error(w, "transaction ID is required", http.StatusBadRequest)
		return
	}

	// Get user ID from context (authentication is always required)
	userID, ok := ts.sseManager.GetUserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Get transaction
	transaction, err := ts.transactionService.GetTransaction(r.Context(), transactionID)
	if err != nil {
		if err.Error() == "transaction not found" {
			http.Error(w, "Transaction not found", http.StatusNotFound)
		} else {
			log.Printf("Failed to get transaction: %v", err)
			http.Error(w, "Failed to get transaction", http.StatusInternalServerError)
		}
		return
	}

	// Validate user ownership
	if transaction.UserID != userID {
		http.Error(w, "Access denied: You don't own this transaction", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(transaction)
}

// HandleUserInfo returns user information from JWT claims
func (ts *TransactionServer) HandleUserInfo(w http.ResponseWriter, r *http.Request) {
	// Get claims from context
	claims, ok := ts.sseManager.GetClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Return sanitized user info
	userInfo := map[string]interface{}{
		"user_id":            claims.GetUserID(),
		"preferred_username": claims.PreferredUsername,
		"email":              claims.Email,
		"name":               claims.Name,
		"given_name":         claims.GivenName,
		"family_name":        claims.FamilyName,
		"email_verified":     claims.EmailVerified,
		"roles":              claims.RealmAccess.Roles,
		"role":               claims.GetRole(),
		"is_client_token":    claims.IsClientToken(),
		"country":            claims.Country,
		"state":              claims.State,
		"issued_at":          claims.IssuedAt().Unix(),
		"expires_at":         claims.ExpiresAt().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(userInfo)
}

func main() {
	// Configuration - Always require authentication
	config := sseor.ConfigFromEnv()
	config.AuthRequired = true
	config.RequireNamespace = false
	config.OIDCIssuerURL = "https://accounts.piveredu.com/realms/gh-realm"
	config.OIDCClientID = "api"

	// Initialize SSE Manager
	manager, err := sseor.NewManager(config)
	if err != nil {
		log.Fatalf("Failed to create SSE manager: %v", err)
	}
	defer manager.Close()

	// Initialize Redis client for transaction storage
	redisOpt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		log.Fatalf("Failed to parse redis URL: %v", err)
	}

	redisClient := redis.NewClient(redisOpt)
	defer redisClient.Close()

	// Test Redis connection
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	// Initialize services
	transactionService := NewTransactionService(redisClient, manager)
	transactionServer := NewTransactionServer(transactionService, manager)

	// Setup routes
	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()

	// Apply authentication middleware to all API routes
	api.Use(manager.LoggingMiddleware)
	api.Use(manager.AuthMiddleware)

	// Transaction endpoints
	api.HandleFunc("/transactions/deposit", transactionServer.HandleDeposit).Methods("POST")
	api.HandleFunc("/transactions/{transactionId}", transactionServer.HandleGetTransaction).Methods("GET")
	api.HandleFunc("/user/info", transactionServer.HandleUserInfo).Methods("GET")

	// SSE endpoint for specific transaction monitoring (auth handled by manager)
	manager.Route("/api/v1/sse/transactions/{transactionId}", transactionServer.HandleTransactionSSE)

	// CORS middleware for API routes
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Country-Iso2, X-State-Iso2")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	})

	// Start transaction API server
	go func() {
		log.Println("Starting transaction API server on :8080")
		if err := http.ListenAndServe(":8080", router); err != nil {
			log.Fatalf("Transaction API server failed: %v", err)
		}
	}()

	// Start SSE server
	log.Println("Starting SSE server on :9090")
	if err := manager.ListenAndServe(":9090"); err != nil {
		log.Fatalf("SSE server failed: %v", err)
	}
}
