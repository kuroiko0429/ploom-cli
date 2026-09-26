/**
 * M5StickC Plus2 as a BLE Central, with an on-device selection menu AND a
 * line-based serial command protocol so a host tool (see ../../../ploom-cli,
 * -transport serial) can drive it headlessly.
 *
 *   M5StickC Plus2
 *     |  BLE scan (continuous)
 *     v
 *   list of discovered devices shown on the LCD
 *     |  BtnA: move cursor   BtnB: select & connect
 *     |  (or: serial "SCAN"/"CONNECT <addr>" from a host)
 *     v
 *   connect
 *     |
 *     v
 *   pair / secure connection
 *     |
 *     v
 *   GATT discovery (services / characteristics -> Serial, both as
 *   human-readable debug lines and as "#..." protocol lines)
 *
 * Library: h2zero/NimBLE-Arduino (2.x API).
 *
 * ---------------------------------------------------------------------
 * Serial protocol (115200 8N1)
 * ---------------------------------------------------------------------
 * Every line the firmware prints that starts with '#' is a protocol event
 * meant for a host tool to parse; every other line is a free-form debug
 * log (safe to print to a human console, ignored by a protocol parser).
 * Commands sent TO the firmware are plain text lines, no prefix, one per
 * line, case-insensitive command word:
 *
 *   PING                     -> #PONG
 *   SCAN                     -> starts scanning; each advertisement seen
 *                                is reported as it arrives (see below);
 *                                keeps running until STOP
 *   STOP                     -> stops scanning, replies #SCANEND
 *   CONNECT <addr>           -> connect + pair + discover the device with
 *                                this address (must have been seen by a
 *                                preceding SCAN); see event sequence below
 *   DISCONNECT               -> #OK DISCONNECT or #ERR DISCONNECT ...
 *   READ <handle>            -> #VAL <handle> <hex|-> or #ERR READ ...
 *   WRITE <handle> <hex>     -> write with response;    #OK WRITE or #ERR
 *   WRITEC <handle> <hex>    -> write without response; #OK WRITEC or #ERR
 *   NOTIFY <handle> ON|OFF   -> subscribe/unsubscribe;  #OK NOTIFY or #ERR
 *                                (subsequent notifications arrive as
 *                                unsolicited #VAL <handle> <hex> lines)
 *   MTU                      -> #MTU <n> or #ERR MTU ...
 *
 * Events FROM the firmware (unsolicited unless noted):
 *   #READY <name>                                  on boot
 *   #ADV <addr> <rssi> <uuids_csv_or_-> <name...>   one per advertisement
 *                                                   seen while scanning
 *                                                   (name is free text to
 *                                                   end of line, may be
 *                                                   empty)
 *   #CONNECTING <addr>
 *   #CONNECTED <addr>
 *   #CONNECTFAIL <addr>
 *   #AUTH bonded=0|1 encrypted=0|1 authenticated=0|1
 *   #SVC <uuid> <start_handle> <end_handle>         one per service
 *   #CHR <uuid> <handle> <props>                    one per characteristic
 *                                                    (props: standard BLE
 *                                                    GATT properties
 *                                                    bitmask, decimal)
 *   #DISCOVERDONE                                   GATT discovery finished
 *   #DISCONNECTED reason=<n>                        connection lost, any
 *                                                    time, expected or not
 */

#include <M5Unified.h>
#include <NimBLEDevice.h>

#include <algorithm>
#include <cctype>
#include <vector>

// ---------------------------------------------------------------------------
// User-tunable settings
// ---------------------------------------------------------------------------

// Case-insensitive substring match against the advertised device name, used
// only to mark likely candidates with a "*" in the on-device list. Selection
// for the on-device UI is always manual (BtnA/BtnB) - this is just a visual
// hint. The serial CONNECT command connects by exact address instead.
static const char* TARGET_NAME_FILTER = "ploom";

static const uint32_t SCAN_TIME_MS  = 0; // 0 = scan forever until stopped
static const int      VISIBLE_ROWS  = 9;
static const int      ROW_HEIGHT_PX = 12;
static const int      LIST_TOP_PX   = 18;

// Security: most consumer BLE gadgets without a screen/keyboard use "Just
// Works" pairing (no MITM protection). Switch these if your device needs
// passkey / numeric-comparison pairing.
static const bool SECURITY_BONDING = true;
static const bool SECURITY_MITM    = false;
static const bool SECURITY_SC      = true; // LE Secure Connections when supported

