package main

import (
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
	MinCredits     int
	Timeout        time.Duration

	mutex sync.RWMutex
	queue chan QueuedCommand
}

var dutyMgr *DutyCycleManager

func initDutyCycleManager() {
	// Parse Timeout
	timeout, err := time.ParseDuration(config.CommandTimeout)
	if err != nil {
		log.Warnf("Invalid CommandTimeout '%s', defaulting to 5m", config.CommandTimeout)
		timeout = 5 * time.Minute
	}

	minCredits := config.DutyCycleMinCredits
	if minCredits <= 0 {
		minCredits = 100 // Safe default
	}

	dutyMgr = &DutyCycleManager{
		CurrentCredits: 200, // Initial optimistic guess to allow startup commands before first 'X' response
		MinCredits:     minCredits,
		Timeout:        timeout,
		queue:          make(chan QueuedCommand, 100),
	}

	log.Infof("DutyCycleManager initialized. MinCredits: %d, Timeout: %s", minCredits, timeout)

	go dutyMgr.dispatcherLoop()
	go dutyMgr.creditRefresher()
}

func (d *DutyCycleManager) Start() {
	// Optional: Any explicit start logic
}

func (d *DutyCycleManager) UpdateCredits(credits int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.CurrentCredits = credits
	log.Debugf("DutyCycle: Credits updated to %d", credits)
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
		// 1. Check Timeout
		if time.Since(cmd.CreatedAt) > d.Timeout {
			log.Warnf("Command to %s timed out (waited %s) due to duty cycle limits. Restoring state.", cmd.DeviceID, time.Since(cmd.CreatedAt))
			d.restoreState(cmd.DeviceID)
			continue
		}

		// 2. Check Credits loop
		var loggedLowCredits bool
		for {
			d.mutex.RLock()
			credits := d.CurrentCredits
			d.mutex.RUnlock()

			if credits >= d.MinCredits {
				break
			}

			// Not enough credits
			if !loggedLowCredits {
				log.Warnf("DutyCycle: Low credits (%d < %d). Pausing for 1s...", credits, d.MinCredits)
				loggedLowCredits = true
			} else {
				log.Debugf("DutyCycle: Still low credits (%d < %d). Waiting...", credits, d.MinCredits)
			}

			// Check timeout again while waiting
			if time.Since(cmd.CreatedAt) > d.Timeout {
				log.Warnf("Command to %s timed out while waiting for credits. Restoring state.", cmd.DeviceID)
				d.restoreState(cmd.DeviceID)
				goto NextCommand // break out of credit loop and process next
			}

			time.Sleep(1 * time.Second)
		}

		// 3. Send
		// Subtract estimated cost immediately (optional, but good for fast sends)
		// Standard packet ~10 credits? Let's just rely on periodic updates or wait.
		select {
		case serialWrite <- cmd.Payload:
			log.Debugf("DutyCycle: Dispatched command to %s (Credits: %d)", cmd.DeviceID, d.CurrentCredits)
		default:
			log.Errorf("DutyCycle: Serial write channel full! Dropping command for %s", cmd.DeviceID)
		}

	NextCommand:
	}
}

func (d *DutyCycleManager) creditRefresher() {
	ticker := time.NewTicker(30 * time.Second) // Refresh often enough
	defer ticker.Stop()

	for range ticker.C {
		// Send 'X' command to query credits
		// We send directly to serialWrite to bypass the queue?
		// Or we enqueue it with high priority?
		// Simpler: Just write to serialWrite if possible, it's short.
		// But if serialWrite is full/blocked, we might have issues.
		// Given 'X' is critical for the queue to unblock, we should force it.

		// Note: IF the queue is blocked waiting for credits, and we put X in the queue, it will never send.
		// So 'X' MUST bypass the credit check.
		// Writing directly to `serialWrite` works because `dispatcherLoop` only writes to `serialWrite` when it has credits,
		// but `serialWrite` is the channel being read by `serialReaderLoop`.
		// wait.. `serialReaderLoop` reads from `serialWrite`.
		// So if `dispatcherLoop` is blocked, `serialWrite` might be empty.
		// So we can write 'X' to `serialWrite` directly.

		log.Debug("DutyCycle: Requesting credit update (X)")
		select {
		case serialWrite <- "X":
		default:
			log.Warn("DutyCycle: Could not send credit query (channel full)")
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
