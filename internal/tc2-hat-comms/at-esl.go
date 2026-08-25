package comms

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TheCacophonyProject/tc2-hat-controller/serialhelper"
	"periph.io/x/conn/v3/gpio"
)

var (
	// Node register addresses (hex), matching cmdpro wXX / mXX dump lines.
	// 0x05 = EVENT_LOCKOUT_MINS (prediction).
	// 0x12/0x13 = HOURS/MINS_PER_INST_READING — battery is an instrument reading;
	// these set how often we report those readings.
	predictionLockoutNodeRegister     int   = 0x05
	predictionLockoutMinutesDefault   int64 = 1 // default 1min.

	batteryLockoutHoursNodeRegister   int   = 0x12
	batteryLockoutMinutesNodeRegister int   = 0x13
	batteryLockoutMinutesDefault      int64 = 180 // default 180mins (3 hours).

	// Pause after AT wake before the real command so the node can leave the wake
	// O^K and accept AT+XCMD / AT+CAM.
	atPostWakeSettle = 50 * time.Millisecond
	atPostRegTimeout = 2000 * time.Millisecond
)

type ATESLMessenger struct {
	BaudRate    int
	TrapSpecies map[string]int32
}

type ATESLLastPrediction struct {
	What    string
	When    time.Time
	Lockout int64
}

type ATESLLastBattery struct {
	Voltage float64
	When    time.Time
	Lockout int64
}

var atesLastPrediction = ATESLLastPrediction{Lockout: predictionLockoutMinutesDefault}
var atesLastBattery = ATESLLastBattery{Lockout: batteryLockoutMinutesDefault}

func processATESL(config *CommsConfig, testClassification *TestClassification, eventChannel chan event) error {
	messenger := ATESLMessenger{
		config.BaudRate,
		config.TrapSpecies,
	}

	if testClassification != nil {
		log.Println("Sending a test classification for AT ESL")
		testTrackingEvent := trackingEvent{
			Species: map[string]int32{
				testClassification.Animal: testClassification.Confidence,
			},
			What:       testClassification.Animal,
			Confidence: testClassification.Confidence,
		}
		err := messenger.processTrackingEvent(testTrackingEvent, &atesLastPrediction)
		if err != nil {
			log.Error("Error processing test tracking event:", err)
		}
		return nil
	}

	for {
		log.Debug("Waiting")
		e := <-eventChannel

		// Process the event, depending on the type
		switch v := e.(type) {
		case trackingEvent:
			err := messenger.processTrackingEvent(v, &atesLastPrediction)
			if err != nil {
				log.Error("Error sending classification:", err)
			}
		case batteryEvent:
			err := messenger.processBatteryEvent(v, &atesLastBattery)
			if err != nil {
				log.Error("Error sending battery reading:", err)
			}
		default:
			log.Infof("No at-esl handler for event: %v", v)
		}
	}
}

func (a ATESLMessenger) processBatteryEvent(b batteryEvent, l *ATESLLastBattery) error {
	log.Infof("Processing battery event: %+v", b)

	lastBattery := time.Since(l.When).Minutes()
	log.Infof("Last battery reading %v minutes ago (lockout %v at %v)", lastBattery, l.Lockout, l.When)

	// It's a battery reading, but within the event lockout - skip notifying
	if lastBattery < float64(l.Lockout) {
		log.Infof("Skipping battery of %v - within event lockout %v minutes (%d)", b.Voltage, lastBattery, l.Lockout)
		return nil
	}

	// AT command, sending a battery reading as hundredths of a volt
	atCmd := fmt.Sprintf("AT+CAMBAT=%d", int32(b.Voltage*100))

	_, err := sendATCommand(atCmd, a.BaudRate)
	if err != nil {
		log.Error("Error sending battery reading:", err)
		return err
	}
	l.Voltage = b.Voltage // Remember the voltage reading
	l.When = time.Now()   // Remember when we detected it

	// Re-query lockout (hub may have changed w12/w13); keep last-good on failure.
	l.Lockout = getBatteryEventLockout(a.BaudRate, l.Lockout)

	return nil
}

