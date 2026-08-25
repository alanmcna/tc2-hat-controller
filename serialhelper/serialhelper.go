package serialhelper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TheCacophonyProject/go-utils/logging"
	"github.com/tarm/serial"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

var log = logging.NewLogger("info")

const cmdlineFile = "/boot/firmware/cmdline.txt"

type SerialUnavailableError struct {
	msg string
}

func (e *SerialUnavailableError) Error() string {
	return e.msg
}

func NewSerialUnavailableError(msg string) error {
	return &SerialUnavailableError{msg: msg}
}

func SerialInUseFromTerminal() bool {
	b, err := os.ReadFile(cmdlineFile)
	if err != nil {
		log.Printf("Error when reading %s: %s", cmdlineFile, err)
		return false
	}
	return strings.Contains(string(b), "console=serial0")
}

// GetSerial will try to get a file lock on the serial port.
// If the file lock can be acquired, it will return the serial file and change mul0 and mul1 to the new values.
// defer ReleaseSerial(serialFile) should be called to release the lock and close the serial file.
func GetSerial(retries int, mul0, mul1 gpio.Level, wait time.Duration) (*os.File, error) {
	// Check if serial is in use by the terminal console.
	if SerialInUseFromTerminal() {
		return nil, NewSerialUnavailableError("serial is in use by the terminal console")
	}

	// Open serial file.
	serialFile, err := os.OpenFile("/dev/serial0", os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	lockAcquired := false
	defer func() {
		if !lockAcquired {
			serialFile.Close()
		}
	}()

	// Try to get a lock on the serial file.
	i := retries
	for {
		err = syscall.Flock(int(serialFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			lockAcquired = true
			break
		}

		if errno, ok := err.(syscall.Errno); ok && errno == syscall.EWOULDBLOCK {
			log.Printf("Serial port is locked. Checking locking process...")
			process, err := getLockingProcess("/dev/serial0")
			if err != nil {
				log.Printf("Error checking locking process: %v", err)
			} else if process == "" {
				log.Printf("No active process found holding the lock. Forcing lock acquisition...")
				// Force unlock by attempting to close and reopen the file
				err := syscall.Flock(int(serialFile.Fd()), syscall.LOCK_UN)
				if err != nil {
					return nil, fmt.Errorf("failed to force unlock: %v", err)
				}
				continue // Retry lock acquisition
			} else {
				log.Printf("Serial port is locked by process: %s", process)
			}

			if i > 0 {
				log.Printf("Serial port is locked by another process. Retrying %d more times in %d seconds...", i, wait/time.Second)
				time.Sleep(wait)
				i--
			} else {
				return nil, NewSerialUnavailableError("failed to get lock on serial, might be in use by other process")
			}
		} else {
			return nil, err
		}
	}

	// Configure GPIO pins for the UART multiplexer as requested.
	if _, err := host.Init(); err != nil {
		log.Fatal(err)
	}
	mul0Pin := gpioreg.ByName("GPIO6")
	if mul0Pin == nil {
		return nil, fmt.Errorf("failed to init GPIO6 pin")
	}
	if err := mul0Pin.Out(mul0); err != nil {
		return nil, err
	}
	mul1Pin := gpioreg.ByName("GPIO12")
	if mul1Pin == nil {
		return nil, fmt.Errorf("failed to init GPIO12 pin")
	}
	if err := mul1Pin.Out(mul1); err != nil {
		return nil, err
	}

	out, err := exec.Command("raspi-gpio", "set", "14", "a0").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to set GPIO14 to a0(UART): %v, output: %s", err, out)
	}
	out, err = exec.Command("raspi-gpio", "set", "15", "a0").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to set GPIO15 to a0(UART): %v, output: %s", err, out)
	}

	return serialFile, nil
}

func getLockingProcess(serialPath string) (string, error) {
	// Run `fuser` to check which process is using the file
	cmd := exec.Command("fuser", serialPath)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			// Exit code 1 from `fuser` means no process is using the file
			return "", nil
		}
		return "", fmt.Errorf("failed to execute fuser: %v", err)
	}
	return output.String(), nil
}

func ReleaseSerial(serialFile *os.File) error {
	fd := int(serialFile.Fd())
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	return serialFile.Close()
}

// QuiesceSerialMux drives the HAT UART mux to the ATtiny/programming select
// (GPIO6 low, GPIO12 low). That disconnects accessory UART paths (trap / RFID /
// ESL) from the Pi while the SoC is still powered — intended for the ATtiny
// shutdown sequence before EN_5V is cut.
//
// Unlike floating the mux/UART pins, this keeps a driven select so we do not
// leave the accessory bus on a floating Pi TX (which can chatter a live peer).
// GPIO14/15 are left alone; they go high-Z naturally when the rail drops.
func QuiesceSerialMux() error {
	if _, err := host.Init(); err != nil {
		return fmt.Errorf("gpio host init: %w", err)
	}
	mul0Pin := gpioreg.ByName("GPIO6")
	if mul0Pin == nil {
		return fmt.Errorf("failed to init GPIO6 pin")
	}
	if err := mul0Pin.Out(gpio.Low); err != nil {
		return fmt.Errorf("GPIO6 low: %w", err)
	}
	mul1Pin := gpioreg.ByName("GPIO12")
	if mul1Pin == nil {
		return fmt.Errorf("failed to init GPIO12 pin")
	}
	if err := mul1Pin.Out(gpio.Low); err != nil {
		return fmt.Errorf("GPIO12 low: %w", err)
	}
	return nil
}

