package ble

import (
	"fmt"

	"tinygo.org/x/bluetooth"

	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
)

// deviceWrapper adapts a tinygo bluetooth.Device to bledevice.Device.
type deviceWrapper struct {
	dev bluetooth.Device
	// mtuChar is any one discovered characteristic, used to read the
	// connection's ATT MTU (tinygo's Linux backend only exposes MTU
	// per-characteristic, but it reflects the whole connection).
	mtuChar *bluetooth.DeviceCharacteristic
}

func (d *deviceWrapper) Disconnect() error {
	return d.dev.Disconnect()
}

func (d *deviceWrapper) Connected() (bool, error) {
	return d.dev.Connected()
}

func (d *deviceWrapper) MTU() (uint16, error) {
	if d.mtuChar == nil {
		return 0, fmt.Errorf("ble: no characteristic available to query MTU")
	}
	return d.mtuChar.GetMTU()
}

// charWrapper adapts a tinygo bluetooth.DeviceCharacteristic to
// bledevice.Characteristic.
type charWrapper struct {
	uuid bluetooth.UUID
	c    bluetooth.DeviceCharacteristic
}

func (c *charWrapper) UUID() string { return c.uuid.String() }

func (c *charWrapper) Read() ([]byte, error) {
	buf := make([]byte, 512)
	n, err := c.c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (c *charWrapper) Write(data []byte) error {
	_, err := c.c.Write(data)
	return err
}

func (c *charWrapper) WriteWithoutResponse(data []byte) error {
	_, err := c.c.WriteWithoutResponse(data)
	return err
}

func (c *charWrapper) EnableNotifications(cb func([]byte)) error {
	if cb == nil {
		return c.c.EnableNotifications(nil)
	}
	return c.c.EnableNotifications(func(buf []byte) { cb(buf) })
}

// DiscoverAll walks the full GATT table of a connected device: every
// service, then every characteristic of each service. This corresponds to
// the "GATT discovery" step. It returns a bledevice.Device and
// []bledevice.Service so callers don't need to depend on tinygo-bluetooth.
func DiscoverAll(dev bluetooth.Device) (bledevice.Device, []bledevice.Service, error) {
	services, err := dev.DiscoverServices(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("discover services: %w", err)
	}

	wrapper := &deviceWrapper{dev: dev}
	out := make([]bledevice.Service, 0, len(services))
	for _, svc := range services {
		chars, err := svc.DiscoverCharacteristics(nil)
		if err != nil {
			return nil, nil, fmt.Errorf("discover characteristics of service %s: %w", svc.UUID().String(), err)
		}
		bsvc := bledevice.Service{UUID: svc.UUID().String(), Chars: make([]bledevice.Characteristic, 0, len(chars))}
		for _, ch := range chars {
			cw := &charWrapper{uuid: ch.UUID(), c: ch}
			bsvc.Chars = append(bsvc.Chars, cw)
			if wrapper.mtuChar == nil {
				wrapper.mtuChar = &ch
			}
		}
		out = append(out, bsvc)
	}
	return wrapper, out, nil
}
