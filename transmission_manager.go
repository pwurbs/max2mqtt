package main

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// QueuedCommand represents a TX command waiting in the local queue
type QueuedCommand struct {
	Payload     string
	Description string
	DeviceID    string
	CreatedAt   time.Time
}

// TransmissionManager handles the complexity of sending commands to MAX! devices.
// It manages the Regulatory Duty Cycle (via CUL Credits), ensuring we don't exceed
// the 1% airtime limit. Commands are dispatched when credits are available.
//
// Note: We do NOT wait for ACKs. The MAX! system is self-correcting—the next RX
// packet from a device will contain its actual state, automatically fixing any
// discrepancy if a TX was lost.
type TransmissionManager struct {
	// Duty Cycle State
	CurrentCredits int
	QueueLength    int // Current buffering level reported by CUL (Real-time hardware state)
	MinCredits     int
	MaxQueue       int // Configured limit for concurrent commands (Software throttle)
	Timeout        time.Duration

	mutex          sync.RWMutex
	queue          chan QueuedCommand
	creditResponse chan struct{} // Signal when credit response received

}

var txMgr *TransmissionManager

// creditResponseRegex matches credit response format: "yy xxx" (e.g., "00 900", "01 850")
var creditResponseRegex = regexp.MustCompile(`^\d+\s+\d+$`)

// whitespaceRegex is precompiled for performance in sscanf
var whitespaceRegex = regexp.MustCompile(`\s+`)

func initTransmissionManager() {
	// Parse Timeout
	timeout, err := time.ParseDuration(config.CommandTimeout)
	if err != nil {
		slog.Warn("Invalid CommandTimeout, defaulting to 1m", "value", config.CommandTimeout)
		timeout = 1 * time.Minute
	}

	txMgr = &TransmissionManager{
		CurrentCredits: 0, // Start pessimistic - will query before first TX
		QueueLength:    0,
		MinCredits:     config.DutyCycleMinCredits,
		MaxQueue:       config.MaxCulQueue,
		Timeout:        timeout,
		queue:          make(chan QueuedCommand, 100),
		creditResponse: make(chan struct{}, 1),
	}

	fmt.Printf("time=\"%s\" level=info msg=\"TransmissionManager initialized. MinCredits: %d, MaxQueue: %d, Timeout: %s\"\n",
		time.Now().Format(time.RFC3339), config.DutyCycleMinCredits, config.MaxCulQueue, timeout)

	go txMgr.dispatcherLoop()
	initVerificationManager()
}

func (t *TransmissionManager) Start() {
	// Optional: Any explicit start logic
}

// UpdateCredits updates both credits and queue length from CUL response
func (t *TransmissionManager) UpdateCredits(credits, queueLen int) {
	t.mutex.Lock()
	t.CurrentCredits = credits
	t.QueueLength = queueLen
	t.mutex.Unlock()
	slog.Debug("TxMgr: Updated", "credits", credits, "queue", queueLen)
}

// SignalCreditResponse notifies the dispatcher that a credit report "yy xxx" has been received.
// This unblocks the dispatcher if it was waiting for a credit check.
func (t *TransmissionManager) SignalCreditResponse() {
	select {
	case t.creditResponse <- struct{}{}:
	default:
		// Channel already has a signal, don't block
	}
}

// Enqueue adds a command to the transmission queue.
// It is safe to call from multiple goroutines (e.g., MQTT handlers).
func (t *TransmissionManager) Enqueue(devicedID, payload, description string) {
	cmd := QueuedCommand{
		Payload:     payload,
		Description: description,
		DeviceID:    devicedID,
		CreatedAt:   time.Now(),
	}

	select {

	case t.queue <- cmd:
		slog.Debug("TxMgr: Enqueued command", "device", devicedID, "desc", description)
	default:
		slog.Warn("TxMgr: Queue full! Dropping command", "device", devicedID)
	}
}

// maybeRestoreState restores state for non-broadcast devices.
// Returns early if deviceID is the broadcast address.
func (t *TransmissionManager) maybeRestoreState(deviceID string) {
	if deviceID == "000000" {
		return
	}
	t.restoreState(deviceID)
}