func SerialSendReceive(retries int, mul0, mul1 gpio.Level, wait time.Duration, data []byte, baud int) ([]byte, error) {
	serialFile, err := GetSerial(retries, mul0, mul1, wait)
	if err != nil {
		return nil, err
	}
	defer ReleaseSerial(serialFile)
	c := &serial.Config{Name: "/dev/serial0", Baud: baud, ReadTimeout: time.Second * 5}
	serialPort, err := serial.OpenPort(c)
	if err != nil {
		return nil, err
	}
	defer serialPort.Close()

	start := time.Now()
	// add a newline at and of data if it is not there already
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	n, err := serialPort.Write(data)
	if err != nil {
		return nil, err
	}

	if n != len(data) {
		return nil, fmt.Errorf("wrote %d bytes, expected %d", n, len(data))
	}

	var response []byte
	var responseTime time.Time
	firstBits := true
	buf := make([]byte, 1)
	for {
		n, err = serialPort.Read(buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if firstBits {
			responseTime = time.Now()
			firstBits = false
		}
		if buf[0] == '\n' {
			break
		}
		response = append(response, buf[0])
	}
	log.Infof("Sent message at %s", start.Format("15:04:05.999"))
	log.Infof("Received message at %s", responseTime.Format("15:04:05.999"))
	log.Debugf("Received %d bytes", len(response))
	log.Debugf("Response time: %s", responseTime)
	return response, nil
}

const (
	defaultATReceiveTimeout = 3 * time.Second
	defaultATDrainTimeout   = 200 * time.Millisecond
	maxATResponseBytes      = 4096
)

// SerialSendReceiveUntil drains pending RX, writes data, then reads until the
// response contains one of endMarkers (e.g. "O^K", "E^RROR"), the overall
// timeout elapses after the write, or maxATResponseBytes is reached.
// Unlike SerialSendReceive it keeps multi-line payloads (needed for AT+XCMD=m00).
// Empty endMarkers means read until timeout/idle only.
func SerialSendReceiveUntil(retries int, mul0, mul1 gpio.Level, wait time.Duration, data []byte, baud int, timeout time.Duration, endMarkers ...[]byte) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultATReceiveTimeout
	}

	serialFile, err := GetSerial(retries, mul0, mul1, wait)
	if err != nil {
		return nil, err
	}
	defer ReleaseSerial(serialFile)

	// Short ReadTimeout so drain/idle detection does not block for seconds per read.
	c := &serial.Config{Name: "/dev/serial0", Baud: baud, ReadTimeout: 100 * time.Millisecond}
	serialPort, err := serial.OpenPort(c)
	if err != nil {
		return nil, err
	}
	defer serialPort.Close()

	drained := drainSerial(serialPort, defaultATDrainTimeout)
	if drained > 0 {
		log.Debugf("Drained %d pending serial bytes before write", drained)
	}

	start := time.Now()
	if len(data) == 0 || (data[len(data)-1] != '\n' && data[len(data)-1] != '\r') {
		data = append(data, '\n')
	}
	n, err := serialPort.Write(data)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, fmt.Errorf("wrote %d bytes, expected %d", n, len(data))
	}

	response, responseTime, err := readUntilMarkers(serialPort, timeout, endMarkers...)

	log.Infof("Sent message at %s", start.Format("15:04:05.999"))
	if !responseTime.IsZero() {
		log.Infof("Received message at %s", responseTime.Format("15:04:05.999"))
	}
	log.Debugf("Received %d bytes (until markers/timeout)", len(response))
	return response, err
}

// serialReader is the read side of a serial port, so the framing logic can be
// unit tested without hardware.
type serialReader interface {
	Read(p []byte) (int, error)
}

// isReadTimeout reports whether a read error just means "no bytes this round".
// tarm/serial sets VMIN=0 when ReadTimeout > 0, so an expired VTIME read
// returns 0 bytes, which os.File turns into io.EOF. Treating that as fatal
// aborts multi-line AT responses that have gaps between lines.
func isReadTimeout(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded)
}

