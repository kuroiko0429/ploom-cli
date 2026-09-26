package hcible

import (
	"fmt"
	"time"

	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
)

// Characteristic implements bledevice.Characteristic on top of the raw ATT
// client.
type Characteristic struct {
	att *attClient
	ch  attCharacteristic
}

func (c *Characteristic) UUID() string { return c.ch.uuid }

func (c *Characteristic) Read() ([]byte, error) { return c.att.read(c.ch.valueHandle) }

func (c *Characteristic) Write(data []byte) error { return c.att.write(c.ch.valueHandle, data) }

func (c *Characteristic) WriteWithoutResponse(data []byte) error {
	return c.att.writeCommand(c.ch.valueHandle, data)
}

func (c *Characteristic) EnableNotifications(cb func([]byte)) error {
	return c.att.setNotify(c.ch, cb)
}

var (
	_ bledevice.Characteristic = (*Characteristic)(nil)
	_ bledevice.Device         = (*Connection)(nil)
)

// Pair actively initiates LE Legacy "Just Works" pairing and waits for
// encryption to start. Call this immediately after Connect, before
// DiscoverAll or anything else - see smp.go's doc comment for why sending
// this first, unprompted, is the entire point of this package.
func (c *Connection) Pair(timeout time.Duration) error {
	ownAddr, err := c.hci.ReadBDAddr()
	if err != nil {
		return err
	}
	return c.smp.pair(ownAddr, timeout)
}

// DiscoverAll exchanges the ATT MTU and walks the full GATT table
// (services, characteristics, and - for any characteristic supporting
// notify/indicate - its CCCD handle), returning bledevice-ready types.
func (c *Connection) DiscoverAll() ([]bledevice.Service, error) {
	if err := c.att.exchangeMTU(247); err != nil {
		return nil, err
	}

	svcs, err := c.att.discoverServices()
	if err != nil {
		return nil, err
	}

	out := make([]bledevice.Service, 0, len(svcs))
	for _, svc := range svcs {
		chars, err := c.att.discoverCharacteristics(svc)
		if err != nil {
			return nil, fmt.Errorf("hcible: discover characteristics of service %s: %w", svc.uuid, err)
		}

		bsvc := bledevice.Service{UUID: svc.uuid, Chars: make([]bledevice.Characteristic, 0, len(chars))}
		for j, ch := range chars {
			if ch.properties&0x30 != 0 { // notify (0x10) or indicate (0x20)
				end := svc.endHandle
				if j+1 < len(chars) {
					end = chars[j+1].handle - 1
				}
				if cccd, err := c.att.findCCCD(ch.valueHandle+1, end); err == nil {
					ch.cccdHandle = cccd
				}
			}
			bsvc.Chars = append(bsvc.Chars, &Characteristic{att: c.att, ch: ch})
		}
		out = append(out, bsvc)
	}
	return out, nil
}
