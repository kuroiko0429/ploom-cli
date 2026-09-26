// Command ploom-cli is a small BLE CLI for Ploom-brand heated tobacco
// devices, in the spirit of iqos-cli. It scans for a nearby device, connects,
// discovers its GATT table, remembers it in config.toml, and drops into an
// interactive REPL for reading/writing/notifying on characteristics.
//
// Three transports are available (-transport flag):
//   - "bluez" (default): talks to BlueZ over D-Bus directly from this
//     process. Known not to work with at least one real device (Ploom aura)
//     on Linux - see README "Known device behavior".
//   - "serial": delegates the actual BLE work to a microcontroller (e.g. an
//     M5StickC Plus2, see tools/M5central) running matching firmware,
//     reached over a USB-serial line. This works where "bluez" doesn't,
//     because the microcontroller's NimBLE stack handles the connection
//     setup differently than BlueZ/the Linux kernel does. Requires flashing
//     a microcontroller.
//   - "rawhci": a from-scratch BLE central (internal/hcible) that opens the
//     adapter directly via a raw HCI_CHANNEL_USER socket, bypassing
//     BlueZ/bluetoothd entirely, controlling the exact HCI/SMP command
//     sequence sent to the controller. No extra hardware needed, but
//     requires root/CAP_NET_RAW and has not been verified against real
//     hardware (root was not available in the environment that wrote it) -
//     see README "Known device behavior" before relying on it.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/kuroiko0429/ploom-cli/internal/ble"
	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
	"github.com/kuroiko0429/ploom-cli/internal/config"
	"github.com/kuroiko0429/ploom-cli/internal/hcible"
	"github.com/kuroiko0429/ploom-cli/internal/repl"
	"github.com/kuroiko0429/ploom-cli/internal/serialble"
)

func main() {
	namePattern := flag.String("name", "Ploom", "substring to match against the advertised device name (case-insensitive)")
	scanTimeout := flag.Duration("timeout", 30*time.Second, "how long to scan before giving up")
	assumeYes := flag.Bool("yes", false, "skip the connect confirmation prompt")
	cfgFlag := flag.String("config", "", "path to config.toml (default: $XDG_CONFIG_HOME/ploom-cli/config.toml)")
	retries := flag.Int("retries", 3, "number of connection attempts before giving up (-transport bluez only)")
	retryDelay := flag.Duration("retry-delay", 2*time.Second, "delay between connection attempts (-transport bluez only)")
	adapterID := flag.String("adapter", "", "Bluetooth adapter to use, e.g. hci1 (default: system default, normally hci0; -transport bluez/rawhci only)")
	transport := flag.String("transport", "bluez", `BLE transport: "bluez" (default), "serial" (M5Stick bridge, see tools/M5central), or "rawhci" (experimental, no BlueZ, needs root)`)
	serialPort := flag.String("port", "", "serial port for -transport serial, e.g. /dev/ttyACM0")
	serialBaud := flag.Int("baud", 115200, "serial baud rate for -transport serial")
	flag.Parse()

	var err error
	switch *transport {
	case "bluez":
		err = runBluez(*namePattern, *scanTimeout, *assumeYes, *cfgFlag, *retries, *retryDelay, *adapterID)
	case "serial":
		err = runSerial(*namePattern, *scanTimeout, *assumeYes, *cfgFlag, *serialPort, *serialBaud)
	case "rawhci":
		err = runRawHCI(*namePattern, *scanTimeout, *assumeYes, *cfgFlag, *adapterID)
	default:
		err = fmt.Errorf("unknown -transport %q (want \"bluez\", \"serial\", or \"rawhci\")", *transport)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func resolveConfigPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	return config.DefaultPath()
}

// runBluez implements the flow entirely over BlueZ D-Bus.
func runBluez(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, retries int, retryDelay time.Duration, adapterID string) error {
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}

	fmt.Printf("Bluetoothアダプタを取得しています (%s)...\n", adapterLabelForLog(adapterID))
	adapter, err := ble.GetAdapter(adapterID)
	if err != nil {
		return err
	}

	fmt.Printf("BLEスキャンを開始します (名前に %q を含むデバイスを探します, timeout=%s)...\n", namePattern, scanTimeout)
	result, err := ble.ScanForDevice(adapter, namePattern, scanTimeout, func(r bluetooth.ScanResult) {
		name := r.LocalName()
		if name == "" {
			name = "(no name)"
		}
		fmt.Printf("  ... saw %-24s %s  rssi=%d\n", name, r.Address.String(), r.RSSI)
	})
	if err != nil {
		if errors.Is(err, ble.ErrScanTimeout) {
			return fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, ble.ErrScanInterrupted) {
			return fmt.Errorf("scan interrupted")
		}
		return err
	}

	deviceName := result.LocalName()
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.Address.String())

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.Address.String()))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil
		}
	}

	btDevice, err := ble.ConnectWithRetry(adapter, result, retries, retryDelay, func(attempt, total int) {
		if attempt > 1 {
			fmt.Printf("retrying connection (%d/%d)...\n", attempt, total)
		}
	})
	if err != nil {
		printConnectFailureHint()
		return err
	}
	fmt.Println("connected.")

	fmt.Println("GATTディスカバリを実行しています...")
	device, services, err := ble.DiscoverAll(btDevice)
	if err != nil {
		return err
	}

	addrType := "public"
	if result.Address.IsRandom() {
		addrType = "random"
	}

	return finish(cfgPath, deviceName, result.Address.String(), addrType, device, services)
}

