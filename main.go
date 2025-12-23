package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"go.bug.st/serial"
)

// Config holds the application configuration
type Config struct {
	SerialPort string `json:"serial_port"`
	BaudRate   int    `json:"baud_rate"`
	MQTTBroker string `json:"mqtt_broker"`
	MQTTUser   string `json:"mqtt_user"`
	MQTTPass   string `json:"mqtt_pass"`
	LogLevel   string `json:"log_level"`
	GatewayID  string `json:"gateway_id"`

	// Duty Cycle Config
	DutyCycleMinCredits int    `json:"duty_cycle_min_credits"`
	CommandTimeout      string `json:"command_timeout"`
}

// Global state
var (
	config       Config
	mqttClient   mqtt.Client
	serialWrite  chan string
	pairingUntil time.Time

	// Message Counter
	msgCounter      int
	msgCounterMutex sync.Mutex

	// Device State Cache (safe for concurrent access)
	stateCache = make(map[string]*MaxDeviceData)
	stateMutex sync.RWMutex

	// Device Config Cache - stores configuration values for each device
	// Used to preserve other config values when changing eco/comfort temperatures
	deviceConfigCache = make(map[string]*DeviceConfig)
	configMutex       sync.RWMutex
)

// DeviceConfig stores the full device configuration for ConfigTemperatures (Type 0x11)
// When changing eco/comfort temperatures, we preserve other values from the cached config.
type DeviceConfig struct {
	ComfortTemp        float64 // Comfort temperature (default 21.0)
	EcoTemp            float64 // Eco temperature (default 17.0)
	MaxTemp            float64 // Maximum temperature (default 30.5)
	MinTemp            float64 // Minimum temperature (default 4.5)
	MeasurementOffset  float64 // Temperature offset (-3.5 to +3.5, default 0)
	WindowOpenTemp     float64 // Window open temperature (default 12.0)
	WindowOpenDuration int     // Window open duration in minutes (default 15)
}

// getDeviceConfig returns the cached config for a device, or creates a new one with defaults
func getDeviceConfig(deviceAddr string) *DeviceConfig {
	configMutex.RLock()
	cfg, exists := deviceConfigCache[deviceAddr]
	configMutex.RUnlock()

	if exists {
		return cfg
	}

	// Create default config
	cfg = &DeviceConfig{
		ComfortTemp:        21.0,
		EcoTemp:            17.0,
		MaxTemp:            30.5,
		MinTemp:            4.5,
		MeasurementOffset:  0.0,
		WindowOpenTemp:     12.0,
		WindowOpenDuration: 15,
	}

	configMutex.Lock()
	deviceConfigCache[deviceAddr] = cfg
	configMutex.Unlock()

	return cfg
}

// Constants for MAX! Protocol
const (
	MaxMsgStart      = 'Z'
	TopicFormatState = "max/%s/state"
)

func main() {
	// 1. Load Configuration
	loadConfig()
	setupLogging()

	log.Info("Starting MAX! to MQTT Bridge")

	// 1.5. Duty Cycle
	initTransmissionManager()

	// 2. Setup MQTT
	setupMQTT()

	// 3. Setup Serial & Main Loop
	serialChan := make(chan string, 10)
	serialWrite = make(chan string, 10)
	go serialReaderLoop(serialChan, serialWrite)

	// Start periodic time sync (broadcasts time on startup and every 24 hours)
	go startPeriodicTimeSync()

	// 4. Handle Signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 5. Processing Loop
	for {
		select {
		case msg := <-serialChan:
			handleSerialMessage(msg)
		case <-sigChan:
			log.Info("Shutting down...")
			return
		}
	}
}

func loadConfig() {
	// Defaults
	config = Config{
		SerialPort:          "/dev/ttyACM0",
		BaudRate:            38400,
		MQTTBroker:          "tcp://homeassistant:1883",
		LogLevel:            "info",
		GatewayID:           "123456",
		DutyCycleMinCredits: 100,
		CommandTimeout:      "1m",
	}

	// Try loading from options.json (Standard HA Add-on location)
	if _, err := os.Stat("/data/options.json"); err == nil {
		data, err := os.ReadFile("/data/options.json")
		if err == nil {
			log.Info("Loading config from /data/options.json")
			// We define a temporary struct because HA Add-on options might be flat or nested
			// Usually they are flat key-values.
			_ = json.Unmarshal(data, &config)
		}
	}

	// Override with Env Vars (Standard Docker pattern)
	if v := os.Getenv("SERIAL_PORT"); v != "" {
		config.SerialPort = v
	}
	if v := os.Getenv("DUTY_CYCLE_MIN_CREDITS"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			config.DutyCycleMinCredits = val
		}
	}
	if v := os.Getenv("COMMAND_TIMEOUT"); v != "" {
		config.CommandTimeout = v
	}
	if v := os.Getenv("MQTT_BROKER"); v != "" {
		config.MQTTBroker = v
	}
	if v := os.Getenv("MQTT_USER"); v != "" {
		config.MQTTUser = v
	}
	if v := os.Getenv("MQTT_PASS"); v != "" {
		config.MQTTPass = v
	}
	if v := os.Getenv("GATEWAY_ID"); v != "" {
		config.GatewayID = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		config.LogLevel = v
	}
	if v := os.Getenv("BAUD_RATE"); v != "" {
		if rate, err := strconv.Atoi(v); err == nil {
			config.BaudRate = rate
		}
	}
}

