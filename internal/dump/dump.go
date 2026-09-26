// Package dump implements `ploom-cli dump`: read every discovered
// characteristic once and print its value, optionally subscribing to
// notifications afterward and printing live updates until interrupted.
// Meant for reverse-engineering a device's GATT protocol - seeing what
// "tags" (characteristics/values) exist and which ones change - without
// having to drive the REPL by hand first.
package dump

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
)

// Run prints the current value of every characteristic across every
// service, then - if watch is true - subscribes to notifications on every
// characteristic that supports them and prints live updates (with a
// timestamp) until interrupted with Ctrl+C (SIGINT) or SIGTERM.
func Run(device bledevice.Device, deviceName, address string, services []bledevice.Service, watch bool) error {
	flat := bledevice.Flatten(services)
	fmt.Printf("%s (%s) - %d services, %d characteristics\n", deviceName, address, len(services), len(flat))

	idx := 0
	for _, svc := range services {
		fmt.Printf("service %s\n", svc.UUID)
		for range svc.Chars {
			ch := flat[idx]
			fmt.Printf("  [%d] %s = %s\n", ch.Index, ch.Char.UUID(), readOrNote(ch.Char))
			idx++
		}
	}

	if !watch {
		return nil
	}

	subscribed, failed := 0, 0
	for _, ch := range flat {
		uuid, i := ch.Char.UUID(), ch.Index
		err := enableNotificationsWithTimeout(ch.Char, func(buf []byte) {
			fmt.Printf("[%s] notify [%d] %s = %s\n", time.Now().Format("15:04:05"), i, uuid, hex.EncodeToString(buf))
		}, 5*time.Second)
		if err != nil {
			fmt.Printf("  [%d] %s: no notifications (%s)\n", i, uuid, err)
			failed++
			continue
		}
		subscribed++
	}
	fmt.Printf("watching %d characteristic(s) for changes", subscribed)
	if failed > 0 {
		fmt.Printf(" (%d don't support it or failed)", failed)
	}
	fmt.Println(" - Ctrl+C to stop, physically operate the device now")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nstopping.")
	return nil
}

// readOrNote reads a characteristic's value for the initial dump, turning a
// read error (e.g. a write-only or notify-only characteristic) into a short
// inline note instead of aborting the whole dump.
func readOrNote(ch bledevice.Characteristic) string {
	val, err := ch.Read()
	if err != nil {
		return fmt.Sprintf("(unreadable: %s)", err)
	}
	if len(val) == 0 {
		return "(empty)"
	}
	return hex.EncodeToString(val)
}

// enableNotificationsWithTimeout guards EnableNotifications with a
// deadline: some characteristics look notify-capable but never actually
// complete the subscribe (e.g. no CCCD descriptor behind a GATT server
// quirk), and a naive call can block forever - which, unlike a normal
// error, silently wedges the whole watch loop with no diagnostic. The
// underlying call is left running in its goroutine if it times out; it's
// harmless since the process is short-lived.
func enableNotificationsWithTimeout(ch bledevice.Characteristic, cb func([]byte), timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- ch.EnableNotifications(cb) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	}
}
