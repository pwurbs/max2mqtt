package main

import (
	"regexp"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type QueuedCommand struct {
	Payload   string
	DeviceID  string
	CreatedAt time.Time
}

type DutyCycleManager struct {
	CurrentCredits int
	QueueLength    int // CUL's internal send queue (0 = empty, ready to TX)
	MinCredits     int
	Timeout        time.Duration

	mutex          sync.RWMutex
	queue          chan QueuedCommand
	creditResponse chan struct{} // Signal when credit response received
}

var dutyMgr *DutyCycleManager

// creditResponseRegex matches credit response format: "yy xxx" (e.g., "00 900", "01 850")
var creditResponseRegex = regexp.MustCompile(`^\d+\s+\d+$`)

func initDutyCycleManager() {
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

	dutyMgr = &DutyCycleManager{
		CurrentCredits: 0, // Start pessimistic - will query before first TX
		QueueLength:    0,
		MinCredits:     minCredits,
		Timeout:        timeout,
		queue:          make(chan QueuedCommand, 100),
		creditResponse: make(chan struct{}, 1),
	}

	log.Infof("DutyCycleManager initialized. MinCredits: %d, Timeout: %s", minCredits, timeout)

	go dutyMgr.dispatcherLoop()
}

func (d *DutyCycleManager) Start() {
	// Optional: Any explicit start logic
}

// UpdateCredits updates both credits and queue length from CUL response
func (d *DutyCycleManager) UpdateCredits(credits, queueLen int) {
	d.mutex.Lock()
	d.CurrentCredits = credits
	d.QueueLength = queueLen
	d.mutex.Unlock()
	log.Debugf("DutyCycle: Updated - Credits: %d, Queue: %d", credits, queueLen)
}

// SignalCreditResponse signals that a credit response was received
func (d *DutyCycleManager) SignalCreditResponse() {
	select {
	case d.creditResponse <- struct{}{}:
	default:
		// Channel already has a signal, don't block
	}
}

func (d *DutyCycleManager) Enqueue(devicedID, payload string) {
	cmd := QueuedCommand{
		Payload:   payload,
		DeviceID:  devicedID,
		CreatedAt: time.Now(),
	}

	select {
	case d.queue <- cmd:
		log.Debugf("DutyCycle: Enqueued command for %s", devicedID)
	default:
		log.Warnf("DutyCycle: Queue full! Dropping command for %s", devicedID)
	}
}

func (d *DutyCycleManager) dispatcherLoop() {
	for cmd := range d.queue {
		// 1. Check Timeout before even querying
		if time.Since(cmd.CreatedAt) > d.Timeout {
			log.Warnf("Command to %s timed out (waited %s) due to duty cycle limits. Restoring state.", cmd.DeviceID, time.Since(cmd.CreatedAt))
			d.restoreState(cmd.DeviceID)
			continue
		}

		// 2. Query credits before TX
		log.Debug("DutyCycle: Querying credits before TX (X)")

		// Clear any stale signal
		select {
		case <-d.creditResponse:
		default:
		}

		// Send X command to query credits/queue
		select {
		case serialWrite <- "X":
		default:
			log.Warn("DutyCycle: Could not send credit query (channel full)")
			d.restoreState(cmd.DeviceID)
			continue
		}

		// 3. Wait for credit response with timeout
		queryTimeout := 3 * time.Second
		select {
		case <-d.creditResponse:
			// Response received, continue to check
		case <-time.After(queryTimeout):
			log.Warnf("DutyCycle: Credit query timeout after %s. Restoring state for %s.", queryTimeout, cmd.DeviceID)
			d.restoreState(cmd.DeviceID)
			continue
		}

		// 4. Check conditions: queue == 0 AND credits >= minCredits
		d.mutex.RLock()
		credits := d.CurrentCredits
		queueLen := d.QueueLength
		d.mutex.RUnlock()

		if queueLen != 0 {
			log.Warnf("DutyCycle: CUL queue busy (queue=%d). Cannot TX to %s. Restoring state.", queueLen, cmd.DeviceID)
			d.restoreState(cmd.DeviceID)
			continue
		}

		if credits < d.MinCredits {
			log.Warnf("DutyCycle: Insufficient credits (%d < %d). Cannot TX to %s. Restoring state.", credits, d.MinCredits, cmd.DeviceID)
			d.restoreState(cmd.DeviceID)
			continue
		}

		// 5. All conditions met - dispatch the command
		select {
		case serialWrite <- cmd.Payload:
			log.Infof("DutyCycle: TX dispatched to %s (Credits: %d, Queue: %d)", cmd.DeviceID, credits, queueLen)
		default:
			log.Errorf("DutyCycle: Serial write channel full! Dropping command for %s", cmd.DeviceID)
		}
	}
}

func (d *DutyCycleManager) restoreState(deviceID string) {
	stateMutex.RLock()
	existing, ok := stateCache[deviceID]
	stateMutex.RUnlock()

	if !ok {
		log.Warnf("DutyCycle: No state known for %s to restore.", deviceID)
		return
	}

	// Re-publish the existing clean state to MQTT
	// This overwrites any optimistic UI changes in HA
	log.Infof("DutyCycle: Restoring state for %s", deviceID)
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
	n, err := strconv.Atoi("")
	_ = n // unused
	_, err = sscanf(text, &q, &c)
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