// drainChannel clears any pending signal from a channel without blocking.
func drainChannel(ch <-chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

// dispatcherLoop consumes queued commands and dispatches them.
// Broadcasts (deviceID=000000) skip state restoration.
// We do NOT wait for ACKs—the next RX from the device will self-correct state if needed.
func (t *TransmissionManager) dispatcherLoop() {
	for cmd := range t.queue {
		t.processCommand(cmd)
	}
}

// processCommand handles a single command dispatch attempt.
func (t *TransmissionManager) processCommand(cmd QueuedCommand) {
	// 1. Check Timeout before even querying
	if time.Since(cmd.CreatedAt) > t.Timeout {
		slog.Warn("Command timed out due to duty cycle limits. Restoring state.", "device", cmd.DeviceID, "wait", time.Since(cmd.CreatedAt))
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	// 2. Query credits and wait for response
	credits, queueLen, ok := t.queryCreditStatus()
	if !ok {
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	// 3. Check conditions: usage of CUL queue allowed up to MaxQueue
	// 3. Check conditions: usage of CUL queue allowed up to MaxQueue
	if queueLen >= t.MaxQueue {
		slog.Warn("TxMgr: CUL queue too busy. Restoring state.", "queue", queueLen, "limit", t.MaxQueue, "device", cmd.DeviceID)
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	if credits < t.MinCredits {
		slog.Warn("TxMgr: Insufficient credits. Restoring state.", "credits", credits, "min", t.MinCredits, "device", cmd.DeviceID)
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	t.dispatchToSerial(cmd, credits, queueLen)
}

// queryCreditStatus sends a credit query and waits for the response.
// Returns (credits, queueLen, success).
func (t *TransmissionManager) queryCreditStatus() (credits int, queueLen int, ok bool) {
	slog.Debug("TxMgr: Querying credits before TX (X)")

	// Clear any stale signal
	drainChannel(t.creditResponse)

	// Send X command to query credits/queue
	select {
	case serialWrite <- "X":
	default:
		slog.Warn("TxMgr: Could not send credit query (channel full)")
		return 0, 0, false
	}

	// Wait for credit response with timeout
	const queryTimeout = 3 * time.Second
	select {
	case <-t.creditResponse:
		// Response received
	case <-time.After(queryTimeout):
		slog.Warn("TxMgr: Credit query timeout", "timeout", queryTimeout)
		return 0, 0, false
	}

	// Read current state
	t.mutex.RLock()
	credits = t.CurrentCredits
	queueLen = t.QueueLength
	t.mutex.RUnlock()

	return credits, queueLen, true
}

// dispatchToSerial sends the command payload to the serial channel.
func (t *TransmissionManager) dispatchToSerial(cmd QueuedCommand, credits, queueLen int) {
	select {

	case serialWrite <- cmd.Payload:
		slog.Info("TxMgr: TX dispatched", "device", cmd.DeviceID, "desc", cmd.Description, "credits", credits, "queue", queueLen)
	default:
		slog.Error("TxMgr: Serial write channel full! Dropping command", "device", cmd.DeviceID)
	}
}

func (t *TransmissionManager) restoreState(deviceID string) {
	stateMutex.RLock()
	existing, ok := stateCache[deviceID]
	stateMutex.RUnlock()

	if !ok {
		slog.Warn("TxMgr: No state known to restore.", "device", deviceID)
		return
	}

	// Re-publish the existing clean state to MQTT
	// This overwrites any optimistic UI changes in HA
	slog.Info("TxMgr: Restoring state", "device", deviceID, "state", existing)
	publishState(deviceID, existing)
}

// ParseCreditResponse checks if a line matches the credit response format
// and returns (queueLen, credits, matched)
func ParseCreditResponse(text string) (queueLen int, credits int, matched bool) {
	if !creditResponseRegex.MatchString(text) {
		return 0, 0, false
	}

	// Parse "yy xxx" format
	var q, c int
	_, err := sscanf(text, &q, &c)
	if err != nil {
		return 0, 0, false
	}

	return q, c, true
}

// sscanf is a helper to parse two integers from a space-separated string
func sscanf(text string, q *int, c *int) (int, error) {
	parts := whitespaceRegex.Split(text, -1)
	if len(parts) < 2 {
		return 0, nil
	}

	qVal, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	*q = qVal

	cVal, err := strconv.Atoi(parts[1])
	if err != nil {
		return 1, err
	}
	*c = cVal

	return 2, nil
}

// -----------------------------------------------------------------------------------
// Verification Manager Logic
// -----------------------------------------------------------------------------------

// VerificationEntry tracks a pending verification
type VerificationEntry struct {
	TargetTemp float64
	Timer      *time.Timer
	ResendFunc func()
}

// VerificationManager manages pending command verifications
type VerificationManager struct {
	mutex         sync.Mutex
	verifications map[string]*VerificationEntry // Key: DeviceID
	timeout       time.Duration
}

var verificationMgr *VerificationManager

// initVerificationManager initializes the global verification manager
func initVerificationManager() {
	timeoutDuration, err := time.ParseDuration(config.TxCheckPeriod)
	if err != nil {
		slog.Warn("Invalid TxCheckPeriod, defaulting to 1m", "value", config.TxCheckPeriod)
		timeoutDuration = 1 * time.Minute
	}

	verificationMgr = &VerificationManager{
		verifications: make(map[string]*VerificationEntry),
		timeout:       timeoutDuration,
	}
	slog.Info("VerificationManager initialized", "period", timeoutDuration)
}

// AddVerification starts tracking a verification for a device
// resendFunc is a closure that will be executed if verification fails
func (vm *VerificationManager) AddVerification(deviceID string, targetTemp float64, resendFunc func()) {
	vm.mutex.Lock()
	defer vm.mutex.Unlock()

	// Stop existing timer if any (last write wins)
	if entry, exists := vm.verifications[deviceID]; exists {
		entry.Timer.Stop()
	}

	slog.Info("Verification started", "device", deviceID, "target", targetTemp, "timeout", vm.timeout)

	// Create new entry with timer
	timer := time.AfterFunc(vm.timeout, func() {
		vm.handleTimeout(deviceID, targetTemp)
	})

	vm.verifications[deviceID] = &VerificationEntry{
		TargetTemp: targetTemp,
		Timer:      timer,
		ResendFunc: resendFunc,
	}
}

// CheckVerification checks if the received actualTemp matches the pending verification
func (vm *VerificationManager) CheckVerification(deviceID string, actualTemp float64) {
	vm.mutex.Lock()
	defer vm.mutex.Unlock()

	entry, exists := vm.verifications[deviceID]
	if !exists {
		return
	}

	if actualTemp == entry.TargetTemp {
		slog.Info("Verification successful", "device", deviceID, "temp", actualTemp)
		entry.Timer.Stop()
		delete(vm.verifications, deviceID)
	} else {
		// Mismatch - we just wait for timeout OR subsequent update
		slog.Debug("Verification mismatch. Waiting...", "device", deviceID, "wanted", entry.TargetTemp, "got", actualTemp)
	}
}

// handleTimeout is called when the verification timer expires
func (vm *VerificationManager) handleTimeout(deviceID string, targetTemp float64) {
	vm.mutex.Lock()
	// Check if entry still exists and matches expected temp (handle race conditions)
	entry, exists := vm.verifications[deviceID]
	if !exists || entry.TargetTemp != targetTemp {
		vm.mutex.Unlock()
		return
	}
	// Remove the failed verification
	delete(vm.verifications, deviceID)
	// Extract resend func to call outside lock
	resendFunc := entry.ResendFunc
	vm.mutex.Unlock()

	slog.Warn("Verification failed: Did not receive confirmation. Resending...", "device", deviceID, "expected", targetTemp)

	// Execute resend
	if resendFunc != nil {
		resendFunc()
	}
}
