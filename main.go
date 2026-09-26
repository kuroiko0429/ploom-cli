// Command ploom-cli is a small BLE CLI for Ploom-brand heated tobacco
// devices, in the spirit of iqos-cli. It scans for a nearby device, connects,
// discovers its GATT table, remembers it in config.toml, and drops into an
// interactive REPL for reading/writing/notifying on characteristics.
//
// `ploom-cli dump [-watch]` connects the same way but, instead of the REPL,
// prints every characteristic's current value once and - with -watch -
// subscribes to notifications and prints live updates until Ctrl+C. Useful
// for seeing what's on the device before poking at it with `write`.
//
// Three transports are available (-transport flag):
//   - "bluez" (default): talks to BlueZ over D-Bus directly from this
//     process. Confirmed reliable against a Ploom aura, but only if the
//     device is put into its pairing/setup mode before connecting - see
//     README "Known device behavior".
//   - "serial": delegates the actual BLE work to a microcontroller (e.g. an
//     M5StickC Plus2, see tools/M5central) running matching firmware,
//     reached over a USB-serial line. Requires flashing a microcontroller.
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
	"github.com/kuroiko0429/ploom-cli/internal/dump"
	"github.com/kuroiko0429/ploom-cli/internal/hcible"
	"github.com/kuroiko0429/ploom-cli/internal/repl"
	"github.com/kuroiko0429/ploom-cli/internal/serialble"
)

// errAborted is returned by the connect* functions when the user declines
// the connect confirmation prompt. It isn't a real error: callers treat it
// as "exit 0, nothing more to do".
var errAborted = errors.New("aborted")

// commonFlags are the connect-related flags shared between the default
// command and `dump`.
type commonFlags struct {
	namePattern *string
	scanTimeout *time.Duration
	assumeYes   *bool
	cfgFlag     *string
	retries     *int
	retryDelay  *time.Duration
	adapterID   *string
	transport   *string
	serialPort  *string
	serialBaud  *int
}

func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		namePattern: fs.String("name", "Ploom", "substring to match against the advertised device name (case-insensitive)"),
		scanTimeout: fs.Duration("timeout", 30*time.Second, "how long to scan before giving up"),
		assumeYes:   fs.Bool("yes", false, "skip the connect confirmation prompt"),
		cfgFlag:     fs.String("config", "", "path to config.toml (default: $XDG_CONFIG_HOME/ploom-cli/config.toml)"),
		retries:     fs.Int("retries", 3, "number of connection attempts before giving up (-transport bluez only)"),
		retryDelay:  fs.Duration("retry-delay", 2*time.Second, "delay between connection attempts (-transport bluez only)"),
		adapterID:   fs.String("adapter", "", "Bluetooth adapter to use, e.g. hci1 (default: system default, normally hci0; -transport bluez/rawhci only)"),
		transport:   fs.String("transport", "bluez", `BLE transport: "bluez" (default), "serial" (M5Stick bridge, see tools/M5central), or "rawhci" (experimental, no BlueZ, needs root)`),
		serialPort:  fs.String("port", "", "serial port for -transport serial, e.g. /dev/ttyACM0"),
		serialBaud:  fs.Int("baud", 115200, "serial baud rate for -transport serial"),
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dump" {
		if err := runDumpCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	cf := registerCommonFlags(flag.CommandLine)
	flag.Parse()

	var err error
	switch *cf.transport {
	case "bluez":
		err = runBluez(*cf.namePattern, *cf.scanTimeout, *cf.assumeYes, *cf.cfgFlag, *cf.retries, *cf.retryDelay, *cf.adapterID)
	case "serial":
		err = runSerial(*cf.namePattern, *cf.scanTimeout, *cf.assumeYes, *cf.cfgFlag, *cf.serialPort, *cf.serialBaud)
	case "rawhci":
		err = runRawHCI(*cf.namePattern, *cf.scanTimeout, *cf.assumeYes, *cf.cfgFlag, *cf.adapterID)
	default:
		err = fmt.Errorf("unknown -transport %q (want \"bluez\", \"serial\", or \"rawhci\")", *cf.transport)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runDumpCmd parses `ploom-cli dump [flags]` and runs it.
func runDumpCmd(args []string) error {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	cf := registerCommonFlags(fs)
	watch := fs.Bool("watch", false, "after the initial dump, subscribe to notifications on every characteristic and print live updates until Ctrl+C")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runDump(*cf.namePattern, *cf.scanTimeout, *cf.assumeYes, *cf.cfgFlag, *cf.retries, *cf.retryDelay, *cf.adapterID, *cf.transport, *cf.serialPort, *cf.serialBaud, *watch)
}

func resolveConfigPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	return config.DefaultPath()
}

// connected holds everything produced by any transport's connect+discover
// step, before either dropping into the REPL (the default command) or
// running `dump` instead.
type connected struct {
	device   bledevice.Device
	name     string
	address  string
	addrType string
	services []bledevice.Service
	// closeConn is best-effort cleanup of the transport's own resource (a
	// raw HCI socket, a serial port). nil if the transport has none (bluez
	// is managed entirely by bluetoothd, nothing here to close).
	closeConn func()
}

