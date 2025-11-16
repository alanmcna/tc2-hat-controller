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
	predictionLockoutNodeRegister 	  int = 5
	predictionLockoutMinutesDefault   int64 = 30 // default 30mins.
	batteryLockoutHoursNodeRegister   int = 12
	batteryLockoutMinutesNodeRegister int = 13
	batteryLockoutMinutesDefault 	  int64 = 180   // default 180mins (3 hours).
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

var atesLastPrediction = ATESLLastPrediction{ Lockout: predictionLockoutMinutesDefault }
var atesLastBattery = ATESLLastBattery{ Lockout: batteryLockoutMinutesDefault }

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
	l.Voltage = b.Voltage  // Remember the voltage reading
	l.When = time.Now()    // Remember when we detected it

	// Now let's check the event lockout
	l.Lockout = getBatteryEventLockout(a.BaudRate)

	return nil
}

func (a ATESLMessenger) processTrackingEvent(t trackingEvent, l *ATESLLastPrediction) error {

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
	if _, found := a.TrapSpecies["any"]; found {

		// We can do without false-positives, not quite any :)
		if t.What == "false-positive" {
			return nil
		}

		target = true
		targetConfidence = a.TrapSpecies["any"]

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

		// TODO - send the thumbnail - for now just log the dimensions
		//tn := getThumbnail(t.ClipId, t.TrackId)
		//log.Infof("Thumbnail is: %d×%d", len(tn), len(tn[0]))

		// Now let's check the event lockout
		l.Lockout = getPredictionEventLockout(a.BaudRate)
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

		// response, err := serialhelper.SerialSendReceive(1, gpio.High, gpio.Low, 10*time.Second, payload, baudRate)
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

	// O^K now send the AT command
	payload := append([]byte(command), byte('\r'))
	log.Infof("Sending AT command: %s", command)

	response, err = serialhelper.SerialSendReceive(1, gpio.High, gpio.Low, 5*time.Second, payload, baudRate)
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

func getRegisteryData(baudRate int, reg int) int64 {
	regCmd := "m00"

	// Currently limited to the first 'page' of registery data (m00)
	cmd := append([]byte("AT+XCMD=" + regCmd), calcCRC16([]byte(regCmd))...)
	log.Infof("get reg %d, data via command %v", reg, cmd)

	response, _ := sendATCommand(string(cmd), baudRate)

	// Let's clean-up the output - trim any unwanted charaters
	if idx := bytes.Index(response, []byte(regCmd + "\r\n\r\n")); idx != -1 {
		response = response[idx:]
	} else {
		// fallback: not found, just log and continue
		log.Warnf("Registry command marker [%v] not found; keeping full response", regCmd)
	}

	col := reg % 10
	row := 0
	if reg > 10 {
		row = reg / (reg - (reg % 10))
	}

	// w05..
	// response.. \xa5\xfc\r\nm00\r\n\r\n00: ff ff ff ff ff 02
	// First, second, third or fourth row of node register block - 00:
	seq := fmt.Sprintf("%02x:", row*16)
	pos := 0

	log.Debugf("Searching for %v in response", seq)
	for i := 0; i <= len(response)-3; i++ {
		if response[i] == seq[0] && response[i+1] == seq[1] && response[i+2] == seq[2] {
			log.Debugf("Found %v - position: %d", seq, i)
			pos = i + 3 + col*3 + 1 // aka it's the nth element + drop the leading space
			break
		}
	}

	hexstr := string(response[pos : pos+2])
	reg_value, err := strconv.ParseInt(hexstr, 16, 64)
	log.Debugf("Converted %v to int value: %d", hexstr, reg_value)

	if err != nil {
		log.Errorf("parseInt error: %v", err)
		reg_value = 0
	} else if reg_value == 255 {
		reg_value = 0
		log.Debugf("Reg value is 255 (FF) re-setting int value: %d", reg_value)
	}
	log.Infof("Reg value = %d", reg_value)

	return reg_value
}

/*

   Prediction event lockout mins
   Time in minutes to have an prediction event lockout; default 30mins.
   Read the 05 node registery to get the value

   2min = 'w0502’
   10min = 'w050a’
   30min = 'w051e’

*/

func getPredictionEventLockout(baudRate int) int64 {

	lockout_minutes := getRegisteryData(baudRate, predictionLockoutNodeRegister)

	if lockout_minutes == 0 {
		lockout_minutes = predictionLockoutMinutesDefault
		log.Infof("Prediction lockout time not set - using default (%d)", predictionLockoutMinutesDefault)
	}

	log.Infof("Prediction lockout time = %d (mins)", lockout_minutes)
	return lockout_minutes
}

/*

   Battery event lockout mins
   Time in minutes to have an battery event lockout; default 180mins (3 hours).
   Read the 12 (hrs) + 13 (mins) node registery to get the value

   3hours = 'w1203’
   30min = 'w131e’

*/
func getBatteryEventLockout(baudRate int) int64 {

	hours := getRegisteryData(baudRate, batteryLockoutHoursNodeRegister)
	mins  := getRegisteryData(baudRate, batteryLockoutMinutesNodeRegister)

	battery_lockout_minutes := hours * 60 + mins
	if battery_lockout_minutes <= 0 {
		log.Infof("Battery lockout time not set - using default (%d)", batteryLockoutMinutesDefault)
	    battery_lockout_minutes = batteryLockoutMinutesDefault
	}

	log.Infof("Battery lockout time = %d (mins)", battery_lockout_minutes)
	return battery_lockout_minutes
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