// readUntilMarkers accumulates bytes until one of endMarkers appears, the
// stream stays idle after data has arrived, or timeout elapses. It returns the
// bytes read plus the time the first byte arrived (zero if nothing arrived).
func readUntilMarkers(port serialReader, timeout time.Duration, endMarkers ...[]byte) ([]byte, time.Time, error) {
	deadline := time.Now().Add(timeout)
	var response []byte
	var responseTime time.Time
	buf := make([]byte, 64)
	idleRounds := 0
	const idleRoundsToStop = 3 // ~300ms quiet after some data, or after markers missed

	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil && !isReadTimeout(err) {
			return response, responseTime, err
		}
		if n == 0 {
			if len(response) > 0 {
				idleRounds++
				if idleRounds >= idleRoundsToStop &&
					(len(endMarkers) == 0 || containsAny(response, endMarkers...)) {
					break
				}
			}
			continue
		}
		idleRounds = 0
		if responseTime.IsZero() {
			responseTime = time.Now()
		}
		response = append(response, buf[:n]...)
		if len(response) > maxATResponseBytes {
			return response, responseTime, fmt.Errorf("serial response exceeded %d bytes", maxATResponseBytes)
		}
		if len(endMarkers) > 0 && containsAny(response, endMarkers...) {
			// Brief extra read window to catch trailing CR/LF after the marker.
			extraDeadline := time.Now().Add(150 * time.Millisecond)
			for time.Now().Before(extraDeadline) {
				n, err = port.Read(buf)
				if err != nil || n == 0 {
					break
				}
				response = append(response, buf[:n]...)
				if len(response) > maxATResponseBytes {
					break
				}
			}
			break
		}
	}

	return response, responseTime, nil
}

func drainSerial(port serialReader, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 256)
	total := 0
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil && !isReadTimeout(err) {
			return total
		}
		if n == 0 {
			// One empty read is enough once the FIFO looks quiet.
			return total
		}
		total += n
	}
	return total
}

func containsAny(data []byte, markers ...[]byte) bool {
	for _, m := range markers {
		if len(m) > 0 && bytes.Contains(data, m) {
			return true
		}
	}
	return false
}

func SerialSend(retries int, mul0, mul1 gpio.Level, wait time.Duration, data []byte, baud int) error {
	start := time.Now()

	serialFile, err := GetSerial(retries, mul0, mul1, wait)
	if err != nil {
		return err
	}
	defer ReleaseSerial(serialFile)

	elapsed := time.Since(start)
	log.Print("Serial lock took ", elapsed)

	start = time.Now()
	c := &serial.Config{Name: "/dev/serial0", Baud: baud, ReadTimeout: time.Second * 5}
	serialPort, err := serial.OpenPort(c)
	if err != nil {
		return err
	}
	defer serialPort.Close()
	elapsed = time.Since(start)
	log.Println("Serial open took ", elapsed)

	start = time.Now()
	n, err := serialPort.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("wrote %d bytes, expected %d", n, len(data))
	}
	elapsed = time.Since(start)
	log.Print("Serial send took ", elapsed)

	return nil
}

// SerialPort represents a persistent, open serial connection with a background line reader.
type SerialPort struct {
	writeMu sync.Mutex
	port    *serial.Port
	file    *os.File
	Lines   chan []byte
	done    chan struct{}
}

// OpenSerial opens the serial port persistently and starts a background line reader goroutine.
func OpenSerial(mul0, mul1 gpio.Level, baud int) (*SerialPort, error) {
	file, err := GetSerial(3, mul0, mul1, time.Second)
	if err != nil {
		return nil, err
	}
	c := &serial.Config{Name: "/dev/serial0", Baud: baud, ReadTimeout: 100 * time.Millisecond}
	port, err := serial.OpenPort(c)
	if err != nil {
		if rerr := ReleaseSerial(file); rerr != nil {
			log.Printf("Failed to release serial: %v", rerr)
		}
		return nil, err
	}
	sp := &SerialPort{
		port:  port,
		file:  file,
		Lines: make(chan []byte, 16),
		done:  make(chan struct{}),
	}
	go sp.readLoop()
	return sp, nil
}

// readLoop continuously reads lines from the serial port and sends them to Lines.
// It exits when Close is called.
func (s *SerialPort) readLoop() {
	defer close(s.Lines)
	buf := make([]byte, 1)
	var line []byte
	for {
		select {
		case <-s.done:
			return
		default:
		}
		n, err := s.port.Read(buf)
		if err != nil || n == 0 {
			continue
		}
		if buf[0] == '\n' {
			if len(line) > 0 {
				msg := make([]byte, len(line))
				copy(msg, line)
				select {
				case s.Lines <- msg:
				case <-s.done:
					return
				}
				line = line[:0]
			}
		} else {
			line = append(line, buf[0])
		}
	}
}

// Write sends data over the serial port. Appends a newline if not already present.
func (s *SerialPort) Write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	n, err := s.port.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("wrote %d bytes, expected %d", n, len(data))
	}
	return nil
}

// Close stops the background reader and releases the serial port.
func (s *SerialPort) Close() error {
	close(s.done)
	s.port.Close()
	return ReleaseSerial(s.file)
}