// runBluez implements the default flow (connect, then REPL) over BlueZ
// D-Bus.
func runBluez(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, retries int, retryDelay time.Duration, adapterID string) error {
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}
	conn, err := connectBluez(namePattern, scanTimeout, assumeYes, retries, retryDelay, adapterID)
	if errors.Is(err, errAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	return finish(cfgPath, conn)
}

// connectBluez scans for and connects to the device entirely over BlueZ
// D-Bus, returning once its GATT table has been discovered.
func connectBluez(namePattern string, scanTimeout time.Duration, assumeYes bool, retries int, retryDelay time.Duration, adapterID string) (*connected, error) {
	fmt.Printf("Bluetoothアダプタを取得しています (%s)...\n", adapterLabelForLog(adapterID))
	adapter, err := ble.GetAdapter(adapterID)
	if err != nil {
		return nil, err
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
			return nil, fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, ble.ErrScanInterrupted) {
			return nil, fmt.Errorf("scan interrupted")
		}
		return nil, err
	}

	deviceName := result.LocalName()
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.Address.String())

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.Address.String()))
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil, errAborted
		}
	}

	btDevice, err := ble.ConnectWithRetry(adapter, result, retries, retryDelay, func(attempt, total int) {
		if attempt > 1 {
			fmt.Printf("retrying connection (%d/%d)...\n", attempt, total)
		}
	})
	if err != nil {
		printConnectFailureHint()
		return nil, err
	}
	fmt.Println("connected.")

	fmt.Println("GATTディスカバリを実行しています...")
	device, services, err := ble.DiscoverAll(btDevice)
	if err != nil {
		return nil, err
	}

	addrType := "public"
	if result.Address.IsRandom() {
		addrType = "random"
	}
	return &connected{device: device, name: deviceName, address: result.Address.String(), addrType: addrType, services: services}, nil
}

// runSerial implements the default flow (connect, then REPL) by delegating
// scan/connect/GATT to a microcontroller over a serial line (see
// internal/serialble).
func runSerial(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, port string, baud int) error {
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}
	conn, err := connectSerial(namePattern, scanTimeout, assumeYes, port, baud)
	if errors.Is(err, errAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.closeConn()
	return finish(cfgPath, conn)
}

// connectSerial scans for and connects to the device through an M5Stick
// bridge over USB-serial, returning once its GATT table has been
// discovered. On success, conn.closeConn closes the serial port; on every
// error path the port is closed here since there's no caller to hand it to.
func connectSerial(namePattern string, scanTimeout time.Duration, assumeYes bool, port string, baud int) (conn *connected, err error) {
	if port == "" {
		return nil, fmt.Errorf("-transport serial requires -port, e.g. -port /dev/ttyACM0")
	}

	fmt.Printf("M5Stickブリッジに接続しています (%s @ %d baud)...\n", port, baud)
	bridge, err := serialble.Open(port, baud, 8*time.Second)
	if err != nil {
		return nil, err
	}
	closeBridge := true
	defer func() {
		if closeBridge {
			bridge.Close()
		}
	}()
	bridge.Logf = func(line string) { fmt.Println("[m5]", line) }

	if err := bridge.Ping(); err != nil {
		return nil, fmt.Errorf("bridge not responding: %w", err)
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
			return nil, fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, serialble.ErrScanInterrupted) {
			return nil, fmt.Errorf("scan interrupted")
		}
		return nil, err
	}

	deviceName := result.Name
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.Address)

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.Address))
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil, errAborted
		}
	}

	fmt.Println("接続・ペアリング・GATTディスカバリを実行しています (M5Stick経由)...")
	device, services, auth, err := bridge.Connect(result.Address, 20*time.Second)
	if err != nil {
		return nil, err
	}
	fmt.Printf("connected. bonded=%v encrypted=%v authenticated=%v\n", auth.Bonded, auth.Encrypted, auth.Authenticated)

	closeBridge = false
	// The serial protocol doesn't currently report the peer's BLE address
	// type (public/random), unlike the bluez transport.
	return &connected{device: device, name: deviceName, address: result.Address, addrType: "unknown", services: services, closeConn: func() { bridge.Close() }}, nil
}

// runRawHCI implements the default flow (connect, then REPL) with a
// from-scratch BLE central (internal/hcible), bypassing BlueZ/bluetoothd
// entirely via a raw HCI socket. Experimental - see README "Known device
// behavior".
func runRawHCI(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, adapterID string) error {
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}
	conn, err := connectRawHCI(namePattern, scanTimeout, assumeYes, adapterID)
	if errors.Is(err, errAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.closeConn()
	return finish(cfgPath, conn)
}

