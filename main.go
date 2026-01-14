package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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

	// Transmission Manager Config
	DutyCycleMinCredits int    `json:"duty_cycle_min_credits"`
	CommandTimeout      string `json:"command_timeout"`
	MaxCulQueue         int    `json:"max_cul_queue"`
	TxCheckPeriod       string `json:"tx_check_period"`
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
	HexTypeFormat    = "0x%02X"
)

func main() {
	// 1. Load Configuration
	loadConfig()
	setupLogging()

	fmt.Printf("time=\"%s\" level=info msg=\"Starting MAX! to MQTT Bridge\"\n", time.Now().Format(time.RFC3339))

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
			slog.Warn("Shutting down...")
			return
		}
	}
}

func loadConfig() {
	// Defaults
	config = Config{
		SerialPort:          "/dev/serial/by-id/usb-SHK_NANO_CUL_868-if00-port0",
		BaudRate:            38400,
		MQTTBroker:          "tcp://homeassistant:1883",
		LogLevel:            "info",
		GatewayID:           "123456",
		DutyCycleMinCredits: 100,
		CommandTimeout:      "1m",
		MaxCulQueue:         5,
		TxCheckPeriod:       "1m",
	}

	// Try loading from options.json (Standard HA Add-on location)
	loadConfigFromFile()

	// Override with Env Vars (Standard Docker pattern)
	loadEnvOverrides()
}

// loadConfigFromFile loads config from HA Add-on options.json if present
func loadConfigFromFile() {
	if _, err := os.Stat("/data/options.json"); err != nil {
		return
	}
	data, err := os.ReadFile("/data/options.json")
	if err != nil {
		return
	}
	slog.Info("Loading config from /data/options.json")
	_ = json.Unmarshal(data, &config)
}

// loadEnvOverrides applies environment variable overrides to config
// loadEnvOverrides applies environment variable overrides to config
func loadEnvOverrides() {
	loadEnvString("SERIAL_PORT", &config.SerialPort)
	loadEnvInt("DUTY_CYCLE_MIN_CREDITS", &config.DutyCycleMinCredits)
	loadEnvString("COMMAND_TIMEOUT", &config.CommandTimeout)
	loadEnvInt("MAX_CUL_QUEUE", &config.MaxCulQueue)
	loadEnvString("TX_CHECK_PERIOD", &config.TxCheckPeriod)
	loadEnvString("MQTT_BROKER", &config.MQTTBroker)
	loadEnvString("MQTT_USER", &config.MQTTUser)
	loadEnvString("MQTT_PASS", &config.MQTTPass)
	loadEnvString("GATEWAY_ID", &config.GatewayID)
	loadEnvString("LOG_LEVEL", &config.LogLevel)
	loadEnvInt("BAUD_RATE", &config.BaudRate)
}

func loadEnvString(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

func loadEnvInt(key string, target *int) {
	if v := os.Getenv(key); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			*target = val
		}
	}
}

func setupLogging() {
	var lvl slog.Level
	switch strings.ToLower(config.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)

	fmt.Printf("time=\"%s\" level=info msg=\"Log level set to: %s\"\n", time.Now().Format(time.RFC3339), lvl)
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
		slog.Info("Connected to MQTT Broker")
		// Subscribe to set commands
		c.Subscribe("max/+/set", 0, handleMQTTCommand)
		c.Subscribe("max/+/mode/set", 0, handleMQTTModeCommand)
		c.Subscribe("max/bridge/pair", 0, handleMQTTPair)
		// Subscribe to config commands (eco temp, comfort temp, display mode)
		c.Subscribe("max/+/eco_temp/set", 0, handleMQTTEcoTemp)
		c.Subscribe("max/+/comfort_temp/set", 0, handleMQTTComfortTemp)
		c.Subscribe("max/+/display_mode/set", 0, handleMQTTDisplayMode)
		c.Subscribe("max/+/associate", 0, handleMQTTAssociate)
	})

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		slog.Warn("Could not connect to MQTT initially, will retry in background", "error", token.Error())
	}
}