// runSerial implements the flow by delegating scan/connect/GATT to a
// microcontroller over a serial line (see internal/serialble).
func runSerial(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, port string, baud int) error {
	if port == "" {
		return fmt.Errorf("-transport serial requires -port, e.g. -port /dev/ttyACM0")
	}
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}

	fmt.Printf("M5Stickブリッジに接続しています (%s @ %d baud)...\n", port, baud)
	bridge, err := serialble.Open(port, baud, 8*time.Second)
	if err != nil {
		return err
	}
	bridge.Logf = func(line string) { fmt.Println("[m5]", line) }
	defer bridge.Close()

	if err := bridge.Ping(); err != nil {
		return fmt.Errorf("bridge not responding: %w", err)
	}

	fmt.Printf("BLEスキャンを開始します (名前に %q を含むデバイスを探します, timeout=%s)...\n", namePattern, scanTimeout)
	upper := strings.ToUpper(namePattern)
	result, err := bridge.Scan(scanTimeout, func(r serialble.ScanResult) bool {
		name := r.Name
		if name == "" {
			name = "(no name)"
		}
		fmt.Printf("  ... saw %-24s %s  rssi=%d\n", name, r.Address, r.RSSI)
		return namePattern == "" || strings.Contains(strings.ToUpper(r.Name), upper)
	})
	if err != nil {
		if errors.Is(err, serialble.ErrScanTimeout) {
			return fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, serialble.ErrScanInterrupted) {
			return fmt.Errorf("scan interrupted")
		}
		return err
	}

	deviceName := result.Name
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.Address)

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.Address))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil
		}
	}

	fmt.Println("接続・ペアリング・GATTディスカバリを実行しています (M5Stick経由)...")
	device, services, auth, err := bridge.Connect(result.Address, 20*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("connected. bonded=%v encrypted=%v authenticated=%v\n", auth.Bonded, auth.Encrypted, auth.Authenticated)

	// The serial protocol doesn't currently report the peer's BLE address
	// type (public/random), unlike the bluez transport.
	return finish(cfgPath, deviceName, result.Address, "unknown", device, services)
}

// runRawHCI implements the flow with a from-scratch BLE central
// (internal/hcible), bypassing BlueZ/bluetoothd entirely via a raw HCI
// socket. Experimental: written to fix the same connect failure documented
// under -transport bluez, but not verified against real hardware (this
// environment has no root access to test it) - see README "Known device
// behavior".
func runRawHCI(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, adapterID string) error {
	if adapterID == "" {
		adapterID = "hci0"
	}
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}

	fmt.Println("警告: raw HCIモードは実機で一度、コントローラをUSBレベルで応答不能にした実績あり")
	fmt.Println("      (復旧には `sudo rmmod btusb && sudo modprobe btusb` 相当が必要だった)。")
	fmt.Println("      唯一のBluetoothアダプタでは実行しないこと。詳細はREADMEの")
	fmt.Println("      \"Known device behavior\" 参照。")
	fmt.Printf("HCIアダプタを直接オープンしています (%s, raw HCI_CHANNEL_USER, BlueZ非経由)...\n", adapterID)
	hci, err := hcible.Open(adapterID)
	if err != nil {
		return fmt.Errorf("%w (raw HCI requires root/CAP_NET_RAW - try running with sudo)", err)
	}
	defer hci.Close()

	fmt.Printf("BLEスキャンを開始します (名前に %q を含むデバイスを探します, timeout=%s)...\n", namePattern, scanTimeout)
	upper := strings.ToUpper(namePattern)
	result, err := hci.Scan(scanTimeout, func(r hcible.ScanResult) bool {
		name := r.LocalName
		if name == "" {
			name = "(no name)"
		}
		fmt.Printf("  ... saw %-24s %s  rssi=%d\n", name, r.AddressString(), r.RSSI)
		return namePattern == "" || strings.Contains(strings.ToUpper(r.LocalName), upper)
	})
	if err != nil {
		if errors.Is(err, hcible.ErrScanTimeout) {
			return fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, hcible.ErrScanInterrupted) {
			return fmt.Errorf("scan interrupted")
		}
		return err
	}

	deviceName := result.LocalName
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.AddressString())

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.AddressString()))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil
		}
	}

	fmt.Println("接続しています (LE Create Connection, 15ms/5s)...")
	conn, err := hci.Connect(result.Address, result.AddressType, hcible.DefaultConnParams, 15*time.Second)
	if err != nil {
		return err
	}

	fmt.Println("ペアリングしています (LE Legacy Just Works, 接続直後に即座に開始)...")
	if err := conn.Pair(15 * time.Second); err != nil {
		conn.Disconnect()
		return fmt.Errorf("pairing failed: %w", err)
	}
	fmt.Println("paired and encrypted.")

	fmt.Println("GATTディスカバリを実行しています...")
	services, err := conn.DiscoverAll()
	if err != nil {
		conn.Disconnect()
		return err
	}

	addrType := "public"
	if result.AddressType == 0x01 {
		addrType = "random"
	}
	return finish(cfgPath, deviceName, result.AddressString(), addrType, conn, services)
}