// ---------------------------------------------------------------------------

enum class AppState { BROWSING, CONNECTING, CONNECTED };
static AppState state = AppState::BROWSING;

struct DeviceEntry {
    const NimBLEAdvertisedDevice* dev;
    std::string                   addr; // dedup key
};

static std::vector<DeviceEntry> deviceList;
static int                      cursor    = 0;
static bool                     listDirty = true;
static NimBLEClient*            pClient   = nullptr;

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

static bool containsIgnoreCase(const std::string& hay, const std::string& needle) {
    if (needle.empty()) return true;
    std::string h = hay;
    std::string n = needle;
    std::transform(h.begin(), h.end(), h.begin(), [](unsigned char c) { return std::tolower(c); });
    std::transform(n.begin(), n.end(), n.begin(), [](unsigned char c) { return std::tolower(c); });
    return h.find(n) != std::string::npos;
}

static bool isMatch(const NimBLEAdvertisedDevice* d) {
    return d->haveName() && containsIgnoreCase(d->getName(), TARGET_NAME_FILTER);
}

static String labelFor(const NimBLEAdvertisedDevice* d) {
    if (d->haveName() && !d->getName().empty()) return String(d->getName().c_str());
    return String(d->getAddress().toString().c_str());
}

static String toHexAscii(const std::string& v) {
    String hex, ascii;
    for (unsigned char b : v) {
        char buf[4];
        snprintf(buf, sizeof(buf), "%02X ", b);
        hex += buf;
        ascii += isprint(b) ? (char)b : '.';
    }
    return hex + " | " + ascii;
}

static String hexEncode(const uint8_t* data, size_t len) {
    static const char digits[] = "0123456789abcdef";
    String            out;
    out.reserve(len * 2);
    for (size_t i = 0; i < len; ++i) {
        out += digits[(data[i] >> 4) & 0xF];
        out += digits[data[i] & 0xF];
    }
    return out;
}

static String hexEncode(const std::string& data) {
    return hexEncode(reinterpret_cast<const uint8_t*>(data.data()), data.size());
}

static std::string hexDecode(const String& hexStr) {
    auto        nibble = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return 0;
    };
    std::string out;
    out.reserve(hexStr.length() / 2);
    for (int i = 0; i + 1 < (int)hexStr.length(); i += 2) {
        out += (char)((nibble(hexStr[i]) << 4) | nibble(hexStr[i + 1]));
    }
    return out;
}

// Standard BLE GATT characteristic properties bitmask (Core spec Vol 3,
// Part G, 3.3.1.1). NimBLE-Arduino only exposes these via can*() booleans on
// NimBLERemoteCharacteristic, not as a raw byte, so it's reconstructed here.
static uint8_t propsBitmask(NimBLERemoteCharacteristic* chr) {
    uint8_t p = 0;
    if (chr->canBroadcast()) p |= 0x01;
    if (chr->canRead()) p |= 0x02;
    if (chr->canWriteNoResponse()) p |= 0x04;
    if (chr->canWrite()) p |= 0x08;
    if (chr->canNotify()) p |= 0x10;
    if (chr->canIndicate()) p |= 0x20;
    if (chr->canWriteSigned()) p |= 0x40;
    if (chr->hasExtendedProps()) p |= 0x80;
    return p;
}

// ---------------------------------------------------------------------------
// Display
// ---------------------------------------------------------------------------

static void showStatus(const String& l1, const String& l2 = "", const String& l3 = "", const String& l4 = "") {
    auto& d = M5.Display;
    d.startWrite();
    d.fillScreen(TFT_BLACK);
    d.setCursor(0, 0);
    d.setTextSize(2);
    d.setTextColor(TFT_GREEN, TFT_BLACK);
    d.println(l1);
    d.setTextSize(1);
    d.setTextColor(TFT_WHITE, TFT_BLACK);
    if (l2.length()) d.println(l2);
    if (l3.length()) d.println(l3);
    if (l4.length()) d.println(l4);
    d.endWrite();
}