// Serial Reader Loop with Reconnection
func serialReaderLoop(out chan<- string, in <-chan string) {
	for {
		port := openSerialPort()
		if port == nil {
			time.Sleep(5 * time.Second)
			continue
		}

		initializeCUL(port)

		// Start Writer Routine for this connection
		stopWriter := make(chan struct{})
		go startSerialWriter(port, in, stopWriter)

		// Read loop
		scanner := bufio.NewScanner(port)
		for scanner.Scan() {
			processSerialLine(scanner.Text(), out)
		}

		close(stopWriter)
		slog.Warn("Serial connection lost or closed. Reconnecting...")
		port.Close()
		time.Sleep(2 * time.Second)
	}
}

// openSerialPort attempts to open the configured serial port.
// Returns nil if the port could not be opened.
func openSerialPort() serial.Port {
	mode := &serial.Mode{
		BaudRate: config.BaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(config.SerialPort, mode)
	if err != nil {
		slog.Error("Failed to open serial port", "port", config.SerialPort, "error", err)
		return nil
	}
	slog.Info("Opened serial port", "port", config.SerialPort)
	return port
}

// initializeCUL sends initialization commands to the CUL device
func initializeCUL(port serial.Port) {
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
		slog.Info("Sending Init Command", "command", cmd)
		if _, err := port.Write([]byte(cmd + "\r")); err != nil {
			slog.Error("Failed to send init command", "command", cmd, "error", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// startSerialWriter runs the serial write loop until stopWriter is closed
func startSerialWriter(port serial.Port, in <-chan string, stopWriter <-chan struct{}) {
	for {
		select {
		case cmd := <-in:
			slog.Debug("TX", "command", cmd)
			if _, err := port.Write([]byte(cmd + "\r")); err != nil {
				slog.Error("Failed to write to serial", "error", err)
				return
			}
		case <-stopWriter:
			return
		}
	}
}

// processSerialLine handles a single line received from the serial port
func processSerialLine(text string, out chan<- string) {
	slog.Debug("RX", "text", text)

	if strings.HasPrefix(text, string(MaxMsgStart)) {
		out <- text
		return
	}

	if text == "LOVF" {
		slog.Warn("CUL: LOVF (Limit Of Voice Full) - TX buffer overflow")
		if txMgr != nil {
			txMgr.SignalLOVF()
		}
		return
	}

	// Check for Credit Report: "yy xxx" (e.g., "00 900")
	if txMgr != nil {
		queueLen, credits, matched := ParseCreditResponse(text)
		if matched {
			txMgr.UpdateCredits(credits, queueLen)
			txMgr.SignalCreditResponse()
		}
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
		slog.Debug("Failed to parse message", "raw", raw, "error", err)
		return
	}

	slog.Info("Parsed Packet", "src", pkt.SrcAddr, "type", fmt.Sprintf(HexTypeFormat, pkt.Type), "payload", fmt.Sprintf("%X", pkt.Payload))

	// Check if this is a PairPing (Type 0x00) and we are in pairing mode
	if pkt.Type == 0x00 {
		if time.Now().Before(pairingUntil) {
			slog.Info("Received PairPing from while in pairing mode. Sending PairPong...", "src", pkt.SrcAddr)
			sendPairPong(pkt.SrcAddr)
		} else {
			slog.Debug("Received PairPing but pairing mode is not active.", "src", pkt.SrcAddr)
		}
		return
	}

	// Check if this is a TimeInformation request (Type 0x03)
	// Devices send this to request current time from the gateway
	if pkt.Type == 0x03 {
		slog.Info("Received TimeInformation request, sending time...", "src", pkt.SrcAddr)
		sendTimeToDevice(pkt.SrcAddr)
		return
	}

	// Message Type Reference (from FHEM 10_MAX.pm):
	// 0x00 = PairPing
	// 0x02 = Ack (contains state data)
	// 0x03 = TimeInformation (request from device or broadcast from gateway)
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
		slog.Info("Ignored message from device", "str", pkt.SrcAddr, "type", fmt.Sprintf(HexTypeFormat, pkt.Type), "payload_len", len(pkt.Payload))
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
		existing = &MaxDeviceData{
			Mode:     "manual",
			HVACMode: "heat",
		}
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

	// Check for pending verification
	if existing.Temperature > 0 && verificationMgr != nil {
		verificationMgr.CheckVerification(srcAddr, existing.Temperature)
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

	slog.Info("Decoded Data", "src", srcAddr, "data", &dataToPublish)
	publishState(srcAddr, &dataToPublish)
}

type MaxDeviceData struct {
	Temperature        float64  `json:"temperature"`
	CurrentTemperature *float64 `json:"current_temperature"`

	Mode       string `json:"mode"`
	HVACMode   string `json:"hvac_mode"`
	HVACAction string `json:"hvac_action"`
	Battery    string `json:"battery"`
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
	switch pkt.Type {
	case 0x42:
		return decodeWallThermostatControl(pkt.Payload)
	case 0x40:
		return decodeSetTemperature(pkt.Payload)
	case 0x02, 0x60, 0x70:
		return decodeThermostatStatus(pkt.Type, pkt.Payload)
	default:
		return nil
	}
}

// decodeWallThermostatControl decodes Type 0x42 (WallThermostatControl)
// Payload: [SetpointRaw] [ActualTempRaw]
func decodeWallThermostatControl(payload []byte) *MaxDeviceData {
	if len(payload) < 2 {
		return nil
	}
	data := &MaxDeviceData{}

	// Byte 0: Setpoint (Bits 0-6) / 2
	data.Temperature = float64(payload[0]&0x7F) / 2.0

	// Byte 1: Actual Temperature (Raw / 10)
	actual := float64(payload[1]) / 10.0
	data.CurrentTemperature = &actual

	return data
}

// decodeSetTemperature decodes Type 0x40 (SetTemperature)
// Payload: [TargetTemp/Mode] - Bits 0-5: Temp/2, Bits 6-7: Mode
func decodeSetTemperature(payload []byte) *MaxDeviceData {
	if len(payload) < 1 {
		return nil
	}
	data := &MaxDeviceData{}
	byte0 := payload[0]

	// Mode from bits 6-7
	modeBits := (byte0 >> 6) & 0x03
	applyModeFromBits(data, modeBits)

	// Temperature from bits 0-5
	data.Temperature = float64(byte0&0x3F) / 2.0

	// Special case: Off (4.5C in manual mode)
	if data.Mode == "manual" && data.Temperature <= 4.5 {
		data.HVACMode = "off"
	}

	return data
}

// decodeThermostatStatus decodes Type 0x02, 0x60, 0x70 (Thermostat Status)
// Contains Mode, Battery, Setpoint, and optionally Actual Temperature
func decodeThermostatStatus(pktType int, payload []byte) *MaxDeviceData {
	// For Type 0x02, skip the first byte (generic status/ack)
	if pktType == 0x02 {
		if len(payload) < 1 {
			return nil
		}
		payload = payload[1:]
	}

	if len(payload) < 3 {
		return nil
	}

	data := &MaxDeviceData{}

	// Byte 0: Mode (bits 0-1) & Battery (bit 7)
	modeBits := payload[0] & 0x03
	applyModeFromBits(data, modeBits)

	if (payload[0] & 0x80) != 0 {
		data.Battery = "low"
	} else {
		data.Battery = "ok"
	}

	// Byte 2: Setpoint Temperature
	data.Temperature = float64(payload[2]&0x7F) / 2.0

	// Special case: Off (4.5C in manual mode)
	if data.Mode == "manual" && data.Temperature <= 4.5 {
		data.HVACMode = "off"
	}

	// Bytes 3-4: Actual Temperature (if present)
	if len(payload) >= 5 {
		rawTemp := int(payload[3]&0x01)<<8 | int(payload[4])
		actualTemp := float64(rawTemp) / 10.0
		data.CurrentTemperature = &actualTemp
	}

	return data
}

// applyModeFromBits sets Mode and HVACMode based on mode bits
// Mode bits: 00=Auto, 01=Manual, 10=Vacation, 11=Boost
func applyModeFromBits(data *MaxDeviceData, modeBits byte) {
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
		"current_temperature_template": "{{ value_json.get('current_temperature') }}",
		"mode_state_topic":             fmt.Sprintf(TopicFormatState, srcAddr),
		"mode_state_template":          "{{ value_json.get('hvac_mode', 'heat') }}",
		"mode_command_topic":           fmt.Sprintf("max/%s/mode/set", srcAddr),
		"modes":                        []string{"auto", "heat", "off"},
		"min_temp":                     5,
		"max_temp":                     30,
		"temp_step":                    0.5,
		"precision":                    0.1,
		"temperature_unit":             "C",
		"action_topic":                 fmt.Sprintf(TopicFormatState, srcAddr),
		"action_template":              "{{ value_json.get('hvac_action') }}",
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
		"value_template": "{{ 'ON' if value_json.get('battery') == 'low' else 'OFF' }}",
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
		slog.Error("Failed to marshal state", "device", deviceID, "error", err)
		return
	}

	// Enhanced Logging to show Entity Mapping
	slog.Debug("MQTT PUB (Broadcast to all entities)", "topic", topic)
	slog.Debug("  -> Climate (Temp)", "temp", data.Temperature)
	if data.CurrentTemperature != nil {
		slog.Debug("  -> Climate (CurTemp)", "cur_temp", *data.CurrentTemperature)
	}
	slog.Debug("  -> Climate (HVAC)", "hvac", data.HVACMode)
	slog.Debug("  -> Internal Mode", "mode", data.Mode)

	slog.Debug("  -> Binary (Battery)", "battery", data.Battery)
	slog.Debug("  Raw Payload", "payload", string(payload))

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

	slog.Info("Received command", "src", srcAddr, "payload", payloadStr)

	temp, err := strconv.ParseFloat(payloadStr, 64)
	if err != nil {
		slog.Error("Invalid temperature format")
		return
	}

	slog.Info("Processing set temperature command", "temp", temp, "src", srcAddr)

	// Preserve current mode from state cache (default to Manual if unknown)
	modeBits := getModeFromCache(srcAddr)

	// Payload: (Temp * 2) | (Mode << 6)
	// Protocol says: (Method 0x40) Formula: (TargetTemp * 2) + (ModeBits << 6).
	tempVal := byte(temp * 2)
	payloadByte := tempVal | (modeBits << 6)

	sendCommand(srcAddr, 0x40, []byte{payloadByte}, fmt.Sprintf("Set Temp %.1f", temp))

	// Start Verification
	if verificationMgr != nil {
		verificationMgr.AddVerification(srcAddr, temp, func() {
			slog.Warn("Retrying Set Temp", "temp", temp, "src", srcAddr)
			sendCommand(srcAddr, 0x40, []byte{payloadByte}, fmt.Sprintf("Retry Set Temp %.1f", temp))
		})
	}
}

func handleMQTTPair(client mqtt.Client, msg mqtt.Message) {
	slog.Info("Activating Pairing Mode for 60 seconds")
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

	slog.Info("Received mode command", "src", srcAddr, "mode", mode)

	// Determine Mode Bits and Target Temp
	var modeBits byte
	var targetTemp float64 = 21.0 // Default fallback

	// Retrieve current state for temp preservation
	stateMutex.RLock()
	if existing, ok := stateCache[srcAddr]; ok && existing.Temperature > 0 {
		targetTemp = existing.Temperature
		slog.Debug("Preserving current temperature for mode change", "temp", targetTemp, "mode", mode)
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
		slog.Warn("Unknown mode", "mode", mode)
		return
	}

	tempVal := byte(targetTemp * 2)
	payloadByte := tempVal | (modeBits << 6)

	sendCommand(srcAddr, 0x40, []byte{payloadByte}, fmt.Sprintf("Set Mode %s", mode))

}

// nextMsgCounter increments and returns the global message counter (1-255, wrapping)
func nextMsgCounter() int {
	msgCounterMutex.Lock()
	defer msgCounterMutex.Unlock()
	msgCounter++
	if msgCounter > 255 {
		msgCounter = 1
	}
	return msgCounter
}

// getGatewayAddress returns the 3-byte gateway address from config
// Falls back to default if config is invalid
func getGatewayAddress() []byte {
	srcBytes, _ := hex.DecodeString(config.GatewayID)
	if len(srcBytes) != 3 {
		return []byte{0x12, 0x34, 0x56} // Fallback
	}
	return srcBytes
}

// sendCommand constructs and sends a MAX! message via CUL
func sendCommand(dstAddr string, typeByte byte, payload []byte, description string) {
	cnt := nextMsgCounter()
	srcBytes := getGatewayAddress()

	// Dst Address
	dstBytes, _ := hex.DecodeString(dstAddr)
	if len(dstBytes) != 3 {
		slog.Error("Invalid destination address", "addr", dstAddr)
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
		// Include counter in description for debugging
		descWithCnt := fmt.Sprintf("%s (Cnt:%d)", description, cnt)
		txMgr.Enqueue(dstAddr, cmd, descWithCnt)
	} else {
		// Fallback if not init (should not happen)
		select {
		case serialWrite <- cmd:
			slog.Info("TX", "dst", dstAddr, "desc", description, "hex", hexStr)
		default:
			slog.Error("Serial write queue full")
		}
	}
}

func sendPairPong(targetAddr string) {
	// PairPong: Type 0x01
	// To send via CUL: "Zs" + HexString
	cnt := nextMsgCounter()
	srcBytes := getGatewayAddress()
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
		slog.Info("Sent PairPong", "target", targetAddr, "cnt", cnt)
		// Schedule time sync for newly paired device (direct, not broadcast)
		slog.Info("Scheduling time sync for newly paired device", "target", targetAddr)
		go func() {
			time.Sleep(2 * time.Second)
			sendTimeToDevice(targetAddr)
		}()
	default:
		slog.Error("Serial write queue full")
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
		slog.Error("Invalid eco temperature format", "payload", payloadStr)
		return
	}

	// Validate range (4.5 - 30.5)
	if temp < 4.5 || temp > 30.5 {
		slog.Error("Eco temperature out of range (4.5-30.5)", "temp", temp)
		return
	}

	slog.Info("Setting eco temperature", "src", srcAddr, "temp", temp)

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
		slog.Error("Invalid comfort temperature format", "payload", payloadStr)
		return
	}

	// Validate range (4.5 - 30.5)
	if temp < 4.5 || temp > 30.5 {
		slog.Error("Comfort temperature out of range (4.5-30.5)", "temp", temp)
		return
	}

	slog.Info("Setting comfort temperature", "src", srcAddr, "temp", temp)

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
	slog.Info("Setting display mode", "src", srcAddr, "mode", payload, "showActual", showActual)

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
func sendTimeBroadcast() {
	payload := buildTimePayload()
	now := time.Now()
	slog.Info("Broadcasting time sync", "time", fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
		now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second()))
	sendCommand("000000", 0x03, payload, "Time Broadcast")
}

// sendTimeToDevice sends Type 0x03 TimeInformation to a specific device address
// This is used to respond to time requests from devices or to sync time after pairing
func sendTimeToDevice(dstAddr string) {
	payload := buildTimePayload()
	now := time.Now()

	slog.Info("Sending time to device", "src", dstAddr, "time", fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
		now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second()))
	sendCommand(dstAddr, 0x03, payload, fmt.Sprintf("Time to %s", dstAddr))
}

// buildTimePayload constructs the 5-byte time payload for MAX! protocol
// Payload format:
//   - Byte 0: Year (since 2000)
//   - Byte 1: Day of month
//   - Byte 2: Hour
//   - Byte 3: Minute | ((Month & 0x0C) << 4)
//   - Byte 4: Second | ((Month & 0x03) << 6)
func buildTimePayload() []byte {
	now := time.Now()
	year := byte(now.Year() - 2000)
	month := byte(now.Month())
	day := byte(now.Day())
	hour := byte(now.Hour())
	min := byte(now.Minute())
	sec := byte(now.Second())

	compressedOne := min | ((month & 0x0C) << 4)
	compressedTwo := sec | ((month & 0x03) << 6)

	return []byte{year, day, hour, compressedOne, compressedTwo}
}

// getModeFromCache retrieves the current mode bits for a device from state cache.
// Returns 0x01 (Manual) as default if device state is not cached.
func getModeFromCache(srcAddr string) byte {
	stateMutex.RLock()
	defer stateMutex.RUnlock()

	existing, ok := stateCache[srcAddr]
	if !ok {
		slog.Warn("No cached state, defaulting to Manual mode", "src", srcAddr)
		return 0x01
	}

	modeBits := modeStringToBits(existing.Mode)
	return modeBits

}

// modeStringToBits converts a mode string to MAX! protocol mode bits
// Mode bits: 00=Auto, 01=Manual, 10=Vacation, 11=Boost
func modeStringToBits(mode string) byte {
	switch mode {
	case "auto":
		return 0x00
	case "manual":
		return 0x01
	case "vacation":
		return 0x02
	case "boost":
		return 0x03
	default:
		return 0x01 // Default to Manual
	}
}

// startPeriodicTimeSync starts a goroutine that broadcasts time on startup
// and then every 1 hour to keep all paired devices synchronized.
func startPeriodicTimeSync() {
	// Initial delay to allow serial connection to be established
	time.Sleep(10 * time.Second)

	for {
		slog.Info("Sending periodic time broadcast")
		sendTimeBroadcast()
		time.Sleep(1 * time.Hour)
	}
}

// handleMQTTAssociate links two MAX! devices together (device-to-device association)
// Topic: max/<source_device>/associate
// Payload: <partner_device_id> or <partner_device_id>:<partner_type>
// Partner types: 1=HeatingThermostat, 2=HeatingThermostatPlus, 3=WallMountedThermostat, 4=ShutterContact, 5=PushButton
// If partner_type is omitted, defaults to 1 (radiator thermostat)
//
// Example: To link radiator 0A1B2C with wall thermostat 0D3E4F:
//
//	mosquitto_pub -t "max/0A1B2C/associate" -m "0D3E4F:3"  # Tell radiator about wall (type 3)
//	mosquitto_pub -t "max/0D3E4F/associate" -m "0A1B2C:1"  # Tell wall about radiator (type 1)
//
// Note: Association must be done in BOTH directions for devices to work together.
func handleMQTTAssociate(client mqtt.Client, msg mqtt.Message) {
	topicParts := strings.Split(msg.Topic(), "/")
	if len(topicParts) < 3 {
		return
	}
	srcDevice := topicParts[1]
	payload := string(msg.Payload())

	// Validate source device address (6 hex chars)
	if len(srcDevice) != 6 {
		slog.Error("Invalid source device address (must be 6 hex chars)", "src", srcDevice)
		return
	}

	// Parse payload: "<partner_id>" or "<partner_id>:<type>"
	var partnerDevice string
	var partnerType byte = 1 // Default to HeatingThermostat

	if strings.Contains(payload, ":") {
		parts := strings.Split(payload, ":")
		if len(parts) != 2 {
			slog.Error("Invalid associate payload format (expected 'device_id' or 'device_id:type')", "payload", payload)
			return
		}
		partnerDevice = strings.TrimSpace(parts[0])
		typeVal, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || typeVal < 1 || typeVal > 5 {
			slog.Error("Invalid partner type (must be 1-5)", "type", parts[1])
			return
		}
		partnerType = byte(typeVal)
	} else {
		partnerDevice = strings.TrimSpace(payload)
	}

	// Validate partner device address
	if len(partnerDevice) != 6 {
		slog.Error("Invalid partner device address (must be 6 hex chars)", "partner", partnerDevice)
		return
	}

	// Convert to uppercase for consistency
	srcDevice = strings.ToUpper(srcDevice)
	partnerDevice = strings.ToUpper(partnerDevice)

	sendAddLinkPartner(srcDevice, partnerDevice, partnerType)
}

// sendAddLinkPartner sends Type 0x20 AddLinkPartner command
// Links srcDevice to partnerDevice so they communicate with each other
// partnerType values:
//   - 1: Heating Thermostat (radiator valve)
//   - 2: Heating Thermostat Plus
//   - 3: Wall Mounted Thermostat
//   - 4: Shutter Contact
//   - 5: Push Button
//
// Payload structure (4 bytes) per FHEM reference:
//   - Bytes 0-2: Partner RF Address (3 bytes)
//   - Byte 3: Partner Type
func sendAddLinkPartner(srcDevice, partnerDevice string, partnerType byte) {
	partnerBytes, err := hex.DecodeString(partnerDevice)
	if err != nil || len(partnerBytes) != 3 {
		slog.Error("Invalid partner device address", "partner", partnerDevice)
		return
	}

	// Payload: PartnerAddr(3) + PartnerType(1) = 4 bytes
	payload := make([]byte, 4)
	payload[0] = partnerBytes[0]
	payload[1] = partnerBytes[1]
	payload[2] = partnerBytes[2]
	payload[3] = partnerType

	typeNames := map[byte]string{
		1: "HeatingThermostat",
		2: "HeatingThermostatPlus",
		3: "WallMountedThermostat",
		4: "ShutterContact",
		5: "PushButton",
	}
	typeName := typeNames[partnerType]
	if typeName == "" {
		typeName = fmt.Sprintf("Unknown(%d)", partnerType)
	}

	description := fmt.Sprintf("AddLinkPartner %s -> %s (%s)", srcDevice, partnerDevice, typeName)
	sendCommand(srcDevice, 0x20, payload, description)
}
