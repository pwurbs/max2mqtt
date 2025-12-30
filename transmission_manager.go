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
// It manages the Regulatory Duty Cycle (via CUL Credits), ensuring we don't exceed
// the 1% airtime limit. Commands are dispatched when credits are available.
//
// Note: We do NOT wait for ACKs. The MAX! system is self-correcting—the next RX
// packet from a device will contain its actual state, automatically fixing any
// discrepancy if a TX was lost.
type TransmissionManager struct {
	// Duty Cycle State
	CurrentCredits int
	QueueLength    int // CUL's internal send queue (0 = empty, ready to TX)
	MinCredits     int
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

func initTransmissionManager() {
	// Parse Timeout
	timeout, err := time.ParseDuration(config.CommandTimeout)
	if err != nil {
		log.Warnf("Invalid CommandTimeout '%s', defaulting to 2m", config.CommandTimeout)
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
		lovfChan:       make(chan struct{}, 1),
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
// Broadcasts (deviceID=000000) skip LOVF handling.
// We do NOT wait for ACKs—the next RX from the device will self-correct state if needed.
func (t *TransmissionManager) dispatcherLoop() {
	for cmd := range t.queue {
		// Handle broadcast messages (000000) - no state restore needed
		isBroadcast := cmd.DeviceID == "000000"

		// 1. Check Timeout before even querying
		if time.Since(cmd.CreatedAt) > t.Timeout {
			log.Warnf("Command to %s timed out (waited %s) due to duty cycle limits. Restoring state.", cmd.DeviceID, time.Since(cmd.CreatedAt))
			if !isBroadcast {
				t.restoreState(cmd.DeviceID)
			}
			continue
		}

		// 2. Query credits before TX
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
			if !isBroadcast {
				t.restoreState(cmd.DeviceID)
			}
			continue
		}

		// 3. Wait for credit response with timeout
		queryTimeout := 3 * time.Second
		select {
		case <-t.creditResponse:
			// Response received, continue to check
		case <-time.After(queryTimeout):
			log.Warnf("TxMgr: Credit query timeout after %s. Restoring state for %s.", queryTimeout, cmd.DeviceID)
			if !isBroadcast {
				t.restoreState(cmd.DeviceID)
			}
			continue
		}

		// 4. Check conditions: queue == 0 AND credits >= minCredits
		t.mutex.RLock()
		credits := t.CurrentCredits
		queueLen := t.QueueLength
		t.mutex.RUnlock()

		if queueLen != 0 {
			log.Warnf("TxMgr: CUL queue busy (queue=%d). Cannot TX to %s. Restoring state.", queueLen, cmd.DeviceID)
			if !isBroadcast {
				t.restoreState(cmd.DeviceID)
			}
			continue
		}

		if credits < t.MinCredits {
			log.Warnf("TxMgr: Insufficient credits (%d < %d). Cannot TX to %s. Restoring state.", credits, t.MinCredits, cmd.DeviceID)
			if !isBroadcast {
				t.restoreState(cmd.DeviceID)
			}
			continue
		}

		// 5. Clear LOVF channel before TX
		select {
		case <-t.lovfChan:
		default:
		}

		// 6. Dispatch the command
		select {
		case serialWrite <- cmd.Payload:
			log.Infof("TxMgr: TX dispatched to %s: '%s' (Credits: %d, Queue: %d)", cmd.DeviceID, cmd.Description, credits, queueLen)
		default:
			log.Errorf("TxMgr: Serial write channel full! Dropping command for %s", cmd.DeviceID)
		}
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