static void renderList() {
    auto& d = M5.Display;
    d.startWrite();
    d.fillScreen(TFT_BLACK);

    d.setTextSize(2);
    d.setTextColor(TFT_CYAN, TFT_BLACK);
    d.setCursor(0, 0);
    d.printf("Scan: %d dev\n", (int)deviceList.size());

    d.setTextSize(1);

    if (deviceList.empty()) {
        d.setTextColor(TFT_DARKGREY, TFT_BLACK);
        d.setCursor(0, LIST_TOP_PX);
        d.println("(searching...)");
        d.setCursor(0, 126);
        d.setTextColor(TFT_WHITE, TFT_BLACK);
        d.print("A-hold: clear/rescan");
        d.endWrite();
        return;
    }

    int count = (int)deviceList.size();
    int start = cursor - VISIBLE_ROWS / 2;
    start     = std::max(0, std::min(start, std::max(0, count - VISIBLE_ROWS)));

    for (int i = start; i < count && i < start + VISIBLE_ROWS; ++i) {
        int  y       = LIST_TOP_PX + (i - start) * ROW_HEIGHT_PX;
        bool sel      = (i == cursor);
        bool matched = isMatch(deviceList[i].dev);

        if (sel) {
            d.fillRect(0, y, d.width(), ROW_HEIGHT_PX, TFT_WHITE);
            d.setTextColor(TFT_BLACK, TFT_WHITE);
        } else {
            d.setTextColor(matched ? TFT_YELLOW : TFT_WHITE, TFT_BLACK);
        }

        String line = String(sel ? ">" : " ") + (matched ? "*" : " ") + labelFor(deviceList[i].dev) + " " +
                      String(deviceList[i].dev->getRSSI()) + "dB";
        d.setCursor(0, y);
        d.print(line);
    }
    d.endWrite();
}

// ---------------------------------------------------------------------------
// GATT discovery
// ---------------------------------------------------------------------------

static void dumpServices(NimBLEClient* c) {
    Serial.println("==== GATT discovery ====");
    const auto& services = c->getServices(true);
    Serial.printf("Found %u service(s)\n", (unsigned)services.size());

    for (auto* svc : services) {
        Serial.printf("[Service] %s\n", svc->toString().c_str());
        Serial.print("#SVC ");
        Serial.print(svc->getUUID().toString().c_str());
        Serial.print(' ');
        Serial.print(svc->getStartHandle());
        Serial.print(' ');
        Serial.println(svc->getEndHandle());

        const auto& chars = svc->getCharacteristics(true);
        for (auto* chr : chars) {
            Serial.printf("  [Char] %s\n", chr->toString().c_str());
            Serial.print("#CHR ");
            Serial.print(chr->getUUID().toString().c_str());
            Serial.print(' ');
            Serial.print(chr->getHandle());
            Serial.print(' ');
            Serial.println(propsBitmask(chr));

            if (chr->canRead()) {
                std::string val = chr->readValue();
                Serial.printf("    value: %s\n", toHexAscii(val).c_str());
            }
            const auto& descs = chr->getDescriptors(true);
            for (auto* dsc : descs) {
                Serial.printf("    [Desc] %s\n", dsc->toString().c_str());
            }
        }
    }
    Serial.println("==== end of GATT discovery ====");
    Serial.println("#DISCOVERDONE");
}

// ---------------------------------------------------------------------------
// Client (connection) callbacks
// ---------------------------------------------------------------------------

class ClientCallbacks : public NimBLEClientCallbacks {
    void onConnect(NimBLEClient* c) override { Serial.println("[BLE] Connected (link layer)"); }

    void onDisconnect(NimBLEClient* c, int reason) override {
        Serial.printf("[BLE] Disconnected, reason=%d - back to device list\n", reason);
        Serial.printf("#DISCONNECTED reason=%d\n", reason);
        state     = AppState::BROWSING;
        listDirty = true;
        // isContinue=true: keep the already-discovered device list intact.
        NimBLEDevice::getScan()->start(SCAN_TIME_MS, true, true);
    }

    // Peer wants us to type in the passkey shown on its display. Without a
    // keyboard/display we cannot supply a real value; log and let it time
    // out rather than guessing a fake key.
    void onPassKeyEntry(NimBLEConnInfo& connInfo) override {
        Serial.println("[BLE] Peer requested passkey entry - not supported on this device");
        showStatus("Pairing failed", "Peer needs passkey", "entry (unsupported)");
    }

    // Numeric comparison pairing: peer shows a 6-digit code and expects a
    // yes/no confirmation. Auto-accept since there is no UI to compare
    // against; log the code to Serial for anyone who wants to verify it.
    void onConfirmPasskey(NimBLEConnInfo& connInfo, uint32_t passkey) override {
        Serial.printf("[BLE] Confirm passkey: %06" PRIu32 " (auto-accepting)\n", passkey);
        NimBLEDevice::injectConfirmPasskey(connInfo, true);
    }