// connectRawHCI scans for and connects to the device over a raw
// HCI_CHANNEL_USER socket, pairing (LE Legacy Just Works) and discovering
// its GATT table. On success, conn.closeConn closes the HCI socket; on
// every error path the socket is closed here since there's no caller to
// hand it to.
func connectRawHCI(namePattern string, scanTimeout time.Duration, assumeYes bool, adapterID string) (conn *connected, err error) {
	if adapterID == "" {
		adapterID = "hci0"
	}

	fmt.Println("警告: raw HCIモードは実機で一度、コントローラをUSBレベルで応答不能にした実績あり")
	fmt.Println("      (復旧には `sudo rmmod btusb && sudo modprobe btusb` 相当が必要だった)。")
	fmt.Println("      唯一のBluetoothアダプタでは実行しないこと。詳細はREADMEの")
	fmt.Println("      \"Known device behavior\" 参照。")
	fmt.Printf("HCIアダプタを直接オープンしています (%s, raw HCI_CHANNEL_USER, BlueZ非経由)...\n", adapterID)
	hci, err := hcible.Open(adapterID)
	if err != nil {
		return nil, fmt.Errorf("%w (raw HCI requires root/CAP_NET_RAW - try running with sudo)", err)
	}
	closeHCI := true
	defer func() {
		if closeHCI {
			hci.Close()
		}
	}()

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
			return nil, fmt.Errorf("no device matching %q found within %s", namePattern, scanTimeout)
		}
		if errors.Is(err, hcible.ErrScanInterrupted) {
			return nil, fmt.Errorf("scan interrupted")
		}
		return nil, err
	}

	deviceName := result.LocalName
	fmt.Printf("Found %s: %s (%s)\n", namePattern, deviceName, result.AddressString())

	if !assumeYes {
		ok, err := confirm(fmt.Sprintf("Connect to %s? [y/N] ", result.AddressString()))
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Println("aborted.")
			return nil, errAborted
		}
	}

	fmt.Println("接続しています (LE Create Connection, 15ms/5s)...")
	hciConn, err := hci.Connect(result.Address, result.AddressType, hcible.DefaultConnParams, 15*time.Second)
	if err != nil {
		return nil, err
	}

	fmt.Println("ペアリングしています (LE Legacy Just Works, 接続直後に即座に開始)...")
	if err := hciConn.Pair(15 * time.Second); err != nil {
		hciConn.Disconnect()
		return nil, fmt.Errorf("pairing failed: %w", err)
	}
	fmt.Println("paired and encrypted.")

	fmt.Println("GATTディスカバリを実行しています...")
	services, err := hciConn.DiscoverAll()
	if err != nil {
		hciConn.Disconnect()
		return nil, err
	}

	addrType := "public"
	if result.AddressType == 0x01 {
		addrType = "random"
	}
	closeHCI = false
	return &connected{device: hciConn, name: deviceName, address: result.AddressString(), addrType: addrType, services: services, closeConn: func() { hci.Close() }}, nil
}

// runDump implements `ploom-cli dump`: connect via whichever transport is
// selected, then hand off to internal/dump instead of the REPL.
func runDump(namePattern string, scanTimeout time.Duration, assumeYes bool, cfgPathFlag string, retries int, retryDelay time.Duration, adapterID, transport, serialPort string, serialBaud int, watch bool) error {
	cfgPath, err := resolveConfigPath(cfgPathFlag)
	if err != nil {
		return err
	}

	var conn *connected
	switch transport {
	case "bluez":
		conn, err = connectBluez(namePattern, scanTimeout, assumeYes, retries, retryDelay, adapterID)
	case "serial":
		conn, err = connectSerial(namePattern, scanTimeout, assumeYes, serialPort, serialBaud)
	case "rawhci":
		conn, err = connectRawHCI(namePattern, scanTimeout, assumeYes, adapterID)
	default:
		err = fmt.Errorf("unknown -transport %q (want \"bluez\", \"serial\", or \"rawhci\")", transport)
	}
	if errors.Is(err, errAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	if conn.closeConn != nil {
		defer conn.closeConn()
	}

	if err := persistSnapshot(cfgPath, conn); err != nil {
		return err
	}
	return dump.Run(conn.device, conn.name, conn.address, conn.services, watch)
}

// persistSnapshot prints the discovered GATT summary and saves it to
// config.toml. Shared by the REPL and dump entry points.
func persistSnapshot(cfgPath string, conn *connected) error {
	flat := bledevice.Flatten(conn.services)
	fmt.Printf("found %d services, %d characteristics.\n", len(conn.services), len(flat))

	cfg := buildConfig(conn.name, conn.address, conn.addrType, conn.services)
	if err := config.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Println("接続した端末情報を保存しました:", cfgPath)
	return nil
}

// finish persists the device/GATT snapshot to config.toml and drops into
// the REPL. Shared by every transport once they've produced a connected
// bledevice.Device and its discovered services.
func finish(cfgPath string, conn *connected) error {
	if err := persistSnapshot(cfgPath, conn); err != nil {
		return err
	}
	session := repl.New(conn.device, conn.name, conn.address, conn.services)
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