func (a ATESLMessenger) processTrackingEvent(t trackingEvent, l *ATESLLastPrediction) error {

	// TODO: tracking events can be buffered - so the clip age make be significant
	// if a tacking event was older than our lockout - perhaps we should send it on
	//
	// The ESL API also assumes all events are 'now' so we also need to extend that to be able to provide
	// the age in seconds/minutes if the age is over some reasonable threshold - maybe 2-5mins.

	lastPrediction := time.Since(l.When).Minutes()
	log.Debugf("Last prediction %v minutes ago (lockout %v at %v)", lastPrediction, l.Lockout, l.When)

	// It's a prediction frame, but within the event lockout - skip notifying
	if lastPrediction < float64(l.Lockout) {
		log.Debugf("Skipping prediction of %v (%v), ClipId %d, TrackId %d - within event lockout %v minutes (%d)",
			t.What, t.Confidence, t.ClipId, t.TrackId, lastPrediction, l.Lockout)
		return nil
	}

	log.Debugf("Processing tracking prediction (frame) event What: %v, Confidence: %v, ClipId %d, TrackId %d, Region: %v, Frame: %v",
		t.What, t.Confidence, t.ClipId, t.TrackId, t.Region, t.Frame)

	var targetConfidence int32 = 0
	target := false
	// We've found an object - is it a target (trapable) species?
	if len(a.TrapSpecies) == 0 { // null is wild

		// We can do without false-positives, not quite any :)
		if t.What == "false-positive" {
			return nil
		}

		target = true
		targetConfidence = 50 // limit to 50% to avoid too much noise

		// Special handler for any - with confidence setting
	} else if _, found := a.TrapSpecies["any"]; found {

		// We can do without false-positives, not quite any :)
		if t.What == "false-positive" {
			return nil
		}

		target = true
		targetConfidence = a.TrapSpecies["any"]

		// If we have specific species let's check for specific confidence levels oer species
	} else if _, found := a.TrapSpecies[t.What]; found {
		target = true
		targetConfidence = a.TrapSpecies[t.What]
	}

	if target && t.Confidence >= targetConfidence {
		log.Infof("Track prediction of a target species with confidence: %s,%d", t.What, t.Confidence)

		atCmd := fmt.Sprintf("AT+CAM=%s,%d", t.What, t.Confidence)
		l.What = t.What     // Remember the object
		l.When = time.Now() // Remember when we detected it

		_, err := sendATCommand(atCmd, a.BaudRate)
		if err != nil {
			log.Error("Error sending classification:", err)
			return err
		}

		// TODO - disable logging the thumbnail as it may not exist (if the event is old)
		//tn := getThumbnail(t.ClipId, t.TrackId)
		//log.Infof("Thumbnail is: %d×%d", len(tn), len(tn[0]))

		// Re-query lockout (hub may have changed w05); keep last-good on failure.
		l.Lockout = getPredictionEventLockout(a.BaudRate, l.Lockout)
	}

	return nil
}

