// Example usage of the sseor package
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/shoot3rs/sseor"
)

// TransactionHandler handles transaction-related SSE connections
type TransactionHandler struct {
	manager *sseor.Manager
}

// NewTransactionHandler creates a new transaction handler
func NewTransactionHandler(manager *sseor.Manager) *TransactionHandler {
	return &TransactionHandler{
		manager: manager,
	}
}

// Handle processes incoming SSE connections for transactions
func (th *TransactionHandler) Handle(w http.ResponseWriter, r *http.Request, manager *sseor.Manager) {
	// Extract user ID from query parameters or JWT token
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	// You can also extract transaction_id if you want to subscribe to specific transactions
	transactionID := r.URL.Query().Get("transaction_id")

	log.Printf("New SSE connection for user: %s, transaction: %s", userID, transactionID)

	// Handle the SSE connection
	if err := manager.HandleSSE(w, r, userID); err != nil {
		log.Printf("SSE connection error: %v", err)
	}
}

// Transaction represents a wallet transaction
type Transaction struct {
	ID       string  `json:"id"`
	UserID   string  `json:"user_id"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
	WalletID string  `json:"wallet_id"`
	Created  int64   `json:"created"`
	Updated  int64   `json:"updated"`
}

// TransactionService simulates your transaction processing service
type TransactionService struct {
	sseManager *sseor.Manager
}

// NewTransactionService creates a new transaction service
func NewTransactionService(sseManager *sseor.Manager) *TransactionService {
	return &TransactionService{
		sseManager: sseManager,
	}
}

// ProcessDeposit simulates processing a deposit transaction
func (ts *TransactionService) ProcessDeposit(userID string, amount float64) (*Transaction, error) {
	// Create transaction
	transaction := &Transaction{
		ID:       generateTransactionID(),
		UserID:   userID,
		Amount:   amount,
		Status:   "pending",
		WalletID: "wallet_" + userID,
		Created:  time.Now().Unix(),
		Updated:  time.Now().Unix(),
	}

	// Send initial status update
	event := sseor.Event{
		ID:   transaction.ID,
		Type: "transaction.created",
		Data: map[string]interface{}{
			"transaction": transaction,
			"message":     "Transaction created and is being processed",
		},
	}

	if err := ts.sseManager.Publish(context.Background(), userID, event); err != nil {
		log.Printf("Failed to publish transaction created event: %v", err)
	}

	// Simulate async processing
	go ts.processTransactionAsync(transaction)

	return transaction, nil
}

// processTransactionAsync simulates the background processing of a transaction
func (ts *TransactionService) processTransactionAsync(transaction *Transaction) {
	// Simulate processing steps
	steps := []struct {
		status  string
		message string
		delay   time.Duration
	}{
		{"processing", "Validating transaction details", 2 * time.Second},
		{"validating", "Checking account balance and limits", 3 * time.Second},
		{"executing", "Processing payment with bank", 5 * time.Second},
		{"completed", "Transaction completed successfully", 1 * time.Second},
	}

	for _, step := range steps {
		time.Sleep(step.delay)

		transaction.Status = step.status
		transaction.Updated = time.Now().Unix()

		event := sseor.Event{
			ID:   transaction.ID,
			Type: "transaction.status_updated",
			Data: map[string]interface{}{
				"transaction": transaction,
				"message":     step.message,
				"step":        step.status,
			},
		}

		if err := ts.sseManager.Publish(context.Background(), transaction.UserID, event); err != nil {
			log.Printf("Failed to publish transaction update: %v", err)
		}
	}

	// Send final completion event
	finalEvent := sseor.Event{
		ID:   transaction.ID,
		Type: "transaction.completed",
		Data: map[string]interface{}{
			"transaction": transaction,
			"message":     "Your deposit has been processed successfully!",
			"wallet": map[string]interface{}{
				"id":      transaction.WalletID,
				"balance": 1000.00 + transaction.Amount, // Simulated new balance
			},
		},
	}

	if err := ts.sseManager.Publish(context.Background(), transaction.UserID, finalEvent); err != nil {
		log.Printf("Failed to publish transaction completion: %v", err)
	}
}

// HTTP handlers for the REST API
func (ts *TransactionService) handleDeposit(w http.ResponseWriter, r *http.Request) {
	var request struct {
		UserID string  `json:"user_id"`
		Amount float64 `json:"amount"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	transaction, err := ts.ProcessDeposit(request.UserID, request.Amount)
	if err != nil {
		http.Error(w, "Failed to process deposit", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"transaction": transaction,
		"message":     "Deposit initiated. Subscribe to SSE for real-time updates.",
		"sse_url":     "/api/v1/sse/transactions?user_id=" + request.UserID,
	})
}

func generateTransactionID() string {
	return "txn_" + time.Now().Format("20060102150405") + "_" + randomString(6)
}

func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(result)
}

func main() {
	// Initialize SSE manager
	sse, err := sseor.NewManager("redis://localhost:6379/5")
	if err != nil {
		log.Fatalf("Failed to create SSE manager: %v", err)
	}
	defer sse.Close()

	// Create handlers
	transactionHandler := NewTransactionHandler(sse)
	transactionService := NewTransactionService(sse)

	// Setup SSE routes
	sse.Route("/api/v1/sse/transactions", transactionHandler.Handle)

	// Setup REST API routes (for testing)
	restRouter := mux.NewRouter()
	restRouter.HandleFunc("/api/v1/transactions/deposit", transactionService.handleDeposit).Methods("POST")
	restRouter.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "healthy",
			"connections": sse.GetTotalConnections(),
		})
	}).Methods("GET")

	// Start REST API server in a goroutine
	go func() {
		log.Println("🔄 [API]: REST API server started on port 8080")
		if err := http.ListenAndServe(":8080", restRouter); err != nil {
			log.Printf("REST API server error: %v", err)
		}
	}()

	// Start SSE server
	log.Println("🚀 [SSEOR]: Server started on port 9090")
	if err := sse.ListenAndServe(":9090"); err != nil {
		log.Println("👮 [SSEOR]: failed to start server:", err)
		os.Exit(1)
	}
}