    void onAuthenticationComplete(NimBLEConnInfo& connInfo) override {
        Serial.printf("[BLE] Auth complete: bonded=%d encrypted=%d authenticated=%d\n", connInfo.isBonded(),
                       connInfo.isEncrypted(), connInfo.isAuthenticated());
    }
} clientCallbacks;

// ---------------------------------------------------------------------------
// Scan callbacks
// ---------------------------------------------------------------------------

static void emitAdv(const NimBLEAdvertisedDevice* dev) {
    Serial.print("#ADV ");
    Serial.print(dev->getAddress().toString().c_str());
    Serial.print(' ');
    Serial.print(dev->getRSSI());
    Serial.print(' ');
    if (dev->haveServiceUUID()) {
        for (uint8_t i = 0; i < dev->getServiceUUIDCount(); ++i) {
            if (i) Serial.print(',');
            Serial.print(dev->getServiceUUID(i).toString().c_str());
        }
    } else {
        Serial.print('-');
    }
    Serial.print(' ');
    if (dev->haveName()) Serial.println(dev->getName().c_str());
    else Serial.println();
}

class ScanCallbacks : public NimBLEScanCallbacks {
    void onResult(const NimBLEAdvertisedDevice* dev) override {
        Serial.printf("[SCAN] %s\n", dev->toString().c_str());
        if (state != AppState::BROWSING) return; // ignore stray results while (dis)connecting

        emitAdv(dev);

        std::string addr = dev->getAddress().toString();
        for (auto& e : deviceList) {
            if (e.addr == addr) return; // already tracked; same pointer gets updated in place by NimBLE
        }
        deviceList.push_back({dev, addr});
        listDirty = true;
    }

    void onScanEnd(const NimBLEScanResults& results, int reason) override {
        Serial.printf("[SCAN] Ended, reason=%d, %d device(s)\n", reason, results.getCount());
    }
} scanCallbacks;

// ---------------------------------------------------------------------------
// Connect + pair + discover
// ---------------------------------------------------------------------------

// Shared by both the on-device BtnB flow and the serial CONNECT command.
static bool doConnect(const NimBLEAdvertisedDevice* target, const String& name, const String& addr) {
    state = AppState::CONNECTING;
    NimBLEDevice::getScan()->stop();
    Serial.println("#SCANEND");

    showStatus("Connecting...", name, addr);
    Serial.printf("[BLE] Connecting to %s (%s)\n", name.c_str(), addr.c_str());
    Serial.print("#CONNECTING ");
    Serial.println(addr);

    pClient = NimBLEDevice::createClient();
    pClient->setClientCallbacks(&clientCallbacks, false);
    pClient->setConnectTimeout(10 * 1000);

    if (!pClient->connect(target)) {
        Serial.println("[BLE] Connect failed");
        Serial.print("#CONNECTFAIL ");
        Serial.println(addr);
        NimBLEDevice::deleteClient(pClient);
        pClient = nullptr;
        state   = AppState::BROWSING;
        NimBLEDevice::getScan()->start(SCAN_TIME_MS, true, true);
        return false;
    }
    Serial.print("#CONNECTED ");
    Serial.println(addr);

    // ---- pair / secure connection --------------------------------------
    showStatus("Pairing...", name, addr);
    bool           secOk = pClient->secureConnection();
    NimBLEConnInfo info  = pClient->getConnInfo();
    Serial.printf("[BLE] secureConnection()=%d bonded=%d encrypted=%d authenticated=%d\n", secOk, info.isBonded(),
                  info.isEncrypted(), info.isAuthenticated());
    Serial.printf("#AUTH bonded=%d encrypted=%d authenticated=%d\n", info.isBonded() ? 1 : 0, info.isEncrypted() ? 1 : 0,
                  info.isAuthenticated() ? 1 : 0);
    if (!info.isEncrypted()) {
        Serial.println("[BLE] Warning: link is not encrypted (device may not support/require pairing) - continuing");
    }

    // ---- GATT discovery --------------------------------------------------
    showStatus("GATT discovery...", name, addr);
    dumpServices(pClient); // also emits #SVC/#CHR/#DISCOVERDONE

    state = AppState::CONNECTED;
    showStatus("Connected", name, addr, String(pClient->getServices().size()) + " service(s), see Serial");
    return true;
}

