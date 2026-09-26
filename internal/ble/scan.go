// Package ble wraps tinygo.org/x/bluetooth to provide the small set of
// operations ploom-cli needs: enabling the adapter, scanning for a device by
// name, connecting, and walking the GATT table.
package ble

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"
)

// ErrScanTimeout is returned by ScanForDevice when no matching device was
// found before the deadline.
var ErrScanTimeout = errors.New("ble: no matching device found before timeout")

// ErrScanInterrupted is returned by ScanForDevice when the user aborts the
// scan (Ctrl+C) before a matching device was found.
var ErrScanInterrupted = errors.New("ble: scan interrupted")

// GetAdapter returns the Bluetooth adapter identified by id (e.g. "hci1"),
// enabled and ready to scan/connect. An empty id uses the system default
// (normally "hci0"). This corresponds to the "Bluetoothアダプタ取得" step.
func GetAdapter(id string) (*bluetooth.Adapter, error) {
	adapter := bluetooth.DefaultAdapter
	if id != "" {
		adapter = bluetooth.NewAdapter(id)
	}
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable bluetooth adapter %q: %w", adapterLabel(id), err)
	}
	return adapter, nil
}

func adapterLabel(id string) string {
	if id == "" {
		return "default"
	}
	return id
}

// ScanForDevice starts a BLE scan and returns the first advertisement whose
// local name contains namePattern (case-insensitive). onSeen, if non-nil, is
// invoked for every advertisement observed (including non-matching ones) so
// the caller can render scan progress.
func ScanForDevice(adapter *bluetooth.Adapter, namePattern string, timeout time.Duration, onSeen func(bluetooth.ScanResult)) (bluetooth.ScanResult, error) {
	pattern := strings.ToUpper(namePattern)

	type outcome struct {
		result bluetooth.ScanResult
		err    error
	}
	done := make(chan outcome, 1)
	stopOnce := make(chan struct{})
	stop := func() {
		select {
		case <-stopOnce:
		default:
			close(stopOnce)
			adapter.StopScan()
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	go func() {
		select {
		case <-sigCh:
			stop()
		case <-timer.C:
			stop()
		case <-done:
		}
	}()

	var matched bluetooth.ScanResult
	var matchedOK bool

	err := adapter.Scan(func(a *bluetooth.Adapter, result bluetooth.ScanResult) {
		if onSeen != nil {
			onSeen(result)
		}
		if pattern != "" && !strings.Contains(strings.ToUpper(result.LocalName()), pattern) {
			return
		}
		matched = result
		matchedOK = true
		stop()
	})

	select {
	case <-done:
	default:
		close(done)
	}

	if matchedOK {
		return matched, nil
	}
	if err != nil {
		return bluetooth.ScanResult{}, fmt.Errorf("scan: %w", err)
	}
	select {
	case <-sigCh:
		return bluetooth.ScanResult{}, ErrScanInterrupted
	default:
	}
	return bluetooth.ScanResult{}, ErrScanTimeout
}

// Connect connects to the device found by ScanForDevice.
func Connect(adapter *bluetooth.Adapter, result bluetooth.ScanResult) (bluetooth.Device, error) {
	device, err := adapter.Connect(result.Address, bluetooth.ConnectionParams{})
	if err != nil {
		return bluetooth.Device{}, fmt.Errorf("connect to %s: %w", result.Address.String(), err)
	}
	return device, nil
}

// ConnectWithRetry calls Connect up to attempts times (attempts < 1 is
// treated as 1), sleeping delay between tries, and reports progress via
// onAttempt (if non-nil) before each try.
//
// Some BLE peripherals (observed on a "Ploom aura" unit via btmon) terminate
// the link within ~1-2 connection intervals of connecting - before any
// GATT/SMP packet is exchanged - if the central isn't already bonded or
// allow-listed. Retrying occasionally succeeds if connection-establishment
// timing happens to line up, but a persistent failure across every attempt
// means the peripheral is deliberately rejecting this central at the link
// layer; no amount of retrying fixes that (see README "Known device
// behavior").
func ConnectWithRetry(adapter *bluetooth.Adapter, result bluetooth.ScanResult, attempts int, delay time.Duration, onAttempt func(attempt, total int)) (bluetooth.Device, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt, attempts)
		}
		device, err := Connect(adapter, result)
		if err == nil {
			return device, nil
		}
		lastErr = err
		if attempt < attempts {
			time.Sleep(delay)
		}
	}
	return bluetooth.Device{}, lastErr
}
