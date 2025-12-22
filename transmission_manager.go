package main

import (
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
// It effectively manages two distinct constraints:
// 1. Regulatory Duty Cycle (via CUL Credits): Ensuring we don't exceed the 1% airtime limit.
// 2. Reliable Delivery (via Handshaking): Ensuring commands are acknowledged by the device (ACK).
type TransmissionManager struct {
	// Duty Cycle State
	CurrentCredits int
	QueueLength    int // CUL's internal send queue (0 = empty, ready to TX)
	MinCredits     int
	Timeout        time.Duration

	mutex          sync.RWMutex
	queue          chan QueuedCommand
	creditResponse chan struct{} // Signal when credit response received

	// Pending TX tracking (per device)
	pendingTX    map[string]bool
	pendingMutex sync.RWMutex

	// LOVF detection
	lovfChan chan struct{}

	// ACK notification channels (per device)
	ackChans map[string]chan struct{}
	ackMutex sync.Mutex
}

var txMgr *TransmissionManager

// creditResponseRegex matches credit response format: "yy xxx" (e.g., "00 900", "01 850")
var creditResponseRegex = regexp.MustCompile(`^\d+\s+\d+$`)

func initTransmissionManager() {
	// Parse Timeout
	timeout, err := time.ParseDuration(config.CommandTimeout)
	if err != nil {
		log.Warnf("Invalid CommandTimeout '%s', defaulting to 1m", config.CommandTimeout)
		timeout = 1 * time.Minute
	}

	minCredits := config.DutyCycleMinCredits
	if minCredits <= 0 {
		minCredits = 100 // Safe default
	}

	txMgr = &TransmissionManager{
		CurrentCredits: 0, // Start pessimistic - will query before first TX
		QueueLength:    0,
		MinCredits:     minCredits,
		Timeout:        timeout,
		queue:          make(chan QueuedCommand, 100),
		creditResponse: make(chan struct{}, 1),
		pendingTX:      make(map[string]bool),
		lovfChan:       make(chan struct{}, 1),
		ackChans:       make(map[string]chan struct{}),
	}

	log.Infof("TransmissionManager initialized. MinCredits: %d, Timeout: %s", minCredits, timeout)

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

// dispatcherLoop consumes queued commands and dispatches them.
// ACK waiting is handled in separate goroutines to allow parallel TX to different devices.
func (t *TransmissionManager) dispatcherLoop() {
	for cmd := range t.queue {
		// 1. Check Timeout before even querying
		if time.Since(cmd.CreatedAt) > t.Timeout {
			log.Warnf("Command to %s timed out (waited %s) due to duty cycle limits. Restoring state.", cmd.DeviceID, time.Since(cmd.CreatedAt))
			t.restoreState(cmd.DeviceID)
			continue
		}

		// 2. Check if device already has a pending TX - reject if busy
		if t.IsPendingTX(cmd.DeviceID) {
			log.Warnf("TxMgr: Device %s busy (awaiting ACK). Rejecting command '%s'. Restoring state.", cmd.DeviceID, cmd.Description)
			t.restoreState(cmd.DeviceID)
			continue
		}

		// 3. Query credits before TX
		log.Debug("TxMgr: Querying credits before TX (X)")

		// Clear any stale signal
		select {
		case <-t.creditResponse:
		default:
		}

		// Send X command to query credits/queue
		select {
		case serialWrite <- "X":
		default:
			log.Warn("TxMgr: Could not send credit query (channel full)")
			t.restoreState(cmd.DeviceID)
			continue
		}

		// 4. Wait for credit response with timeout
		queryTimeout := 3 * time.Second
		select {
		case <-t.creditResponse:
			// Response received, continue to check
		case <-time.After(queryTimeout):
			log.Warnf("TxMgr: Credit query timeout after %s. Restoring state for %s.", queryTimeout, cmd.DeviceID)
			t.restoreState(cmd.DeviceID)
			continue
		}

		// 5. Check conditions: queue == 0 AND credits >= minCredits
		t.mutex.RLock()
		credits := t.CurrentCredits
		queueLen := t.QueueLength
		t.mutex.RUnlock()

		if queueLen != 0 {
			log.Warnf("TxMgr: CUL queue busy (queue=%d). Cannot TX to %s. Restoring state.", queueLen, cmd.DeviceID)
			t.restoreState(cmd.DeviceID)
			continue
		}

		if credits < t.MinCredits {
			log.Warnf("TxMgr: Insufficient credits (%d < %d). Cannot TX to %s. Restoring state.", credits, t.MinCredits, cmd.DeviceID)
			t.restoreState(cmd.DeviceID)
			continue
		}

		// 6. Clear LOVF channel before TX
		select {
		case <-t.lovfChan:
		default:
		}

		// 7. All conditions met - mark device as busy and dispatch
		t.SetPendingTX(cmd.DeviceID)

		// Create ACK channel BEFORE sending to prevent race condition where ACK arrives
		// before we start waiting. The channel is buffered (size 1) so it will hold the signal.
		t.getOrCreateAckChan(cmd.DeviceID)

		select {
		case serialWrite <- cmd.Payload:
			log.Infof("TxMgr: TX dispatched to %s: '%s' (Credits: %d, Queue: %d). Waiting for ACK...", cmd.DeviceID, cmd.Description, credits, queueLen)
			// Spawn goroutine to wait for ACK - dispatcher continues immediately
			go t.handleAckWait(cmd)
		default:
			log.Errorf("TxMgr: Serial write channel full! Dropping command for %s", cmd.DeviceID)
			t.ClearPendingTX(cmd.DeviceID)
			continue
		}
	}
}

// handleAckWait waits for ACK in a separate goroutine, allowing the dispatcher to continue.
func (t *TransmissionManager) handleAckWait(cmd QueuedCommand) {
	defer t.ClearPendingTX(cmd.DeviceID)

	ackReceived := t.waitForAckOrError(cmd.DeviceID)

	if !ackReceived {
		log.Warnf("TxMgr: No ACK from %s within timeout for command '%s'. Restoring state.", cmd.DeviceID, cmd.Description)
		t.restoreState(cmd.DeviceID)
	} else {
		log.Infof("TxMgr: ACK received from %s. Command confirmed: %s", cmd.DeviceID, cmd.Description)
	}
}

// IsPendingTX checks if a device is currently awaiting ACK
func (t *TransmissionManager) IsPendingTX(deviceID string) bool {
	t.pendingMutex.RLock()
	defer t.pendingMutex.RUnlock()
	return t.pendingTX[deviceID]
}

// SetPendingTX marks a device as awaiting ACK
func (t *TransmissionManager) SetPendingTX(deviceID string) {
	t.pendingMutex.Lock()
	defer t.pendingMutex.Unlock()
	t.pendingTX[deviceID] = true
}

// ClearPendingTX marks a device as no longer awaiting ACK
func (t *TransmissionManager) ClearPendingTX(deviceID string) {
	t.pendingMutex.Lock()
	defer t.pendingMutex.Unlock()
	delete(t.pendingTX, deviceID)
}

// SignalLOVF signals that a LOVF (Limit Of Voice Full) response was received
func (t *TransmissionManager) SignalLOVF() {
	select {
	case t.lovfChan <- struct{}{}:
	default:
		// Channel already has a signal, don't block
	}
}

// NotifyAck notifies that an ACK was received from a specific device
func (t *TransmissionManager) NotifyAck(deviceID string) {
	t.ackMutex.Lock()
	ch, exists := t.ackChans[deviceID]
	t.ackMutex.Unlock()

	if exists {
		select {
		case ch <- struct{}{}:
		default:
			// Channel already has a signal
		}
	}
}

// waitForAckOrError waits for the specific device to acknowledge the command.
// It returns true if ACK received, false if timed out or LOVF occurred.
func (t *TransmissionManager) waitForAckOrError(deviceID string) bool {
	ackChan := t.getOrCreateAckChan(deviceID)
	defer t.removeAckChan(deviceID)

	select {
	case <-ackChan:
		return true
	case <-t.lovfChan:
		log.Warn("TxMgr: LOVF received during TX wait")
		return false
	case <-time.After(t.Timeout):
		return false
	}
}

// getOrCreateAckChan gets or creates an ACK channel for a device
func (t *TransmissionManager) getOrCreateAckChan(deviceID string) chan struct{} {
	t.ackMutex.Lock()
	defer t.ackMutex.Unlock()

	if ch, exists := t.ackChans[deviceID]; exists {
		return ch
	}

	ch := make(chan struct{}, 1)
	t.ackChans[deviceID] = ch
	return ch
}

// removeAckChan removes the ACK channel for a device
func (t *TransmissionManager) removeAckChan(deviceID string) {
	t.ackMutex.Lock()
	defer t.ackMutex.Unlock()
	delete(t.ackChans, deviceID)
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
	// We need to modify existing state slightly or just publish?
	// If we just publish, HA receives the "old" values again and updates UI.
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
	parts := regexp.MustCompile(`\s+`).Split(text, -1)
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