static bool connectToSelected() {
    if (deviceList.empty() || cursor < 0 || cursor >= (int)deviceList.size()) return false;

    const NimBLEAdvertisedDevice* target = deviceList[cursor].dev;
    String                        name   = target->haveName() ? target->getName().c_str() : "(no name)";
    String                        addr   = target->getAddress().toString().c_str();
    return doConnect(target, name, addr);
}

static const NimBLEAdvertisedDevice* findByAddr(const std::string& addr) {
    for (auto& e : deviceList) {
        if (e.addr == addr) return e.dev;
    }
    return nullptr;
}

static NimBLERemoteCharacteristic* findCharByHandle(uint16_t handle) {
    if (!pClient) return nullptr;
    for (auto* svc : pClient->getServices()) {
        for (auto* chr : svc->getCharacteristics()) {
            if (chr->getHandle() == handle) return chr;
        }
    }
    return nullptr;
}

// ---------------------------------------------------------------------------
// Serial command protocol
// ---------------------------------------------------------------------------

static void sendOK(const char* ctx) {
    Serial.print("#OK ");
    Serial.println(ctx);
}

static void sendErr(const char* ctx, const String& msg) {
    Serial.print("#ERR ");
    Serial.print(ctx);
    Serial.print(' ');
    Serial.println(msg);
}

static void handleSerialLine(String line) {
    line.trim();
    if (line.length() == 0) return;

    int    sp   = line.indexOf(' ');
    String cmd  = (sp < 0) ? line : line.substring(0, sp);
    String rest = (sp < 0) ? String("") : line.substring(sp + 1);
    cmd.toUpperCase();
    rest.trim();

    if (cmd == "PING") {
        Serial.println("#PONG");
        return;
    }

    if (cmd == "SCAN") {
        if (state != AppState::BROWSING) {
            sendErr("SCAN", "busy");
            return;
        }
        NimBLEDevice::getScan()->stop();
        deviceList.clear();
        cursor    = 0;
        listDirty = true;
        NimBLEDevice::getScan()->start(SCAN_TIME_MS, false, true);
        return;
    }

    if (cmd == "STOP") {
        NimBLEDevice::getScan()->stop();
        Serial.println("#SCANEND");
        return;
    }

    if (cmd == "CONNECT") {
        if (state != AppState::BROWSING) {
            sendErr("CONNECT", "busy");
            return;
        }
        const NimBLEAdvertisedDevice* target = findByAddr(std::string(rest.c_str()));
        if (!target) {
            sendErr("CONNECT", "unknown address - scan first");
            return;
        }
        String name = target->haveName() ? target->getName().c_str() : "(no name)";
        doConnect(target, name, rest);
        return;
    }

    if (cmd == "DISCONNECT") {
        if (pClient && pClient->isConnected()) {
            pClient->disconnect();
            sendOK("DISCONNECT");
        } else {
            sendErr("DISCONNECT", "not connected");
        }
        return;
    }

    if (cmd == "READ") {
        uint16_t handle = (uint16_t)rest.toInt();
        auto*    chr    = findCharByHandle(handle);
        if (!chr) {
            sendErr("READ", "unknown handle");
            return;
        }
        std::string val = chr->readValue();
        Serial.print("#VAL ");
        Serial.print(handle);
        Serial.print(' ');
        Serial.println(val.empty() ? String("-") : hexEncode(val));
        return;
    }

    if (cmd == "WRITE" || cmd == "WRITEC") {
        int sp2 = rest.indexOf(' ');
        if (sp2 < 0) {
            sendErr(cmd == "WRITE" ? "WRITE" : "WRITEC", "usage: <handle> <hex>");
            return;
        }
        uint16_t handle = (uint16_t)rest.substring(0, sp2).toInt();
        String   hexStr = rest.substring(sp2 + 1);
        hexStr.trim();
        auto* chr = findCharByHandle(handle);
        if (!chr) {
            sendErr(cmd.c_str(), "unknown handle");
            return;
        }
        std::string data = hexDecode(hexStr);
        bool ok = chr->writeValue(reinterpret_cast<const uint8_t*>(data.data()), data.size(), cmd == "WRITE");
        if (ok) sendOK(cmd.c_str());
        else sendErr(cmd.c_str(), "write failed");
        return;
    }

    if (cmd == "NOTIFY") {
        int sp2 = rest.indexOf(' ');
        if (sp2 < 0) {
            sendErr("NOTIFY", "usage: <handle> ON|OFF");
            return;
        }
        uint16_t handle = (uint16_t)rest.substring(0, sp2).toInt();
        String   onoff  = rest.substring(sp2 + 1);
        onoff.trim();
        onoff.toUpperCase();
        auto* chr = findCharByHandle(handle);
        if (!chr) {
            sendErr("NOTIFY", "unknown handle");
            return;
        }
        bool ok;
        if (onoff == "ON") {
            ok = chr->subscribe(true, [](NimBLERemoteCharacteristic* c, uint8_t* data, size_t len, bool) {
                Serial.print("#VAL ");
                Serial.print(c->getHandle());
                Serial.print(' ');
                Serial.println(len == 0 ? String("-") : hexEncode(data, len));
            });
        } else if (onoff == "OFF") {
            ok = chr->unsubscribe(true);
        } else {
            sendErr("NOTIFY", "usage: <handle> ON|OFF");
            return;
        }
        if (ok) sendOK("NOTIFY");
        else sendErr("NOTIFY", "failed");
        return;
    }

    if (cmd == "MTU") {
        if (!pClient) {
            sendErr("MTU", "not connected");
            return;
        }
        Serial.print("#MTU ");
        Serial.println(pClient->getMTU());
        return;
    }

    sendErr("CMD", "unknown: " + cmd);
}