// finish saves the device/GATT snapshot to config.toml and drops into the
// REPL. Shared by both transports once they've produced a connected
// bledevice.Device and its discovered services.
func finish(cfgPath, deviceName, address, addrType string, device bledevice.Device, services []bledevice.Service) error {
	flat := bledevice.Flatten(services)
	fmt.Printf("found %d services, %d characteristics.\n", len(services), len(flat))

	cfg := buildConfig(deviceName, address, addrType, services)
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Println("接続した端末情報を保存しました:", cfgPath)

	session := repl.New(device, deviceName, address, services)
	return session.Run()
}

func buildConfig(deviceName, address, addrType string, services []bledevice.Service) config.Config {
	entries := make([]config.ServiceEntry, 0, len(services))
	for _, svc := range services {
		chars := make([]string, 0, len(svc.Chars))
		for _, ch := range svc.Chars {
			chars = append(chars, ch.UUID())
		}
		entries = append(entries, config.ServiceEntry{
			UUID:            svc.UUID,
			Characteristics: chars,
		})
	}

	return config.Config{
		Device: config.Device{
			Name:        deviceName,
			Address:     address,
			AddressType: addrType,
			ConnectedAt: time.Now(),
			Services:    entries,
		},
	}
}

func confirm(prompt string) (bool, error) {
	fmt.Print(prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}

// printConnectFailureHint prints guidance for the connect failure mode
// diagnosed via btmon/Android HCI snoop captures on a Ploom aura unit, for
// the -transport bluez path specifically.
//
// Confirmed facts:
//   - In the device's normal/background advertising state, the link
//     establishes (LE Enhanced Connection Complete: Success) and the
//     peripheral itself terminates it (Disconnect reason: Remote User
//     Terminated Connection) after almost exactly 2 connection events -
//     ~85ms at BlueZ's default 45ms interval, ~33ms after forcing a 15ms
//     interval via a manual MGMT_OP_LOAD_CONN_PARAM (0x0035) call matching
//     Android's proven-working values - with zero L2CAP/SMP/ATT packets
//     exchanged either way. So the peripheral's patience in this state is
//     event-count-based, not wall-clock based, and matching Android's
//     connection interval does NOT fix it (falsified by direct experiment).
//     BlueZ's main.conf [LE] interval settings are also dead config for
//     Device1.Connect() on Linux regardless (upstream bug,
//     bluez/bluez#293).
//   - Confirmed fix, reproduced repeatedly: put the device into its
//     dedicated pairing/setup mode first - on a "Ploom aura" unit, open and
//     close the slide cover, then hold the button for ~5 seconds until the
//     LED starts blinking. Connecting with plain -transport bluez while
//     the LED is blinking succeeds every time (LED goes solid/off on
//     success), and - surprisingly - no SMP pairing/bonding is required at
//     all: reads work on every characteristic including the vendor
//     0xfef5 control service straight over an unauthenticated ATT link.
//     Once one connection has landed this way, later reconnects (without
//     repeating the physical trigger) keep succeeding for some time
//     afterward - the short-patience state above is apparently specific to
//     the device's untouched background-advertising mode.
//   - -transport serial (an M5StickC Plus2 running NimBLE-Arduino, see
//     tools/M5central) and -transport rawhci remain available as
//     alternatives that don't need the physical trigger, for unattended/
//     headless use.
func printConnectFailureHint() {
	fmt.Fprint(os.Stderr, `
connect failed after all retries. If the failure looks like
"le-connection-abort-by-local", this is almost certainly the Ploom's
background advertising state being too impatient for BlueZ's connection
setup - confirmed fix:

  1. Put the device into pairing/setup mode: open and close the slide
     cover, then hold the button for ~5 seconds until its LED starts
     blinking.
  2. Immediately retry this same command while the LED is blinking.

No pairing/bonding is needed - a plain unauthenticated connection works
once it lands during that window, and stays reliable for later reconnects
afterward too.

If that doesn't help, -transport serial (an M5StickC Plus2 running
NimBLE-Arduino, see tools/M5central in this repo) and -transport rawhci are
available as alternatives - see the README's "Known device behavior"
section.
`)
}

// adapterLabelForLog renders the adapter flag value for the startup log line.
func adapterLabelForLog(id string) string {
	if id == "" {
		return "default adapter, normally hci0"
	}
	return id
}