func setupLogging() {
	lvl, err := log.ParseLevel(config.LogLevel)
	if err != nil {
		lvl = log.InfoLevel
	}
	log.SetLevel(lvl)
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	log.Infof("Log level set to: %s", lvl)
}

func setupMQTT() {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(config.MQTTBroker)
	if config.MQTTUser != "" {
		opts.SetUsername(config.MQTTUser)
		opts.SetPassword(config.MQTTPass)
	}
	opts.SetClientID("max2mqtt_bridge")
	opts.SetAutoReconnect(true)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Info("Connected to MQTT Broker")
		// Subscribe to set commands
		c.Subscribe("max/+/set", 0, handleMQTTCommand)
		c.Subscribe("max/+/mode/set", 0, handleMQTTModeCommand)
		c.Subscribe("max/bridge/pair", 0, handleMQTTPair)
		// Subscribe to config commands (eco temp, comfort temp, display mode)
		c.Subscribe("max/+/eco_temp/set", 0, handleMQTTEcoTemp)
		c.Subscribe("max/+/comfort_temp/set", 0, handleMQTTComfortTemp)
		c.Subscribe("max/+/display_mode/set", 0, handleMQTTDisplayMode)
	})

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Warn("Could not connect to MQTT initially, will retry in background: ", token.Error())
	}
}

