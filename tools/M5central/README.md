# M5central — M5StickC Plus2 を BLE セントラルにする

M5StickC Plus2 自身が **セントラル（接続する側）** として動作し、本体の
液晶＋2ボタンで手動操作できるほか、USBシリアル経由でホスト側ツール
（[ploom-cli](../../README.md) の `-transport serial`）から headless に
駆動することもできる。

**実機で実証済み**: このプロジェクトが実際に動かなかった理由は
Linux の BlueZ 側のバグ（`Device1.Connect()` が接続インターバルを
30-50ms 固定でハードコードしていて、対象の Ploom aura デバイスが
その遅さに痺れを切らして毎回~2接続イベントで切断する）で、BlueZ側では
どうやっても直せなかった。一方この M5StickC Plus2 (NimBLE-Arduino) から
接続すると **毎回問題なく** ペアリング・ボンディング・GATT全読み出しまで
完走する。詳細は [ploom-cli の README](../../README.md#known-device-behavior)。

```
M5StickC Plus2
  │ BLE scan (継続)
  ▼
液晶に発見デバイス一覧を表示 ─── or ─── ホストからシリアルで SCAN/CONNECT
  │ BtnA: カーソル移動   BtnB: 選択して接続
  ▼
connect
  │
  ▼
pair / secure connection
  │
  ▼
GATT discovery (全サービス/キャラクタリスティックを列挙、Serialへ
人間可読ログ + "#..."プロトコル行の両方で出力)
```

ライブラリは `h2zero/NimBLE-Arduino` (2.x API) + `m5stack/M5Unified`。
実機（M5StickC Plus2, ESP32-PICO-V3-02）でビルド・書き込み・シリアルログ
での動作確認済み（液晶操作自体は目視未確認、下記「確認できたこと」参照）。

## ビルド / 書き込み

```sh
pio run                                        # ビルド
pio run -t upload --upload-port /dev/ttyACM0   # 書き込み
pio device monitor -p /dev/ttyACM0 -b 115200   # ログ確認
```

ボードは PlatformIO の公式レジストリに `m5stickc_plus2` が無いため、
`boards/m5stickc_plus2.json`（8MB flash / ESP32-PICO 用の自前定義）を
プロジェクトに同梱している。

## 操作方法

起動すると自動的に BLE スキャンを継続し、見つけたデバイスを液晶に一覧表示する。

| 状態 | BtnA (前面ボタン) | BtnB (側面ボタン) |
|---|---|---|
| 一覧表示中 | 短押し: カーソルを次のデバイスへ<br>長押し(約0.5秒): 一覧クリア＋再スキャン | 短押し: カーソルのデバイスに接続開始 |
| 接続中 | 短押し/長押し: 切断して一覧に戻る | 短押し: GATT テーブルを Serial に再ダンプ |

一覧の各行は `> * DeviceName -60dB` のような表示。
- `>` = 現在のカーソル（反転表示）
- `*` = 広告名が `TARGET_NAME_FILTER`（デフォルト `"ploom"`、大文字小文字
  区別なしの部分一致）にマッチしたことを示す黄色マーカー（選択の目印。
  自動接続はしない）
- 名前が無い広告は MAC アドレスを表示

接続すると、
1. `secureConnection()` でペアリング/暗号化を試行
   （IO capability は `NO_INPUT_OUTPUT` = Just Works。ボタン/画面が無い
   デバイス想定。パスキー方式が必要なら `SECURITY_MITM` /
   `setSecurityIOCap` を変更する）
2. 暗号化できてもできなくても続けて GATT discovery を実行し、
   全サービス・キャラクタリスティック・ディスクリプタと読み取れる値を
   Serial に列挙する。
3. 液晶に `Connected` とデバイス名・アドレス・サービス数を表示する。

切断すると自動的に一覧表示に戻り、スキャンを再開する
（それまでに見つけたデバイス一覧は保持される）。

**Ploom の実際の広告名が `"Ploom..."` でない場合**でも一覧には出るので、
一覧を見ながらカーソルを合わせて BtnB で接続すればよい
（`*` マークは目印であり必須ではない）。名前が分かったら
`src/main.cpp` の `TARGET_NAME_FILTER` を書き換えると目印が付くようになる。

## シリアルプロトコル（ploom-cli 連携用）

ボタン操作と併用可能な形で、USBシリアル(115200 8N1)経由のコマンド
プロトコルも実装済み。ファームウェアが送る行のうち **`#` で始まる行だけが
プロトコル行**（それ以外は人間向けのデバッグログで、無視して構わない）。
ホストからのコマンドは1行1コマンド、プレフィックス無しの平文。

| コマンド (ホスト→本体) | 応答/挙動 |
|---|---|
| `PING` | `#PONG` |
| `SCAN` | スキャン開始。見つかる度に `#ADV` を出力し続ける（`STOP`まで） |
| `STOP` | スキャン停止、`#SCANEND` |
| `CONNECT <addr>` | 直前の`SCAN`で見つけたアドレスに接続→ペア→GATT探索。下記イベント列を出力 |
| `DISCONNECT` | `#OK DISCONNECT` or `#ERR DISCONNECT ...` |
| `READ <handle>` | `#VAL <handle> <hex\|->` or `#ERR READ ...` |
| `WRITE <handle> <hex>` | Write with response。`#OK WRITE` or `#ERR` |
| `WRITEC <handle> <hex>` | Write without response。`#OK WRITEC` or `#ERR` |
| `NOTIFY <handle> ON\|OFF` | 購読開始/停止。`#OK NOTIFY` or `#ERR`。以降の通知は非同期の`#VAL <handle> <hex>` |
| `MTU` | `#MTU <n>` |

本体からのイベント（`#`始まり、非同期含む）:

```
#READY <name>                                  起動時
#ADV <addr> <rssi> <uuids_csv_or_-> <name...>   スキャン中、1広告ごと
#CONNECTING <addr>
#CONNECTED <addr>
#CONNECTFAIL <addr>
#AUTH bonded=0|1 encrypted=0|1 authenticated=0|1
#SVC <uuid> <start_handle> <end_handle>         サービスごと
#CHR <uuid> <handle> <props>                    キャラクタリスティックごと（propsはBLE標準のプロパティビットマスク10進数）
#DISCOVERDONE                                   GATT探索完了
#DISCONNECTED reason=<n>                        切断時（いつでも非同期で来る）
```

ホスト側の実装は [`internal/serialble`](../../internal/serialble/serialble.go)。
使い方: `ploom-cli -transport serial -port /dev/ttyACM0`。

**注意**: USBシリアルチップによっては、ポートを開いただけでは本体が
リセットされない（この基板のCH9102では実際にリセットされなかった）。
ホスト側はそのケースに備えて起動直後の`#READY`だけでなく、`PING`への
`#PONG`応答も「生きている」判定に使う実装にしてある。

## 実機確認済み: Ploom aura への接続成功

`ploom-cli -transport serial -port /dev/ttyACM0 -name Ploom -yes` で実際の
Ploom aura 802029KL に対し、スキャン→発見→接続→ペアリング→ボンディング→
GATT全探索→`ploom-cli`のREPLでの`read`/`write`/`mtu`/`disconnect`まで
一通り動作確認済み：

```
connected. bonded=true encrypted=true authenticated=false
found 5 services, 21 characteristics.
```

発見された5サービス: `0x1800`(GAP), `0x1801`(GATT), `0x180a`(Device
Information: Manufacturer="JT", Model="Ploom aura", FW/SW Revision),
`53654010-...`(ベンダー固有、write+notifyの2キャラクタリスティック),
`0xfef5`(ベンダー固有、8キャラクタリスティック、write/notify/read混在
— おそらくこれが実際の制御プロトコル)。

RAM 12%, Flash 24% 使用（8MB flash 環境で余裕あり）。継続スキャンで
新規デバイスが見つかるたびに一覧再描画コードが走るが、十数個の実デバイス
検出下でもクラッシュ・リセットループなし。

**未確認（環境上の制約）**: 物理ボタン(BtnA/BtnB)での手動操作フローと
液晶の表示内容は目視未確認（`ploom-cli -transport serial`経由の
シリアルコマンドフローでの動作は実機で確認済み）。

## 実際の Ploom で試す手順

1. Ploom デバイスの電源を入れ、BLE 広告状態にする（吸引 or ボタン操作で
   Bluetooth 接続待受になる機種が多い）。
2. M5StickC Plus2 の液晶に一覧が出るので、BtnA でカーソルを合わせ
   （`*` が付いていればそれが候補）、BtnB で接続。
3. 自動で pair → GATT discovery まで進む。液晶に `Connected` と表示され、
   Serial ログに全サービス/characteristic が出力される。ペアリングに
   失敗しても GATT discovery 自体は走るので、暗号化なしで読める
   characteristic があるかも Serial ログで分かる。