static String serialLineBuf;

static void pollSerialCommands() {
    while (Serial.available()) {
        char c = (char)Serial.read();
        if (c == '\n') {
            String line  = serialLineBuf;
            serialLineBuf = "";
            handleSerialLine(line);
        } else if (c != '\r') {
            serialLineBuf += c;
            if (serialLineBuf.length() > 256) serialLineBuf = ""; // guard against line-noise/garbage
        }
    }
}

// ---------------------------------------------------------------------------

void setup() {
    auto cfg = M5.config();
    M5.begin(cfg);
    M5.Display.setRotation(1);

    Serial.begin(115200);
    delay(200);
    Serial.println("\n[BLE] M5StickC Plus2 BLE Central starting");

    showStatus("BLE Central", "Init...");

    NimBLEDevice::init("M5StickC-Central");
    NimBLEDevice::setPower(3); // +3dBm

    // "Just Works" pairing by default (no keyboard/display on this device).
    NimBLEDevice::setSecurityIOCap(BLE_HS_IO_NO_INPUT_OUTPUT);
    NimBLEDevice::setSecurityAuth(SECURITY_BONDING, SECURITY_MITM, SECURITY_SC);

    NimBLEScan* pScan = NimBLEDevice::getScan();
    pScan->setScanCallbacks(&scanCallbacks, false);
    pScan->setActiveScan(true);
    pScan->setInterval(100);
    pScan->setWindow(100);
    pScan->start(SCAN_TIME_MS, false, true);

    Serial.printf("[BLE] Scanning. \"*\" marks names containing \"%s\". BtnA: move/hold=rescan, BtnB: connect\n",
                  TARGET_NAME_FILTER);
    Serial.println("#READY M5StickC-Central");
    listDirty = true;
}

void loop() {
    M5.update();
    pollSerialCommands();
    delay(10);

    switch (state) {
        case AppState::BROWSING:
            if (M5.BtnA.wasHold()) {
                Serial.println("[UI] Clear list + rescan");
                NimBLEDevice::getScan()->stop();
                deviceList.clear();
                cursor    = 0;
                listDirty = true;
                NimBLEDevice::getScan()->start(SCAN_TIME_MS, false, true); // isContinue=false: fresh list
            } else if (M5.BtnA.wasClicked()) {
                if (!deviceList.empty()) {
                    cursor    = (cursor + 1) % (int)deviceList.size();
                    listDirty = true;
                }
            }
            if (M5.BtnB.wasClicked()) {
                if (!connectToSelected()) {
                    showStatus("Connect failed", "Back to list...");
                    delay(800);
                    listDirty = true;
                }
            }
            break;

        case AppState::CONNECTED:
            if (M5.BtnA.wasClicked() || M5.BtnA.wasHold()) {
                Serial.println("[UI] Manual disconnect");
                if (pClient) pClient->disconnect();
            } else if (M5.BtnB.wasClicked()) {
                if (pClient) dumpServices(pClient);
            }
            break;

        case AppState::CONNECTING:
            break; // doConnect() runs to completion before this is ever observed here
    }

    if (state == AppState::BROWSING && listDirty) {
        renderList();
        listDirty = false;
    }
}