func sendATWakeUp(baudRate int) error {

	log.Debugf("Wake up serial device.")
	payload := []byte("\r\rAT\r")

	retries := 0 // Don't retry (for now)
	attempt := 1

	for {
		log.Infof("Sending AT wakeup command[%d]: %q", attempt, string(payload))

		err := serialhelper.SerialSend(1, gpio.High, gpio.Low, 10*time.Second, payload, baudRate)
		attempt = attempt + 1

		if err != nil {
			return fmt.Errorf("serial send error: %w", err)
		}
		if attempt > retries {
			return nil
			// Don't error - just carry on
			// return fmt.Errorf("Failed to wake up serial device after %d attempts!", attempt)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sendATCommand(command string, baudRate int) ([]byte, error) {

	response := []byte("")

	// Test mode :)
	if baudRate == 0 {
		log.Infof("Baud rate 0 - assuming test mode, no serial device.")
		return response, nil
	}

	// Try and wake up the serial receiver first
	err := sendATWakeUp(baudRate)
	if err != nil {
		return response, fmt.Errorf("could not wake serial receiver: %w", err)
	}

	// Give the node time to finish the wake O^K and be ready for the next AT command.
	time.Sleep(atPostWakeSettle)

	// O^K now send the AT command. Drain clears wake leftover; read until O^K/E^RROR
	// so multi-line responses (e.g. m00 register dump) are kept intact.
	payload := append([]byte(command), byte('\r'))
	log.Infof("Sending AT command: %q", command)

	response, err = serialhelper.SerialSendReceiveUntil(
		1, gpio.High, gpio.Low, 0*time.Second, payload, baudRate, atPostRegTimeout,
		[]byte("O^K"), []byte("E^RROR"),
	)
	if err != nil {
		return response, fmt.Errorf("serial send receive error: %w", err)
	}

	log.Debugf("Raw AT response: %q, %v", string(response), response)

	// Read back response and check for OK or ERROR
	scanner := bufio.NewScanner(bytes.NewReader(response))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "E^RROR" {
			return response, fmt.Errorf("device returned ERROR")
		}
	}

	if err := scanner.Err(); err != nil {
		return response, fmt.Errorf("scanner error: %w", err)
	}

	return response, nil
}

// parseRegistryByte extracts one register byte from an mXX hex dump.
// reg is the node register address (e.g. 0x05, 0x12). Value 0xff is treated as unset (0).
//
// Rows are matched on their absolute label ("10:" holds 0x10-0x1f). Dumps are
// requested row-aligned, so if the node labels rows relative to the request
// instead the first dump row is still the row we asked for, and is used as a
// fallback.
func parseRegistryByte(response []byte, reg int) (int64, error) {
	if len(response) == 0 {
		return 0, fmt.Errorf("empty registry response")
	}

	addr := reg & 0xff
	col := addr & 0x0f
	prefix := fmt.Sprintf("%02x:", addr&0xf0)

	var firstRow []string
	scanner := bufio.NewScanner(bytes.NewReader(response))
	for scanner.Scan() {
		lower := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if !isRegistryRow(lower) {
			continue
		}
		fields := strings.Fields(lower)
		if firstRow == nil {
			firstRow = fields
		}
		if strings.HasPrefix(lower, prefix) {
			return registryColumn(fields, addr, col)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if firstRow != nil {
		// Only reachable if the node relabels rows; warn since it also looks like
		// a node that ignored the requested row and dumped from 0x00 instead.
		log.Warnf("Registry row %s absent; reading column %d of first dump row %q", prefix, col, firstRow[0])
		return registryColumn(firstRow, addr, col)
	}
	return 0, fmt.Errorf("registry row %s not found in response", prefix)
}

// isRegistryRow reports whether line starts with a two hex digit row label such
// as "10:", so echoed commands and status lines are skipped.
func isRegistryRow(line string) bool {
	if len(line) < 3 || line[2] != ':' {
		return false
	}
	_, err := strconv.ParseUint(line[:2], 16, 8)
	return err == nil
}

// registryColumn reads column col of a dump row, where fields[0] is the row
// label. addr is only used for logging.
func registryColumn(fields []string, addr, col int) (int64, error) {
	if len(fields) < col+2 {
		return 0, fmt.Errorf("registry row %q too short for column %d", strings.Join(fields, " "), col)
	}
	hexstr := fields[col+1]
	if len(hexstr) != 2 {
		return 0, fmt.Errorf("invalid hex byte %q in row %q", hexstr, strings.Join(fields, " "))
	}
	regValue, err := strconv.ParseInt(hexstr, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parseInt %q: %w", hexstr, err)
	}
	if regValue == 255 {
		log.Infof("Reg 0x%02x = 0 (FF unset) from %s", addr, formatRegistryRow(fields, col))
		return 0, nil
	}
	log.Infof("Reg 0x%02x = %d (0x%02x) from %s", addr, regValue, uint8(regValue), formatRegistryRow(fields, col))
	return regValue, nil
}

// formatRegistryRow rebuilds a dump line with the selected column in [brackets].
func formatRegistryRow(fields []string, col int) string {
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fields[0])
	for i, hexstr := range fields[1:] {
		b.WriteByte(' ')
		if i == col {
			b.WriteByte('[')
			b.WriteString(hexstr)
			b.WriteByte(']')
		} else {
			b.WriteString(hexstr)
		}
	}
	return b.String()
}

// registryDumpCommand returns the mXX command that dumps the 16-byte row
// containing reg. The node wants two digits but ignores the low one, so the row
// is selected by the high nibble (0x12 → "m10").
func registryDumpCommand(reg int) string {
	return fmt.Sprintf("m%02x", reg&0xf0)
}

const (
	xcmdFlag = 0x7e
	xcmdESC  = 0x1b
)

// byteStuffXCMD prefixes CH_FLAG (0x7e) before flag/CR/LF/ESC so the node's
// AT parser does not treat those bytes as end-of-frame. Matches serial_node.py
// and hub-and-node atproc.c / rfid.c destuff (flag_escaped).
func byteStuffXCMD(msg []byte) []byte {
	out := make([]byte, 0, len(msg)+4)
	for _, c := range msg {
		if c == xcmdFlag || c == '\r' || c == '\n' || c == xcmdESC {
			out = append(out, xcmdFlag)
		}
		out = append(out, c)
	}
	return out
}

// encodeATXCMD builds AT+XCMD=<cmd><crc16>, stuffing only the command+CRC.
// CRC is computed on the unstuffed command. The AT line terminator is not stuffed;
// sendATCommand appends that CR afterwards.
func encodeATXCMD(cmd string) []byte {
	body := []byte(cmd)
	body = append(body, calcCRC16(body)...)
	return append([]byte("AT+XCMD="), byteStuffXCMD(body)...)
}

// fetchRegistryDump requests the register row containing reg. The node prints
// ~80 bytes from the row start, so one dump covers all 16 registers in the row.
func fetchRegistryDump(baudRate int, reg int) ([]byte, error) {
	regCmd := registryDumpCommand(reg)
	cmd := encodeATXCMD(regCmd)
	log.Infof("fetch registry dump for 0x%02x via command %s (%q)", reg&0xff, regCmd, cmd)

	response, err := sendATCommand(string(cmd), baudRate)
	if err != nil {
		return response, err
	}
	if len(response) == 0 {
		return response, fmt.Errorf("empty response to %s", regCmd)
	}
	return response, nil
}

func getRegisteryData(baudRate int, reg int) (int64, bool) {
	response, err := fetchRegistryDump(baudRate, reg)
	if err != nil {
		log.Warnf("registry read failed for 0x%02x: %v", reg, err)
		return 0, false
	}

	regValue, err := parseRegistryByte(response, reg)
	if err != nil {
		log.Warnf("registry parse failed for 0x%02x: %v (response %q)", reg, err, string(response))
		return 0, false
	}
	return regValue, true
}

/*

   Prediction event lockout mins
   Time in minutes to have an prediction event lockout; default 1min.
   Read the 05 node registery to get the value

   2min = 'w0502’
   10min = 'w050a’
   30min = 'w051e’

*/

func getPredictionEventLockout(baudRate int, current int64) int64 {
	lockoutMinutes, ok := getRegisteryData(baudRate, predictionLockoutNodeRegister)
	if !ok {
		log.Warnf("Prediction lockout read failed - retaining %d", current)
		return current
	}
	if lockoutMinutes == 0 {
		lockoutMinutes = predictionLockoutMinutesDefault
		log.Infof("Prediction lockout time not set - using default (%d)", predictionLockoutMinutesDefault)
	}

	log.Infof("Prediction lockout time = %d (mins)", lockoutMinutes)
	return lockoutMinutes
}

/*
Battery / instrument reading interval.
Default 180mins (3 hours). Read 0x12 (hrs) + 0x13 (mins) = HOURS/MINS_PER_INST_READING.
Battery voltage is reported as an instrument reading, so this interval gates how often we send it.

3hours = 'w1203’
30min = 'w131e’
*/
func getBatteryEventLockout(baudRate int, current int64) int64 {
	// 0x12 and 0x13 share row 1, so a single m10 dump covers both.
	response, err := fetchRegistryDump(baudRate, batteryLockoutHoursNodeRegister)
	if err != nil {
		log.Warnf("Battery lockout registry read failed - retaining %d: %v", current, err)
		return current
	}

	hours, errH := parseRegistryByte(response, batteryLockoutHoursNodeRegister)
	mins, errM := parseRegistryByte(response, batteryLockoutMinutesNodeRegister)
	if errH != nil || errM != nil {
		log.Warnf("Battery lockout parse failed - retaining %d (hours=%v mins=%v)", current, errH, errM)
		return current
	}

	batteryLockoutMinutes := hours*60 + mins
	if batteryLockoutMinutes <= 0 {
		log.Infof("Battery lockout time not set - using default (%d)", batteryLockoutMinutesDefault)
		batteryLockoutMinutes = batteryLockoutMinutesDefault
	}

	log.Infof("Battery lockout time = %d (mins)", batteryLockoutMinutes)
	return batteryLockoutMinutes
}

func feedCRC16(crc uint16, dat byte) uint16 {
	for i := 0; i < 8; i++ {
		bit0 := (crc ^ uint16(dat)) & 1
		crc >>= 1
		if bit0 == 1 {
			crc ^= 0x8408
		}
		dat >>= 1
	}
	return crc
}

func calcCRC16(msg []byte) []byte {
	crc := uint16(0xFFFF)
	for _, b := range msg {
		crc = feedCRC16(crc, b)
	}
	return []byte{byte(crc & 0xFF), byte(crc >> 8)}
}
