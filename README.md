# ploom-cli

A small BLE CLI for Ploom-brand heated tobacco devices, in the spirit of
`iqos-cli`. Three transports, selected with `-transport`:

- **`bluez`** (default): talks to BlueZ over D-Bus directly via
  [`tinygo.org/x/bluetooth`](https://pkg.go.dev/tinygo.org/x/bluetooth), so
  it runs alongside a normal `bluetoothd` — no raw HCI socket, no root
  required. **Confirmed broken against the Ploom aura on one specific
  controller (Intel AX201 / `btintel`), confirmed working out of the box on
  another (Realtek RTL8852BU / `btrtl`)** — see "Known device behavior"
  below. Try this first; it's the simplest transport and may just work on
  your hardware.
- **`serial`**: delegates the actual BLE work to a microcontroller (an
  M5StickC Plus2 running the firmware in [`tools/M5central`](tools/M5central),
  NimBLE-Arduino) reached over USB-serial. This is the one **confirmed
  working** against the Ploom aura unit — see below for why. Requires
  flashing a microcontroller.
- **`rawhci`**: a from-scratch BLE central
  ([`internal/hcible`](internal/hcible)) that opens the adapter directly via
  a raw `HCI_CHANNEL_USER` socket, bypassing BlueZ/bluetoothd entirely and
  controlling the exact HCI/SMP command sequence (in particular: it sends
  the SMP Pairing Request immediately upon connecting, before anything
  else, and never sends "LE Read Remote Used Features" or any other
  housekeeping command first). No extra hardware needed, but needs
  root/`CAP_NET_RAW`. **⚠️ Confirmed on real hardware to be able to wedge
  the Bluetooth USB device itself** (recoverable with a driver reload, see
  "Known device behavior" below) - don't run it against your only Bluetooth
  adapter.

## Flow

```mermaid
flowchart TD
    A[Get Bluetooth adapter / open serial bridge] --> B[Start BLE scan]
    B --> C{Name contains pattern?}
    C -- no --> B
    C -- yes --> D[Found: name + MAC]
    D --> E{Connect? [y/N]}
    E -- N --> Z[Abort]
    E -- y --> F[Connect + pair]
    F --> G[GATT discovery]
    G --> H[Save device + GATT table to config.toml]
    H --> I[Start REPL]
```

## Build

```sh
go build -o ploom-cli .
```

## Usage

```sh
./ploom-cli [-name Ploom] [-timeout 30s] [-yes] [-config PATH] \
            [-transport bluez [-retries 3] [-retry-delay 2s] [-adapter hci1]] \
            [-transport serial -port /dev/ttyACM0 [-baud 115200]] \
            [-transport rawhci [-adapter hci0]]
```

- `-name` — substring to match against the advertised local name
  (case-insensitive). Default `Ploom`.
- `-timeout` — how long to scan before giving up. Default `30s`.
- `-yes` — skip the `Connect to ...? [y/N]` prompt.
- `-config` — override the config.toml path. Default
  `$XDG_CONFIG_HOME/ploom-cli/config.toml` (usually
  `~/.config/ploom-cli/config.toml`).
- `-transport` — `bluez` (default), `serial`, or `rawhci`. See intro above.
- `-retries` / `-retry-delay` — connection attempts / delay between them.
  `-transport bluez` only. Default `3` / `2s`.
- `-adapter` — Bluetooth adapter to use, e.g. `hci1` for a second USB
  dongle. `-transport bluez`/`rawhci` only. Default: system default
  (normally `hci0`).
- `-port` — serial port for `-transport serial`, e.g. `/dev/ttyACM0`.
  **Required** for that transport.
- `-baud` — serial baud rate for `-transport serial`. Default `115200`
  (must match `tools/M5central`'s `Serial.begin(...)`).

On a match it prints `Found <pattern>: <name> (<MAC>)`, prompts to connect,
then on confirmation connects (retrying up to `-retries` times on
`-transport bluez`), walks every GATT service/characteristic, saves a
snapshot of the device (name, address, address type, service/characteristic
UUIDs, timestamp) to `config.toml`, and drops into a REPL. On `-transport
bluez`, if every connection attempt fails, it prints a diagnostic hint (see
"Known device behavior" below) instead of a bare error.

## REPL

```
ploom> help
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
```

`<idx>` is a flat index across all characteristics of all services, in
discovery order — run `services` to see the mapping.

`notifyall on` is useful for reverse-engineering the device's protocol:
subscribe to everything, then physically operate the device (press a
button, take a puff, plug in charging) and watch which handle fires and
with what payload. **Observed**: the Ploom aura's GATT table itself
differs by bonding state — an unbonded connection exposed a *different*
9-service/16-characteristic profile (generic Battery/Current Time
services, different vendor UUIDs) than a bonded one (5
services/21 characteristics, including the `53654010-...` and `0xfef5`
vendor services). Re-run discovery after a fresh pairing if characteristics
you expect are missing.

## Known device behavior

Scanning correctly finds real Ploom devices (verified against a "Ploom aura"
unit: `Ploom aura 802029KL`, address type `public`, single vendor-specific
service `53654010-a391-4a65-83fa-bc58084aca28`). The connect step, however,
fails on that unit with `le-connection-abort-by-local` (and pairing fails
with `AuthenticationCanceled`) — reproducible with plain `bluetoothctl
connect`/`pair` too, so it's a BlueZ/device-level negotiation issue, not a
ploom-cli bug.

A `sudo btmon` capture during a `bluetoothctl pair` attempt confirms this at
the link layer: `LE Enhanced Connection Complete` reports `Status: Success`,
but ~85ms later (almost exactly 2 connection events at BlueZ's default 45ms
interval) the peripheral itself sends `LL_TERMINATE_IND` (`Disconnect
Complete` reason: `Remote User Terminated Connection`) — with zero
L2CAP/SMP/ATT packets exchanged in between.

**Interval hypothesis, tested and falsified.** An Android HCI snoop log of
the vendor app both *reconnecting* to, and *freshly pairing* with, the same
unit showed it connecting with a **15ms connection interval / 5s supervision
timeout**, vs BlueZ's defaults of **45ms / 420ms** — and BlueZ's `main.conf`
`[LE]` interval settings turned out to be dead config for this codepath
anyway: `src/device.c`'s `device_connect_le()` calls `bt_io_connect()` with
no interval options at all (confirmed by reading the BlueZ 5.87 source), so
the kernel's own hardcoded default (30-50ms) is used regardless of
`main.conf` (a known upstream bug, [bluez/bluez#293][gh293]). The one thing
that *does* reach the actual HCI command is loading a per-device override
directly into the kernel via the mgmt socket (`MGMT_OP_LOAD_CONN_PARAM`,
opcode `0x0035`) — doing that with the exact values Android uses (min/max
interval `12` = 15ms, timeout `500` = 5s) produced an `LE Extended Create
Connection` that genuinely requested and got a 15ms interval... and the
device **still** disconnected, now at ~33ms — proportionally the same ~2
connection events, just faster because the interval was shorter. The
peripheral's patience is event-count-based, not wall-clock-based, so a
faster interval doesn't help at all.

**Current best lead.** Android's snoop log shows its stack also sends "LE
Read Remote Features" right after connecting (same as BlueZ's own "LE Read
Remote Used Features"), but it receives the peripheral's SMP Security
Request *while that command is still outstanding*. In every one of our own
captures, the exact same BlueZ/kernel command's completion event arrives
carrying the disconnect status — no incoming ACL/SMP data is ever seen
first. That points at this host's controller/driver, or BlueZ/the kernel's
fixed post-connect command sequence, delaying delivery of the peripheral's
first packet — not at connection parameters, which are provably not the
problem.

**Root cause, narrowed to this host's Intel AX201 controller/driver
(`btintel`), not BlueZ in general.** An M5StickC Plus2 running NimBLE-Arduino
(see [`tools/M5central`](tools/M5central)) connects to the same Ploom aura
unit over and over without issue: pairs, bonds (`bonded=true
encrypted=true`), and discovers the full GATT table (5 services, 21
characteristics, including a `0xfef5` vendor service with 8
read/write/notify characteristics that's almost certainly the real control
interface). NimBLE's connection setup evidently doesn't share whatever
defect is on this host. `-transport serial` in ploom-cli routes through it
— see `tools/M5central/README.md` for the firmware and serial protocol,
`internal/serialble` for the Go client. This is a real fix, not a
workaround: same `ploom-cli` binary, same REPL, same `config.toml`, just a
different transport underneath.

**Confirmed: `-transport bluez` (plain, unmodified, no flags) connects fine
on a second machine with a Realtek RTL8852BU (`btrtl` driver) instead of
the Intel AX201 (`btintel` driver) used for all of the investigation above.**
Same binary, no config changes, connected and reached the REPL on the first
try. This rules out "BlueZ is broken for this device" as a general claim -
every earlier finding in this section (the ~2-event disconnect, the
falsified interval hypothesis, the post-connect-command-timing lead) is
specific to the Intel AX201/`btintel` combination this project was
originally developed against. **If your Bluetooth adapter isn't an Intel
one, try plain `-transport bluez` first** - it may just work, no
microcontroller or root access needed.

**On an Intel AX201 (or other `btintel`-driven controller) specifically:**
every config-level lever (`main.conf`, direct mgmt calls) was tried and
ruled out; the remaining leads are `btintel`'s driver-level timing or the
AX201 controller's own HCI response latency for the post-connect command
sequence, both out of scope to patch here. Use `-transport serial` for this
combination. `bluetoothctl remove <MAC>` before retrying `-transport bluez`
is still worth doing to clear stale bond state, but won't fix the
underlying issue on this controller.

**`-transport rawhci`: an alternative to needing a microcontroller,
written but not verified.** Since the leading theory was BlueZ/the kernel's
fixed command sequence (not the device), `internal/hcible` implements a
from-scratch BLE central directly on a raw `HCI_CHANNEL_USER` socket, so it
never sends "LE Read Remote Used Features" or anything else before pairing
- the very first thing it sends after `LE Enhanced Connection Complete` is
an SMP Pairing Request, proactively, without waiting for the peripheral's
Security Request (which is itself faster than every reference capture in
this document). Pairing is LE Legacy "Just Works" only (no ECDH/LE Secure
Connections implementation - the device's IO capability being
NoInputNoOutput makes LE Legacy pairing spec-conformant regardless of what
the peer supports), and no bonding keys are persisted (every run re-pairs
from scratch).

This was built and unit-tested (`internal/hcible/*_test.go`) in an
environment with no root/`CAP_NET_RAW` access, so it was never connected to
real hardware before being handed off for testing. What is verified: the
SMP `c1`/`s1` Legacy Pairing math matches the Bluetooth Core Spec's official
test vectors (`crypto_test.go`, cross-checked against Linux kernel
`net/bluetooth/smp.c` byte-for-byte); the advertising-data and GATT
discovery parsers are verified against real bytes captured (via
`sudo btmon`/the M5Stick bridge) from the actual Ploom aura unit
(`gap_test.go`, `att_test.go`); and the full SMP pairing state machine's PDU
sequencing/STK derivation is exercised end-to-end over a real Unix
socketpair against a simulated peer.

**⚠️ Real risk, observed on actual hardware: this can wedge the Bluetooth
USB device itself, not just the software.** On first real-hardware test (an
Intel AX201 / `btusb`), a `Connect()` that timed out left the controller
unresponsive to *any* HCI command - not just from this tool, `bluetoothd`'s
own reset-on-shutdown failed too - and `dmesg` showed the failure was at the
**USB level**, below HCI entirely:

```
usb 3-10: device descriptor read/64, error -110
Bluetooth: hci0: HCI reset during shutdown failed
Bluetooth: hci0: Opcode 0x0c03 failed: -110
```

Recovery required reloading the USB driver (`sudo rmmod btusb && sudo
modprobe btusb`); a plain HCI/mgmt-level reset was not enough. The exact
trigger isn't confirmed (Intel controllers have a proprietary firmware/boot
handshake normally handled transparently by `btintel.ko` that a from-scratch
HCI client doesn't replicate; whether that's the cause here or something
else is untested), so treat this as a known hazard rather than a fixed bug.
`Connect()`'s timeout path has since been hardened to avoid leaving a
connection dangling on the controller if it completes right as the timeout
fires (a plausible contributor), but this has **not been re-verified against
hardware** and the underlying trigger may not be fully addressed.

**Recommendation:** don't run `-transport rawhci` against your only/primary
Bluetooth adapter. If you want to keep testing it, use a spare USB
Bluetooth dongle (`-adapter hci1`) you don't mind having to reset via driver
reload, ideally a non-Intel one (e.g. CSR8510/Realtek) that doesn't need a
post-reset firmware upload. If a run does wedge the adapter: `sudo rmmod
btusb && sudo modprobe btusb` (adjust the module name if it's a different
chipset's driver), then confirm recovery with `systemctl status bluetooth`
before retrying anything.

[gh293]: https://github.com/bluez/bluez/issues/293

## License

GPL-3.0-or-later - see [LICENSE](LICENSE). In the spirit of `iqos-cli`,
which this project takes its structure from.
