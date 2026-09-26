// Package repl implements the interactive shell ploom-cli drops into once a
// device is connected and its GATT table has been discovered.
package repl

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
)

// Session holds the live device connection and discovered GATT table that
// REPL commands operate on.
type Session struct {
	device     bledevice.Device
	deviceName string
	address    string
	services   []bledevice.Service
	flat       []bledevice.FlatChar
	notifying  map[int]bool
}

// New builds a REPL session bound to an already-connected device.
func New(device bledevice.Device, deviceName, address string, services []bledevice.Service) *Session {
	return &Session{
		device:     device,
		deviceName: deviceName,
		address:    address,
		services:   services,
		flat:       bledevice.Flatten(services),
		notifying:  make(map[int]bool),
	}
}

// Run reads commands from stdin until "exit"/"quit"/"disconnect" or EOF
// (Ctrl+D).
func (s *Session) Run() error {
	s.printBanner()
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("ploom> ")
		if !scanner.Scan() {
			fmt.Println()
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		cmd := strings.ToLower(fields[0])
		args := fields[1:]

		switch cmd {
		case "help", "?":
			s.printHelp()
		case "info":
			s.cmdInfo()
		case "services", "ls":
			s.cmdServices()
		case "read":
			s.cmdRead(args)
		case "write":
			s.cmdWrite(args, false)
		case "writecmd":
			s.cmdWrite(args, true)
		case "notify":
			s.cmdNotify(args)
		case "notifyall":
			s.cmdNotifyAll(args)
		case "mtu":
			s.cmdMTU()
		case "disconnect":
			if err := s.device.Disconnect(); err != nil {
				fmt.Println("error:", err)
			} else {
				fmt.Println("disconnected.")
			}
			return nil
		case "exit", "quit":
			return nil
		default:
			fmt.Printf("unknown command: %q (type 'help')\n", cmd)
		}
	}
	return scanner.Err()
}

func (s *Session) printBanner() {
	fmt.Printf("connected to %s (%s) - %d services, %d characteristics\n",
		s.deviceName, s.address, len(s.services), len(s.flat))
	fmt.Println("type 'help' for commands, 'exit' to quit")
}

func (s *Session) printHelp() {
	fmt.Print(`commands:
  help                      show this help
  info                      show device info
  services | ls             list discovered services and characteristics
  read <idx>                read characteristic <idx>
  write <idx> <hex>         write <hex> bytes to characteristic <idx> (with response)
  writecmd <idx> <hex>      write <hex> bytes without waiting for a response
  notify <idx> on|off       enable/disable notifications on characteristic <idx>
  notifyall on|off          enable/disable notifications on every characteristic that supports it
  mtu                       show the negotiated ATT MTU for this connection
  disconnect                disconnect from the device and exit
  exit | quit               leave the REPL (device stays connected)
`)
}

func (s *Session) cmdInfo() {
	connected, err := s.device.Connected()
	status := "unknown"
	if err == nil {
		if connected {
			status = "connected"
		} else {
			status = "disconnected"
		}
	}
	fmt.Printf("name:    %s\n", s.deviceName)
	fmt.Printf("address: %s\n", s.address)
	fmt.Printf("status:  %s\n", status)
	fmt.Printf("gatt:    %d services, %d characteristics\n", len(s.services), len(s.flat))
}

func (s *Session) cmdServices() {
	idx := 0
	for _, svc := range s.services {
		fmt.Printf("service %s\n", svc.UUID)
		for range svc.Chars {
			ch := s.flat[idx]
			fmt.Printf("  [%d] char %s\n", ch.Index, ch.Char.UUID())
			idx++
		}
	}
}

func (s *Session) resolveIndex(arg string) (*bledevice.FlatChar, error) {
	i, err := strconv.Atoi(arg)
	if err != nil {
		return nil, fmt.Errorf("expected a characteristic index, got %q (see 'services')", arg)
	}
	if i < 0 || i >= len(s.flat) {
		return nil, fmt.Errorf("index %d out of range (0..%d), see 'services'", i, len(s.flat)-1)
	}
	return &s.flat[i], nil
}

func (s *Session) cmdRead(args []string) {
	if len(args) != 1 {
		fmt.Println("usage: read <idx>")
		return
	}
	ch, err := s.resolveIndex(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	val, err := ch.Char.Read()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s = %s\n", ch.Char.UUID(), hex.EncodeToString(val))
}

func (s *Session) cmdWrite(args []string, withoutResponse bool) {
	if len(args) != 2 {
		fmt.Printf("usage: %s <idx> <hex>\n", map[bool]string{true: "writecmd", false: "write"}[withoutResponse])
		return
	}
	ch, err := s.resolveIndex(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	data, err := hex.DecodeString(strings.TrimPrefix(args[1], "0x"))
	if err != nil {
		fmt.Println("error: invalid hex payload:", err)
		return
	}
	if withoutResponse {
		err = ch.Char.WriteWithoutResponse(data)
	} else {
		err = ch.Char.Write(data)
	}
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("wrote %d bytes to %s\n", len(data), ch.Char.UUID())
}

// setNotify enables or disables notifications on s.flat[idx], updating
// s.notifying. It is a no-op if the characteristic is already in the
// requested state.
func (s *Session) setNotify(idx int, on bool) error {
	ch := s.flat[idx]
	if on == s.notifying[idx] {
		return nil
	}
	if on {
		uuid := ch.Char.UUID()
		err := ch.Char.EnableNotifications(func(buf []byte) {
			fmt.Printf("\n[%s] notify #%d %s = %s\nploom> ", time.Now().Format("15:04:05"), idx, uuid, hex.EncodeToString(buf))
		})
		if err != nil {
			return err
		}
		s.notifying[idx] = true
		return nil
	}
	if err := ch.Char.EnableNotifications(nil); err != nil {
		return err
	}
	delete(s.notifying, idx)
	return nil
}

func (s *Session) cmdNotify(args []string) {
	if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
		fmt.Println("usage: notify <idx> on|off")
		return
	}
	ch, err := s.resolveIndex(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	on := args[1] == "on"
	verb := map[bool]string{true: "enabled", false: "disabled"}[on]
	if on == s.notifying[ch.Index] {
		fmt.Printf("already %s on %s\n", verb, ch.Char.UUID())
		return
	}
	if err := s.setNotify(ch.Index, on); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("notifications %s on %s\n", verb, ch.Char.UUID())
}

// cmdNotifyAll subscribes/unsubscribes every characteristic in the GATT
// table that supports it, in one shot - meant for exploring a device's
// protocol: turn it on, then physically operate the device (press a
// button, take a puff, plug in charging, etc.) and watch which handles
// fire and with what payload.
func (s *Session) cmdNotifyAll(args []string) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		fmt.Println("usage: notifyall on|off")
		return
	}
	on := args[0] == "on"
	verb := map[bool]string{true: "enabled", false: "disabled"}[on]

	var changed, failed int
	for _, ch := range s.flat {
		if on == s.notifying[ch.Index] {
			continue
		}
		if err := s.setNotify(ch.Index, on); err != nil {
			failed++
			continue
		}
		changed++
	}
	fmt.Printf("notifications %s on %d characteristic(s)", verb, changed)
	if failed > 0 {
		fmt.Printf(" (%d don't support it or failed)", failed)
	}
	fmt.Println()
}

func (s *Session) cmdMTU() {
	mtu, err := s.device.MTU()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("mtu = %d\n", mtu)
}
