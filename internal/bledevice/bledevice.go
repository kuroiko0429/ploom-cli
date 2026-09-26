// Package bledevice defines transport-agnostic interfaces for a connected
// BLE peripheral and its GATT table. ploom-cli has two backends that produce
// these: internal/ble (BlueZ over D-Bus) and internal/serialble (a
// microcontroller BLE bridge talked to over a serial port). Everything
// downstream (the REPL, config persistence) only depends on this package,
// not on either backend directly.
package bledevice

// Characteristic is a single GATT characteristic: readable, writable, and
// optionally subscribable, regardless of what transport backs it.
type Characteristic interface {
	// UUID returns the characteristic's UUID as a canonical string, e.g.
	// "00002a29-0000-1000-8000-00805f9b34fb" or a 128-bit vendor UUID.
	UUID() string

	// Read reads the current value.
	Read() ([]byte, error)

	// Write writes a new value and waits for the peer to acknowledge it.
	Write(data []byte) error

	// WriteWithoutResponse writes a new value without waiting for
	// acknowledgement.
	WriteWithoutResponse(data []byte) error

	// EnableNotifications subscribes to value-change notifications, calling
	// cb with each new value. Passing a nil callback unsubscribes.
	EnableNotifications(cb func(value []byte)) error
}

// Service is a GATT service and its characteristics.
type Service struct {
	UUID  string
	Chars []Characteristic
}

// Device is a connected peripheral, regardless of transport.
type Device interface {
	// Disconnect tears down the connection.
	Disconnect() error

	// Connected reports whether the connection is still up.
	Connected() (bool, error)

	// MTU returns the negotiated ATT MTU for this connection.
	MTU() (uint16, error)
}

// FlatChar tags a characteristic with a stable index and its owning
// service's UUID, so callers (e.g. the REPL) can address any characteristic
// with a single number instead of a full UUID.
type FlatChar struct {
	Index       int
	ServiceUUID string
	Char        Characteristic
}

// Flatten produces a single indexed list of every characteristic across
// every service, in discovery order.
func Flatten(services []Service) []FlatChar {
	var flat []FlatChar
	for _, svc := range services {
		for _, ch := range svc.Chars {
			flat = append(flat, FlatChar{Index: len(flat), ServiceUUID: svc.UUID, Char: ch})
		}
	}
	return flat
}