// Serial Reader Loop with Reconnection
func serialReaderLoop(out chan<- string, in <-chan string) {
	for {
		// mode := &serial.Mode{
		// 	BaudRate: config.BaudRate,
		// }
		mode := &serial.Mode{
			BaudRate: config.BaudRate,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		}
		port, err := serial.Open(config.SerialPort, mode)
		if err != nil {
			log.Errorf("Failed to open serial port %s: %v. Retrying in 5s...", config.SerialPort, err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Infof("Opened serial port %s", config.SerialPort)

		// Allow reliable device startup/reboot time (Increased to 4s for NanoCULs)
		time.Sleep(4 * time.Second)

		// Clear any garbage from boot process
		port.ResetInputBuffer()

		// Send Initialization Commands
		// V   = Version Check (Tests comms)
		// Zr  = Enable MAX! Sniffer Mode (Critical)
		// X   = Query Credits
		initCmds := []string{"V", "Zr", "X"}
		for _, cmd := range initCmds {
			log.Infof("Sending Init Command: %s", cmd)
			if _, err := port.Write([]byte(cmd + "\r")); err != nil {
				log.Errorf("Failed to send init command %s: %v", cmd, err)
			}
			// Small delay between commands
			time.Sleep(500 * time.Millisecond)
		}

		// Start Writer Routine for this connection
		stopWriter := make(chan struct{})
		go func() {
			for {
				select {
				case cmd := <-in:
					log.Debugf("TX: %s", cmd)
					// Use \r only for consistency
					if _, err := port.Write([]byte(cmd + "\r")); err != nil {
						log.Errorf("Failed to write to serial: %v", err)
						return // Reader loop will detect error/close
					}
				case <-stopWriter:
					return
				}
			}
		}()

		scanner := bufio.NewScanner(port)
		for scanner.Scan() {
			text := scanner.Text()
			log.Debugf("RX: %s", text)
			if strings.HasPrefix(text, string(MaxMsgStart)) {
				out <- text
			} else if text == "LOVF" {
				// LOVF = Limit Of Voice Full - CUL TX buffer overflow
				log.Warn("CUL: LOVF (Limit Of Voice Full) - TX buffer overflow")
				if txMgr != nil {
					txMgr.SignalLOVF()
				}
			} else if txMgr != nil {
				// Check for Credit Report: "yy xxx" (e.g., "00 900")
				// Delegated to TransmissionManager's parser
				queueLen, credits, matched := ParseCreditResponse(text)
				if matched {
					txMgr.UpdateCredits(credits, queueLen)
					txMgr.SignalCreditResponse()
				}
			}
		}

		close(stopWriter)
		log.Warn("Serial connection lost or closed. Reconnecting...")
		port.Close()
		time.Sleep(2 * time.Second)
	}
}

// Parser Logic
type MaxPacket struct {
	Raw      string
	Length   int
	MsgCount int
	Flag     int
	Type     int
	SrcAddr  string
	DstAddr  string
	Group    int
	Payload  []byte
}

func parseMaxMessage(raw string) (*MaxPacket, error) {
	// Raw: Z0B0102031234561234560012
	// Remove 'Z'
	if len(raw) < 2 {
		return nil, fmt.Errorf("message too short")
	}
	hexStr := raw[1:]

	// Decode Hex
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}

	if len(data) < 10 { // Min header size
		return nil, fmt.Errorf("data too short for header")
	}

	// Structure (Z + Hex)
	// Byte 0: Length
	// Byte 1: Cnt
	// Byte 2: Flg
	// Byte 3: Type
	// Byte 4-6: Src
	// Byte 7-9: Dst
	// Byte 10: Grp
	// Byte 11+: Payload

	if len(data) < 11 { // Min header size
		return nil, fmt.Errorf("data too short for header")
	}

	pkt := &MaxPacket{
		Raw:      raw,
		Length:   int(data[0]),
		MsgCount: int(data[1]),
		Flag:     int(data[2]),
		Type:     int(data[3]),
		SrcAddr:  fmt.Sprintf("%06X", data[4:7]),
		DstAddr:  fmt.Sprintf("%06X", data[7:10]),
		Group:    int(data[10]),
		Payload:  data[11:],
	}

	return pkt, nil
}

func handleSerialMessage(raw string) {
	raw = strings.TrimSpace(raw)
	pkt, err := parseMaxMessage(raw)
	if err != nil {
		log.Debugf("Failed to parse message '%s': %v", raw, err)
		return
	}

	log.Infof("Parsed Packet from %s (Type 0x%02X): %X", pkt.SrcAddr, pkt.Type, pkt.Payload)

	// Check if this is a PairPing (Type 0x00) and we are in pairing mode
	if pkt.Type == 0x00 {
		if time.Now().Before(pairingUntil) {
			log.Infof("Received PairPing from %s while in pairing mode. Sending PairPong...", pkt.SrcAddr)
			sendPairPong(pkt.SrcAddr)
		} else {
			log.Debugf("Received PairPing from %s but pairing mode is not active.", pkt.SrcAddr)
		}
		return
	}

	// Notify ACK received for TX handshaking (Type 0x02 = ACK)
	// Do this immediately to unblock pending TX, regardless of payload length/validity for state.
	// We also accept 0x70 (Wall), 0x60 (Rad), and 0x42 (WallCtrl) as implicit ACKs,
	// as receiving them confirms the device is responsive.
	// We also accept 0x40 (SetTemperature) as implicit ACK - device broadcasts state after config changes
	isAck := pkt.Type == 0x02 || pkt.Type == 0x70 || pkt.Type == 0x60 || pkt.Type == 0x42 || pkt.Type == 0x40
	if isAck && txMgr != nil {
		txMgr.NotifyAck(pkt.SrcAddr)
	}

	// Message Type Reference (from FHEM 10_MAX.pm):
	// 0x00 = PairPing
	// 0x02 = Ack (contains state data)
	// 0x60 = ThermostatState (HeatingThermostat) - IGNORED
	// 0x70 = WallThermostatState (WallMountedThermostat)
	// 0x42 = WallThermostatControl
	// 0x30 = ShutterContactState
	// 0x40 = SetTemperature

	// Filter: Process Wall Thermostats (0x70), Radiator Thermostats (0x60), and Control/Acks.
	// 1. Explicit State types: 0x70 (Wall), 0x60 (Radiator), 0x42 (Wall Control)
	// 2. Ack (0x02):
	//    - Extended: Length >= 6 (Has actual temp)
	//    - Standard: Length >= 4 (Has Setpoint/Mode/Valve) - Essential for confirmation
	// 3. SetTemperature (0x40): Manual change on Wall Thermostat
	isValidType := pkt.Type == 0x70 || pkt.Type == 0x60 || pkt.Type == 0x42 || pkt.Type == 0x40
	isValidAck := pkt.Type == 0x02 && len(pkt.Payload) >= 4

	if !isValidType && !isValidAck {
		log.Infof("Ignored message from device %s (Type 0x%02X, PayloadLen %d) - Not a valid state packet", pkt.SrcAddr, pkt.Type, len(pkt.Payload))
		// Explicitly ignored:
		// - 0x30 (Shutter Contact)
		// - 0x02 (Short Ack < 4 bytes)
		return
	}

	// Decode State first to check for Actual Temperature
	newData := decodePayload(pkt)
	if newData == nil {
		return
	}

	// Valid Message -> Update Discovery & State
	sendDiscovery(pkt.SrcAddr)
	updateDeviceState(pkt.SrcAddr, newData)
}

func updateDeviceState(srcAddr string, newData *MaxDeviceData) {
	stateMutex.Lock()
	existing, exists := stateCache[srcAddr]
	if !exists {
		existing = &MaxDeviceData{}
		stateCache[srcAddr] = existing
	}

	// Merge specific fields if valid
	if newData.Temperature > 0 {
		existing.Temperature = newData.Temperature
	}
	if newData.CurrentTemperature != nil {
		existing.CurrentTemperature = newData.CurrentTemperature
	}

	if newData.Mode != "" {
		existing.Mode = newData.Mode
	}
	if newData.HVACMode != "" {
		existing.HVACMode = newData.HVACMode
	}
	if newData.Battery != "" {
		existing.Battery = newData.Battery
	}

	// Calculate HVAC Action based on User Rule:
	// if current_temperature < temperature, then "heating" else "idle"
	if existing.CurrentTemperature != nil {
		if *existing.CurrentTemperature < existing.Temperature {
			existing.HVACAction = "heating"
		} else {
			existing.HVACAction = "idle"
		}
	} else {
		// Without current temp, we default to idle to be safe,
		// or we could leave it explicitly empty/unknown.
		existing.HVACAction = "idle"
	}

	// Make a copy for publishing to avoid race conditions after unlock
	dataToPublish := *existing
	if existing.CurrentTemperature != nil {
		val := *existing.CurrentTemperature
		dataToPublish.CurrentTemperature = &val
	}
	stateMutex.Unlock()

	log.Infof("Decoded Data for %s: %s", srcAddr, &dataToPublish)
	publishState(srcAddr, &dataToPublish)
}

type MaxDeviceData struct {
	Temperature        float64  `json:"temperature,omitempty"`
	CurrentTemperature *float64 `json:"current_temperature,omitempty"`

	Mode       string `json:"mode,omitempty"`
	HVACMode   string `json:"hvac_mode,omitempty"`
	HVACAction string `json:"hvac_action,omitempty"`
	Battery    string `json:"battery,omitempty"`
}

// convert pointer to string for appropriate logging
func (d *MaxDeviceData) String() string {
	curTempStr := "N/A"
	if d.CurrentTemperature != nil {
		curTempStr = fmt.Sprintf("%.1f", *d.CurrentTemperature)
	}
	return fmt.Sprintf("Temp: %.1f, CurTemp: %s, Mode: %s, Action: %s, Batt: %s",
		d.Temperature, curTempStr, d.Mode, d.HVACAction, d.Battery)
}

// decodePayload parses the MAX! thermostat state payload
// Based on references/protocol
func decodePayload(pkt *MaxPacket) *MaxDeviceData {
	// Protocol:
	// Type 0x02, 0x70, 0x60: Thermostat Status (Mode, Valve, Setpoint, Optional Actual)
	// Type 0x42: WallThermostatControl (Setpoint, Actual Temp)
	// Type 0x40: SetTemperature (Mode, Setpoint)
	if pkt.Type != 0x02 && pkt.Type != 0x70 && pkt.Type != 0x60 && pkt.Type != 0x42 && pkt.Type != 0x40 {
		return nil
	}

	payload := pkt.Payload
	data := &MaxDeviceData{}

	// Handle 0x42 (WallThermostatControl)
	// Payload: [SetpointRaw] [ActualTempRaw]
	if pkt.Type == 0x42 {
		if len(payload) < 2 {
			return nil
		}
		// Byte 0: Setpoint (Bits 0-6) / 2
		// Bit 7 is likely Mode-related or just ignored in simple control packets?
		// FHEM uses: ($payload[0] & 0x7F) / 2
		data.Temperature = float64(payload[0]&0x7F) / 2.0

		// Byte 1: Actual Temperature (Raw / 10)
		// FHEM uses: $payload[1] / 10
		// Note: Protocol usually has 9th bit for temp in Byte 0 MSB?
		// For 0x42, it seems simpler: just Byte 1.
		actual := float64(payload[1]) / 10.0
		data.CurrentTemperature = &actual

		return data
	}

	// Handle 0x40 (SetTemperature)
	// Payload: [TargetTemp/Mode]
	if pkt.Type == 0x40 {
		if len(payload) < 1 {
			return nil
		}
		// Byte 0:
		// Bits 0-5: Temp / 2
		// Bits 6-7: Mode
		byte0 := payload[0]

		// Mode
		modeBits := (byte0 >> 6) & 0x03
		switch modeBits {
		case 0:
			data.Mode = "auto"
			data.HVACMode = "auto"
		case 1:
			data.Mode = "manual"
			data.HVACMode = "heat"
		case 2:
			data.Mode = "vacation"
			data.HVACMode = "off"
		case 3:
			data.Mode = "boost"
			data.HVACMode = "heat"
		}

		// Temperature
		// Formula: (Byte & 0x3F) / 2
		tempRaw := byte0 & 0x3F
		data.Temperature = float64(tempRaw) / 2.0

		// Special case: Off (4.5C)
		if data.Mode == "manual" && data.Temperature <= 4.5 {
			data.HVACMode = "off"
		}

		return data
	}

	// Handle 0x02, 0x70

	// Fix for Type 0x02 ("Ack/Status"): The first byte is a generic status/ack,
	// followed by the actual payload (Mode/Valve/Temp).
	if pkt.Type == 0x02 {
		if len(payload) < 1 {
			return nil
		}
		payload = payload[1:]
	}

	if len(payload) < 3 {
		return nil
	}

	// Byte 0: Mode & Battery
	// Bits 0-1 (Mode): 00=Auto, 01=Manual, 10=Vacation, 11=Boost
	// Bit 7 (Battery): 1=Low Battery
	modeBits := payload[0] & 0x03
	switch modeBits {
	case 0:
		data.Mode = "auto"
		data.HVACMode = "auto"
	case 1:
		data.Mode = "manual"
		data.HVACMode = "heat"
	case 2:
		data.Mode = "vacation"
		data.HVACMode = "off"
	case 3:
		data.Mode = "boost"
		data.HVACMode = "heat"
	}

	if (payload[0] & 0x80) != 0 {
		data.Battery = "low"
	} else {
		data.Battery = "ok"
	}

	// Byte 2: Setpoint Temperature
	// Formula: (Byte & 0x7F) / 2
	setpointRaw := payload[2] & 0x7F
	data.Temperature = float64(setpointRaw) / 2.0

	// Special case: If Mode is manual (heat) but Temp is 4.5 (OFF), map to HVAC "off"
	if data.Mode == "manual" && data.Temperature <= 4.5 {
		data.HVACMode = "off"
	}

	// Dynamic Wall Thermostat Detection (for 0x70 / 0x02)
	// Protocol: If Payload Length >= 5, Bytes 3-4 are Actual Temperature
	if len(payload) >= 5 {
		// Formula: ((Byte3 & 0x01) << 8) | Byte4
		// Divide by 10.0
		byte3 := payload[3]
		byte4 := payload[4]

		rawTemp := int(byte3&0x01)<<8 | int(byte4)
		actualTemp := float64(rawTemp) / 10.0

		data.CurrentTemperature = &actualTemp
	}

	return data
}

func sendDiscovery(srcAddr string) {
	// 1. Climate Entity
	deviceID := "max_" + srcAddr

	// Common Device Info
	deviceInfo := map[string]string{
		"identifiers":  deviceID,
		"manufacturer": "eQ-3",
		"model":        "MAX! Device",
		"name":         "MAX! " + srcAddr,
	}

	// Climate Config
	// MAX! modes are mapped to Home Assistant HVAC modes:
	// Auto -> auto, Manual -> heat, Manual (4.5°C) -> off, Vacation -> off
	// The separate Mode Select entity has been removed.
	climateTopic := fmt.Sprintf("homeassistant/climate/%s/config", deviceID)
	climatePayload := map[string]interface{}{
		"name":         "MAX! Thermostat " + srcAddr,
		"unique_id":    deviceID + "_climate",
		"device_class": "climate",
		// ... standard fields ...
		"state_topic":                  fmt.Sprintf(TopicFormatState, srcAddr),
		"temperature_command_topic":    fmt.Sprintf("max/%s/set", srcAddr),
		"temperature_state_topic":      fmt.Sprintf(TopicFormatState, srcAddr),
		"temperature_state_template":   "{{ value_json.temperature }}",
		"current_temperature_topic":    fmt.Sprintf(TopicFormatState, srcAddr),
		"current_temperature_template": "{{ value_json.current_temperature }}",
		"mode_state_topic":             fmt.Sprintf(TopicFormatState, srcAddr),
		"mode_state_template":          "{{ value_json.hvac_mode }}",
		"mode_command_topic":           fmt.Sprintf("max/%s/mode/set", srcAddr),
		"modes":                        []string{"auto", "heat", "off"},
		"min_temp":                     5,
		"max_temp":                     30,
		"temp_step":                    0.5,
		"precision":                    0.1,
		"temperature_unit":             "C",
		"action_topic":                 fmt.Sprintf(TopicFormatState, srcAddr),
		"action_template":              "{{ value_json.hvac_action }}",
		"device":                       deviceInfo,
	}

	bytes, _ := json.Marshal(climatePayload)
	mqttClient.Publish(climateTopic, 0, true, bytes)

	// 3. Battery Binary Sensor
	// Note: We use 'device_class: battery' which correctly maps "ON" to "Low" and "OFF" to "Normal" in HA.
	// Previous versions might have used a regular sensor, but binary_sensor is semantically correct here.
	battTopic := fmt.Sprintf("homeassistant/binary_sensor/%s_battery/config", deviceID)
	battPayload := map[string]interface{}{
		"name":         "Battery",
		"unique_id":    deviceID + "_battery",
		"device_class": "battery", // For binary_sensor: On=Low, Off=Normal
		"state_topic":  fmt.Sprintf(TopicFormatState, srcAddr),
		// Template: Return ON (Low) or OFF (Normal)
		"value_template": "{{ 'ON' if value_json.battery == 'low' else 'OFF' }}",
		"device":         deviceInfo,
	}
	bytes, _ = json.Marshal(battPayload)
	token := mqttClient.Publish(battTopic, 0, true, bytes)
	token.Wait()

	// 4. Eco Temperature Number Entity
	// Allows configuration of eco temperature from Home Assistant
	// mode: "box" displays as text input instead of slider
	ecoTempTopic := fmt.Sprintf("homeassistant/number/%s_eco_temp/config", deviceID)
	ecoTempPayload := map[string]interface{}{
		"name":                "Eco Temperature",
		"unique_id":           deviceID + "_eco_temp",
		"icon":                "mdi:leaf",
		"entity_category":     "config",
		"mode":                "box",
		"min":                 4.5,
		"max":                 30.5,
		"step":                0.5,
		"unit_of_measurement": "°C",
		"command_topic":       fmt.Sprintf("max/%s/eco_temp/set", srcAddr),
		"device":              deviceInfo,
	}
	bytes, _ = json.Marshal(ecoTempPayload)
	mqttClient.Publish(ecoTempTopic, 0, true, bytes)

	// 5. Comfort Temperature Number Entity
	// Allows configuration of comfort temperature from Home Assistant
	// mode: "box" displays as text input instead of slider
	comfortTempTopic := fmt.Sprintf("homeassistant/number/%s_comfort_temp/config", deviceID)
	comfortTempPayload := map[string]interface{}{
		"name":                "Comfort Temperature",
		"unique_id":           deviceID + "_comfort_temp",
		"icon":                "mdi:sofa",
		"entity_category":     "config",
		"mode":                "box",
		"min":                 4.5,
		"max":                 30.5,
		"step":                0.5,
		"unit_of_measurement": "°C",
		"command_topic":       fmt.Sprintf("max/%s/comfort_temp/set", srcAddr),
		"device":              deviceInfo,
	}
	bytes, _ = json.Marshal(comfortTempPayload)
	mqttClient.Publish(comfortTempTopic, 0, true, bytes)

	// 6. Display Mode Select Entity (Wall Thermostats only, but shown for all devices)
	// Options: "Setpoint" (show target temp) or "Actual" (show current temp)
	// Note: This setting only affects Wall Thermostats; Radiator Thermostats will ignore it.
	displayModeTopic := fmt.Sprintf("homeassistant/select/%s_display_mode/config", deviceID)
	displayModePayload := map[string]interface{}{
		"name":            "Display Mode",
		"unique_id":       deviceID + "_display_mode",
		"icon":            "mdi:thermometer-lines",
		"entity_category": "config",
		"command_topic":   fmt.Sprintf("max/%s/display_mode/set", srcAddr),
		"options":         []string{"Setpoint", "Actual"},
		"optimistic":      true,
		"device":          deviceInfo,
	}
	bytes, _ = json.Marshal(displayModePayload)
	mqttClient.Publish(displayModeTopic, 0, true, bytes)
}

func publishState(srcAddr string, data *MaxDeviceData) {
	deviceID := "max_" + srcAddr
	topic := fmt.Sprintf(TopicFormatState, srcAddr)

	payload, err := json.Marshal(data)
	if err != nil {
		log.Errorf("Failed to marshal state for %s: %v", deviceID, err)
		return
	}

	// Enhanced Logging to show Entity Mapping
	log.Debugf("MQTT PUB %s (Broadcast to all entities):", topic)
	log.Debugf("  -> Climate (Temp): %.1f", data.Temperature)
	if data.CurrentTemperature != nil {
		log.Debugf("  -> Climate (CurTemp): %.1f", *data.CurrentTemperature)
	}
	log.Debugf("  -> Climate (HVAC): %s", data.HVACMode)
	log.Debugf("  -> Internal Mode: %s", data.Mode)

	log.Debugf("  -> Binary (Battery): %s", data.Battery)
	log.Debugf("  Raw Payload: %s", string(payload))

	token := mqttClient.Publish(topic, 0, true, payload) // Changed to retained=true
	token.Wait()
}

func handleMQTTCommand(client mqtt.Client, msg mqtt.Message) {
	// Topic: max/[ID]/set
	// Payload: Temperature float (e.g. "21.5")
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 3 {
		return
	}
	srcAddr := topicParts[1]
	payloadStr := string(msg.Payload())

	log.Infof("Received command for %s: %s", srcAddr, payloadStr)

	temp, err := strconv.ParseFloat(payloadStr, 64)
	if err != nil {
		log.Error("Invalid temperature format")
		return
	}

	log.Infof("Processing set temperature command: %.1f for %s", temp, srcAddr)

	// Preserve current mode from state cache (default to Manual if unknown)
	// Mode bits: 00=Auto, 01=Manual, 10=Vacation, 11=Boost
	var modeBits byte = 0x01 // Default to Manual

	stateMutex.RLock()
	if existing, ok := stateCache[srcAddr]; ok {
		switch existing.Mode {
		case "auto":
			modeBits = 0x00
		case "manual":
			modeBits = 0x01
		case "vacation":
			modeBits = 0x02
		case "boost":
			modeBits = 0x03
		}
		log.Debugf("Preserving current mode '%s' (0x%02X) for %s", existing.Mode, modeBits, srcAddr)
	} else {
		log.Warnf("No cached state for %s, defaulting to Manual mode", srcAddr)
	}
	stateMutex.RUnlock()

	// Payload: (Temp * 2) | (Mode << 6)
	// Protocol says: (Method 0x40) Formula: (TargetTemp * 2) + (ModeBits << 6).
	tempVal := byte(temp * 2)
	payloadByte := tempVal | (modeBits << 6)

	sendCommand(srcAddr, 0x40, []byte{payloadByte}, fmt.Sprintf("Set Temp %.1f", temp))
}

func handleMQTTPair(client mqtt.Client, msg mqtt.Message) {
	log.Info("Activating Pairing Mode for 60 seconds")
	pairingUntil = time.Now().Add(60 * time.Second)
}

func handleMQTTModeCommand(client mqtt.Client, msg mqtt.Message) {
	// Topic: max/[ID]/mode/set
	// Payload: Mode string (auto, manual, boost, vacation)
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 4 {
		return
	}
	srcAddr := topicParts[1]
	mode := string(msg.Payload())

	log.Infof("Received mode command for %s: %s", srcAddr, mode)

	// Determine Mode Bits and Target Temp
	var modeBits byte
	var targetTemp float64 = 21.0 // Default fallback

	// Retrieve current state for temp preservation
	stateMutex.RLock()
	if existing, ok := stateCache[srcAddr]; ok && existing.Temperature > 0 {
		targetTemp = existing.Temperature
		log.Debugf("Preserving current temperature %.1f for mode change to %s", targetTemp, mode)
	}
	stateMutex.RUnlock()

	switch mode {
	case "auto":
		modeBits = 0x00
		// In Auto, we send the current encoded temp to effectively set a temporary override
		// matching the current setpoint, preserving the value as requested.
	case "heat": // Manual
		modeBits = 0x01
		// Use current setpoint
	case "off":
		modeBits = 0x01 // Manual
		targetTemp = 4.5
	case "boost": // Boost
		modeBits = 0x03
		targetTemp = 30.0 // Usually ignored for boost but safe max
	default:
		log.Warnf("Unknown mode %s", mode)
		return
	}

	tempVal := byte(targetTemp * 2)
	payloadByte := tempVal | (modeBits << 6)

	sendCommand(srcAddr, 0x40, []byte{payloadByte}, fmt.Sprintf("Set Mode %s", mode))
}

// sendCommand constructs and sends a MAX! message via CUL
func sendCommand(dstAddr string, typeByte byte, payload []byte, description string) {
	// Increment Global Counter
	msgCounterMutex.Lock()
	msgCounter++
	if msgCounter > 255 {
		msgCounter = 1
	}
	cnt := msgCounter
	msgCounterMutex.Unlock()

	// Src Address (Cube ID)
	srcBytes, _ := hex.DecodeString(config.GatewayID)
	if len(srcBytes) != 3 {
		srcBytes = []byte{0x12, 0x34, 0x56} // Fallback
	}

	// Dst Address
	dstBytes, _ := hex.DecodeString(dstAddr)
	if len(dstBytes) != 3 {
		log.Errorf("Invalid destination address: %s", dstAddr)
		return
	}

	// Message Construction
	// Structure: Len(1) | Cnt(1) | Flg(1) | Type(1) | Src(3) | Dst(3) | Grp(1) | Payload(...)
	// Length byte excludes itself.
	// Fixed Header fields after Len: Cnt(1)+Flg(1)+Type(1)+Src(3)+Dst(3)+Grp(1) = 10 bytes
	packetLen := 10 + len(payload)

	// Flag: 0x04 for broadcasts, 0x00 for regular commands (per maxcul.js)
	var flag byte = 0x00
	if dstAddr == "000000" {
		flag = 0x04
	}

	packet := make([]byte, 0, packetLen+1)
	packet = append(packet, byte(packetLen))
	packet = append(packet, byte(cnt))
	packet = append(packet, flag)
	packet = append(packet, typeByte)
	packet = append(packet, srcBytes...)
	packet = append(packet, dstBytes...)
	packet = append(packet, 0x00) // GroupID (0)
	packet = append(packet, payload...)

	// Encode to Hex
	hexStr := hex.EncodeToString(packet)

	// Send using Duty Cycle Manager
	// CUL expects: Zs<HexData>
	cmd := "Zs" + hexStr

	if txMgr != nil {
		txMgr.Enqueue(dstAddr, cmd, description)
	} else {
		// Fallback if not init (should not happen)
		select {
		case serialWrite <- cmd:
			log.Infof("TX -> %s: %s [%s]", dstAddr, description, hexStr)
		default:
			log.Error("Serial write queue full")
		}
	}
}

func sendPairPong(targetAddr string) {
	// PairPong: Type 0x01
	// Structure: Len(1) | Cnt(1) | Type(1) | Src(3) | Dst(3) | Payload(1=0x00?)
	// To send via CUL: "Zs" + HexString

	// Increment Global Counter safely
	msgCounterMutex.Lock()
	msgCounter++
	if msgCounter > 255 {
		msgCounter = 1
	}
	cnt := msgCounter
	msgCounterMutex.Unlock()

	srcBytes, _ := hex.DecodeString(config.GatewayID)
	if len(srcBytes) != 3 {
		srcBytes = []byte{0x12, 0x34, 0x56} // Fallback default
	}

	dstBytes, _ := hex.DecodeString(targetAddr)

	// Payload must be 5 bytes to hit Len 0x0E
	// Byte 0: Group (0x00)
	// Byte 1-4: Unknown/Padding? (0x00)
	payload := []byte{0x00, 0x00, 0x00, 0x00, 0x00}

	// Structure: Len | Cnt | Flg | Type | Src | Dst | Payload
	// Fields after Len: Cnt(1) + Flg(1) + Type(1) + Src(3) + Dst(3) + Payload(5) = 14 bytes (0x0E)
	msgLen := 0x0E

	// Pkt: Len, Cnt, Flg(0x00), Type(0x01)
	packet := []byte{byte(msgLen), byte(cnt), 0x00, 0x01}
	packet = append(packet, srcBytes...)
	packet = append(packet, dstBytes...)
	packet = append(packet, payload...)

	hexStr := hex.EncodeToString(packet)
	// Send as CUL send command (Zs = Send raw)
	cmd := "Zs" + hexStr

	select {
	case serialWrite <- cmd:
		log.Infof("Sent PairPong to %s (Cnt: %d)", targetAddr, cnt)
		// Schedule time broadcast for newly paired device
		log.Infof("Scheduling time broadcast for newly paired device %s", targetAddr)
		go func() {
			time.Sleep(2 * time.Second)
			sendTimeBroadcast()
		}()
	default:
		log.Error("Serial write queue full")
	}
}

// handleMQTTEcoTemp handles eco temperature configuration
// Topic: max/<id>/eco_temp/set
// Payload: Temperature float (e.g. "17.0")
func handleMQTTEcoTemp(client mqtt.Client, msg mqtt.Message) {
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 4 {
		return
	}
	srcAddr := topicParts[1]
	payloadStr := string(msg.Payload())

	temp, err := strconv.ParseFloat(payloadStr, 64)
	if err != nil {
		log.Errorf("Invalid eco temperature format: %s", payloadStr)
		return
	}

	// Validate range (4.5 - 30.5)
	if temp < 4.5 || temp > 30.5 {
		log.Errorf("Eco temperature out of range (4.5-30.5): %.1f", temp)
		return
	}

	log.Infof("Setting eco temperature for %s: %.1f", srcAddr, temp)

	// Update cached config
	cfg := getDeviceConfig(srcAddr)
	configMutex.Lock()
	cfg.EcoTemp = temp
	configMutex.Unlock()

	// Send ConfigTemperatures command
	sendConfigTemperatures(srcAddr, cfg)
}

// handleMQTTComfortTemp handles comfort temperature configuration
// Topic: max/<id>/comfort_temp/set
// Payload: Temperature float (e.g. "21.0")
func handleMQTTComfortTemp(client mqtt.Client, msg mqtt.Message) {
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 4 {
		return
	}
	srcAddr := topicParts[1]
	payloadStr := string(msg.Payload())

	temp, err := strconv.ParseFloat(payloadStr, 64)
	if err != nil {
		log.Errorf("Invalid comfort temperature format: %s", payloadStr)
		return
	}

	// Validate range (4.5 - 30.5)
	if temp < 4.5 || temp > 30.5 {
		log.Errorf("Comfort temperature out of range (4.5-30.5): %.1f", temp)
		return
	}

	log.Infof("Setting comfort temperature for %s: %.1f", srcAddr, temp)

	// Update cached config
	cfg := getDeviceConfig(srcAddr)
	configMutex.Lock()
	cfg.ComfortTemp = temp
	configMutex.Unlock()

	// Send ConfigTemperatures command
	sendConfigTemperatures(srcAddr, cfg)
}

// handleMQTTDisplayMode handles display mode for wall thermostats
// Topic: max/<id>/display_mode/set
// Payload: "Setpoint" (show target temp) or "Actual" (show current temp)
// Note: This only affects Wall Thermostats; Radiator Thermostats will ignore this command.
func handleMQTTDisplayMode(client mqtt.Client, msg mqtt.Message) {
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 4 {
		return
	}
	srcAddr := topicParts[1]
	payload := string(msg.Payload())

	showActual := strings.EqualFold(payload, "Actual")
	log.Infof("Setting display mode for %s: %s (showActual=%v)", srcAddr, payload, showActual)

	sendDisplayMode(srcAddr, showActual)
}

// sendConfigTemperatures sends Type 0x11 ConfigTemperatures command
// Uses cached config values for all parameters
// Payload structure (7 bytes):
//   - Byte 0: Comfort temperature (*2)
//   - Byte 1: Eco temperature (*2)
//   - Byte 2: Max temperature (*2)
//   - Byte 3: Min temperature (*2)
//   - Byte 4: Measurement offset ((value + 3.5) * 2)
//   - Byte 5: Window open temperature (*2)
//   - Byte 6: Window open duration (/5 minutes)
func sendConfigTemperatures(dstAddr string, cfg *DeviceConfig) {
	payload := make([]byte, 7)
	payload[0] = byte(cfg.ComfortTemp * 2)
	payload[1] = byte(cfg.EcoTemp * 2)
	payload[2] = byte(cfg.MaxTemp * 2)
	payload[3] = byte(cfg.MinTemp * 2)
	payload[4] = byte((cfg.MeasurementOffset + 3.5) * 2)
	payload[5] = byte(cfg.WindowOpenTemp * 2)
	payload[6] = byte(cfg.WindowOpenDuration / 5)

	description := fmt.Sprintf("Config Temps (Comfort: %.1f, Eco: %.1f)", cfg.ComfortTemp, cfg.EcoTemp)
	sendCommand(dstAddr, 0x11, payload, description)
}

// sendDisplayMode sends Type 0x82 SetDisplayActualTemperature command
// Payload: 0x04 (show actual temperature) or 0x00 (show setpoint)
// Note: This command only works on Wall Thermostats
func sendDisplayMode(dstAddr string, showActual bool) {
	var payload byte = 0x00
	if showActual {
		payload = 0x04
	}

	displayStr := "setpoint"
	if showActual {
		displayStr = "actual"
	}
	sendCommand(dstAddr, 0x82, []byte{payload}, fmt.Sprintf("Set Display Mode: %s", displayStr))
}

// sendTimeBroadcast sends Type 0x03 TimeInformation to broadcast address 000000
// All devices paired with this gateway will receive and accept this message.
// Payload format (5 bytes):
//   - Byte 0: Year (since 2000)
//   - Byte 1: Day of month
//   - Byte 2: Hour
//   - Byte 3: Minute | ((Month & 0x0C) << 4)
//   - Byte 4: Second | ((Month & 0x03) << 6)
func sendTimeBroadcast() {
	now := time.Now()
	year := byte(now.Year() - 2000)
	month := byte(now.Month())
	day := byte(now.Day())
	hour := byte(now.Hour())
	min := byte(now.Minute())
	sec := byte(now.Second())

	compressedOne := min | ((month & 0x0C) << 4)
	compressedTwo := sec | ((month & 0x03) << 6)

	payload := []byte{year, day, hour, compressedOne, compressedTwo}

	log.Infof("Broadcasting time sync: %d-%02d-%02d %02d:%02d:%02d",
		now.Year(), month, day, hour, min, sec)

	sendCommand("000000", 0x03, payload, "Time Broadcast")
}

// startPeriodicTimeSync starts a goroutine that broadcasts time on startup
// and then every 24 hours to keep all paired devices synchronized.
func startPeriodicTimeSync() {
	// Initial delay to allow serial connection to be established
	time.Sleep(10 * time.Second)

	for {
		log.Info("Sending periodic time broadcast")
		sendTimeBroadcast()
		time.Sleep(24 * time.Hour)
	}
}
