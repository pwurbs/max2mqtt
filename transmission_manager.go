package main

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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

	// LOVF detection
	lovfChan chan struct{}
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
		log.Warnf("Invalid CommandTimeout '%s', defaulting to 1m", config.CommandTimeout)
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
		lovfChan:       make(chan struct{}, 1),
	}

	fmt.Printf("time=\"%s\" level=info msg=\"TransmissionManager initialized. MinCredits: %d, MaxQueue: %d, Timeout: %s\"\n",
		time.Now().Format(time.RFC3339), config.DutyCycleMinCredits, config.MaxCulQueue, timeout)

	go txMgr.dispatcherLoop()
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
	log.Debugf("TxMgr: Updated - Credits: %d, Queue: %d", credits, queueLen)
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
		log.Debugf("TxMgr: Enqueued command for %s: %s", devicedID, description)
	default:
		log.Warnf("TxMgr: Queue full! Dropping command for %s", devicedID)
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
		log.Warnf("Command to %s timed out (waited %s) due to duty cycle limits. Restoring state.", cmd.DeviceID, time.Since(cmd.CreatedAt))
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
	if queueLen >= t.MaxQueue {
		log.Warnf("TxMgr: CUL queue too busy (queue=%d >= limit=%d). Cannot TX to %s. Restoring state.", queueLen, t.MaxQueue, cmd.DeviceID)
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	if credits < t.MinCredits {
		log.Warnf("TxMgr: Insufficient credits (%d < %d). Cannot TX to %s. Restoring state.", credits, t.MinCredits, cmd.DeviceID)
		t.maybeRestoreState(cmd.DeviceID)
		return
	}

	// 4. Clear LOVF channel before TX and dispatch
	drainChannel(t.lovfChan)
	t.dispatchToSerial(cmd, credits, queueLen)
}

// queryCreditStatus sends a credit query and waits for the response.
// Returns (credits, queueLen, success).
func (t *TransmissionManager) queryCreditStatus() (credits int, queueLen int, ok bool) {
	log.Debug("TxMgr: Querying credits before TX (X)")

	// Clear any stale signal
	drainChannel(t.creditResponse)

	// Send X command to query credits/queue
	select {
	case serialWrite <- "X":
	default:
		log.Warn("TxMgr: Could not send credit query (channel full)")
		return 0, 0, false
	}

	// Wait for credit response with timeout
	const queryTimeout = 3 * time.Second
	select {
	case <-t.creditResponse:
		// Response received
	case <-time.After(queryTimeout):
		log.Warnf("TxMgr: Credit query timeout after %s", queryTimeout)
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
		log.Infof("TxMgr: TX dispatched to %s: '%s' (Credits: %d, Queue: %d)", cmd.DeviceID, cmd.Description, credits, queueLen)
	default:
		log.Errorf("TxMgr: Serial write channel full! Dropping command for %s", cmd.DeviceID)
	}
}

// SignalLOVF signals that a LOVF (Limit Of Voice Full) response was received
func (t *TransmissionManager) SignalLOVF() {
	select {
	case t.lovfChan <- struct{}{}:
	default:
		// Channel already has a signal, don't block
	}
}

func (t *TransmissionManager) restoreState(deviceID string) {
	stateMutex.RLock()
	existing, ok := stateCache[deviceID]
	stateMutex.RUnlock()

	if !ok {
		log.Warnf("TxMgr: No state known for %s to restore.", deviceID)
		return
	}

	// Re-publish the existing clean state to MQTT
	// This overwrites any optimistic UI changes in HA
	log.Infof("TxMgr: Restoring state for %s: %s", deviceID, existing)
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
