# 03 — Thermal Receipt / Kitchen Ticket Printing

_Researched 2026-11-23. Target: Expo (dev build) Android app in TypeScript, using only free/OSS libraries, with cheap Chinese 58/80 mm ESC/POS printers over Bluetooth Classic (SPP) and LAN (TCP 9100). Receipts must print Nepali (Devanagari)._

## TL;DR

- **Recommended stack:** draw the receipt yourself, encode it in JS, and keep the transport layer thin.
  1. Lay out the receipt with **@shopify/react-native-skia** (offscreen surface). `react-native-view-shot` of a hidden view is the fallback. Read back the RGBA pixels.
  2. Turn them into ESC/POS bytes with **@point-of-sale/receipt-printer-encoder** (MIT, v4.0.1 released 2026-09). Use `.image({data,width,height}, {width: 384|576, algorithm})`, which handles dithering, 8-dot padding and raster (GS v 0) or column (ESC \*) mode. It needs a `structuredClone` polyfill on Hermes.
  3. Send the bytes over **react-native-tcp-socket** (LAN, port 9100) or **react-native-bluetooth-classic** (SPP, takes base64 binary writes). Both are MIT and actively maintained in Nov 2026.
- **Fast-ship alternative:** **@haroldtran/react-native-thermal-printer**. It is one native module covering SPP (the class is named "BLE" but it really opens an RFCOMM SPP socket), Net and USB, and it has `printImageBase64` with GS v 0 and Floyd–Steinberg dithering. But it has **no license** (see risks), one maintainer and 20 stars. Use it as a reference, or vendor it only after the license question is cleared.
- **Rule out react-native-esc-pos-printer** (tr3v3r). It wraps the **Epson ePOS SDK**, so it only works with Epson TM printers, not generic Chinese ESC/POS printers. The library itself is good: New-Arch, Expo-ready, and released 2026-10.
- **Devanagari:** always print the receipt as a raster image. Do not rely on code pages.
- **Server-side printing from the cloud does not work** without a LAN-side agent (see section 3).
- **Printing from the browser:** Web Serial and WebUSB in Chromium (desktop, and Android Chrome 148+) are workable for a counter PC. Web Bluetooth only speaks BLE GATT, so it cannot reach most SPP printers. Safari and Firefox support none of these.

## 1. Candidate table

Stars, open issues and last push dates come from the GitHub API on 2026-11-23. Release dates come from npm.

| Library | Transports | Image / bitmap | Last release / push | Stars / open issues | License | New Arch | Expo plugin | Android 12+ BT perms | Verdict |
|---|---|---|---|---|---|---|---|---|---|
| **@point-of-sale/receipt-printer-encoder** | none (encoder only; pure JS) | Yes. `image()`, dithering (threshold / bayer / floyd-steinberg / atkinson), raster or column mode | 4.0.1, 2026-09-18 | 334 / 7 | MIT | n/a (pure JS) | not needed | n/a | **Use.** Also works in browser and Node, so one encoder covers app, web and agent |
| esc-pos-encoder | encoder | yes | 3.0.0, 2024-09 | 379 / 3 | MIT | n/a | n/a | n/a | Superseded by the package above. Avoid |
| **react-native-tcp-socket** | TCP / TLS | n/a (send raw bytes) | 6.4.3, 2026-09-10 | 399 / 37 | MIT | Legacy bridge module, runs through the interop layer. Issue #187 "Support New Arch" is open | Autolinks, no plugin needed | n/a (INTERNET only) | **Use** for LAN 9100. Watch open bugs #248 and #246 (socket-id crashes after close) |
| **react-native-bluetooth-classic** | BT Classic SPP (Android); iOS needs MFi | n/a (write accepts base64 binary) | 1.73.0-rc.17 on npm (2025-11); repo rc.18, 2026-11-17 | 268 / 21 | MIT | Legacy bridge through interop. Issue #334 "New Arch Support" is open | Autolinks. Add permissions via `expo-build-properties` or app.json `android.permissions` | App must request `BLUETOOTH_CONNECT` / `BLUETOOTH_SCAN` at runtime | **Use** for SPP. Pin the exact rc version |
| @haroldtran/react-native-thermal-printer | SPP (RFCOMM UUID 00001101, despite the "BLEPrinter" name), TCP, USB | Yes. `printImage` / `printImageBase64`, GS v 0 raster, Floyd–Steinberg or Otsu threshold | 1.2.0, 2026-03-04 | 20 / 2 | **None declared** (forked from ISC upstream) | Legacy `ReactPackage`; no codegen | None; its manifest merges BT_CONNECT / SCAN (`neverForLocation`) | Declares the permissions, but the app must still request them at runtime | **Fallback / reference only.** Legal and bus-factor risk |
| react-native-thermal-receipt-printer (HeligPfleigh) | BLE(=SPP), Net, USB | image only in 1.2.0-rc | 1.2.0-rc.2, 2023-12. Last code commit 2024-10 (dependabot) | 199 / 84 | ISC | Legacy | none | Old. Issue #127: PendingIntent crash on Android 12 | Abandoned. Avoid |
| react-native-thermal-receipt-printer-image-qr | same as above | yes | 0.1.12, 2024-09 | 155 / 84 | ISC | Legacy | none | stale | Abandoned. Avoid |
| react-native-bluetooth-escpos-printer (januslo) | SPP | `printPic` | npm 0.0.5, 2019. Push 2024-07 | 396 / 191 | MIT | Legacy, broken on modern Gradle | none | No Android 12 support | Dead. Avoid |
| @vardrz/react-native-bluetooth-escpos-printer | SPP (januslo fork, ESC/POS + TSC) | `printPic` | 0.1.2, 2026-01 | 11 / 1 | MIT | Legacy | none | partial | Small fork. Possible SPP fallback |
| react-native-thermal-pos-printer (CoFixer) | BT, USB, WiFi, Ethernet | `printImage` (base64) | 1.6.6, 2025-08. Push 2025-09 | 12 / 7 | MIT | unknown | none | Documents BT_SCAN / CONNECT | Young and quiet. Low trust |
| react-native-esc-pos-printer (tr3v3r) | BLE, LAN, USB, Wi-Fi via **Epson ePOS SDK** | yes (prints RN views too) | 4.5.1, 2026-10-28 | 284 / 27 | MIT wrapper; Epson SDK binaries have Epson's own license | **Yes** (RN ≥ 0.76) | Yes (`expo prebuild`) | handled | **Epson TM printers only.** Not for generic printers |
| expo-thermal-printer | ? | ? | 0.1.1, 2024-12; repo gone (404) | n/a | n/a | n/a | n/a | n/a | Dead |

**Expo context:** SDK 55 and later run only on the New Architecture. Old-style bridge modules (the transport libraries above) still load through React Native's interop layer, which is on by default. Expect them to work, but **build a spike on the exact SDK you target in week 1** and run `npx expo doctor`.

**Android 12+ Bluetooth permissions:** declare `BLUETOOTH_CONNECT`, and `BLUETOOTH_SCAN` with `usesPermissionFlags="neverForLocation"` if you discover devices. Request both at runtime with `PermissionsAndroid.requestMultiple`. Keep `BLUETOOTH` / `BLUETOOTH_ADMIN` with `maxSdkVersion="30"`, and add `ACCESS_FINE_LOCATION` for Android 11 and below if you scan. Simplest UX: only list **already-paired** (bonded) devices, which needs `BLUETOOTH_CONNECT` alone. Staff pair the printer once in Android settings (PIN is usually 0000 or 1234).

## 2. Devanagari (Nepali) printing

**Why text mode fails:** cheap printers ship with Latin, CJK or occasionally Arabic fonts. None has a Devanagari code page. Even ESC/POS printers sold with "Hindi" support can't shape conjuncts (क्ष, त्र, half forms, matra reordering), because that needs a full text-shaping engine such as HarfBuzz. **Confidence: high.**

**Approach: raster everything** (or at least every line that contains Devanagari).

1. **Rendering.**
   - **Preferred: Skia.** `Skia.Surface.MakeOffscreen(576, h)`, then a Paragraph API with a bundled Noto Sans Devanagari / Mukta font (Skia shapes text with HarfBuzz). Then `snapshot().readPixels()` gives RGBA. This needs no visible view, renders deterministically, and works in a background task.
   - **Alternative: `react-native-view-shot`** (6.2.1, 2026-10) on an off-screen `<View collapsable={false}>`. Simpler to lay out with JSX/Flex, but it needs the view mounted (positioned off-screen, not `display:none`), and its scaling depends on device pixel density. Set the exact pixel width (`width: 576 / PixelRatio.get()`) or resize afterwards.
2. **Widths.** Use **384 dots for 58 mm** (48 mm printable area at 203 dpi) and **576 dots for 80 mm** (72 mm area). Some 80 mm models only take 512. Make the width a per-printer setting and print a test pattern during setup. Widths must be multiples of 8, which the encoder pads automatically.
3. **Black and white.** Text renders cleanly with a plain **threshold** (about 128–160). Dithering (Atkinson or Floyd–Steinberg) suits logos and photos only, because it makes text fuzzy. Render text in pure black, without anti-aliasing if you can, and at a reasonably large size (≥ 22 px at 576 width).
4. **Raster command.** `GS v 0` is the most widely supported raster command on Chinese clones. Send tall receipts in **bands** (for example 24–256 rows per GS v 0 block). Some cheap firmware has small buffers and prints garbage or stalls on one huge block. Fall back to column mode (`ESC *`) if a model misbehaves. ReceiptPrinterEncoder exposes both modes.
5. **Speed and size.** An 80 mm receipt of about 1500 rows is 576/8 × 1500 ≈ **108 KB**. LAN prints it in under 1 s. SPP gives roughly 10–30 KB/s in practice, so **3–8 s** per long receipt. Mitigations:
   - Keep receipts short and avoid big logos.
   - Rasterize only the Devanagari lines, and send numbers and prices as native text (ASCII renders fine in printer fonts). This is a hybrid mode.
   - Reuse the open socket.
   - Pace writes with chunks of about 512 B–4 KB and short delays if the printer drops data.
6. **Cut and drawer.** Send `GS V` (cut) and `ESC p` (drawer kick) after the image. Many 58 mm units have no cutter.

**Encoder input on React Native:** the encoder accepts an ImageData-like object `{ data: Uint8ClampedArray(RGBA), width, height }`, so pass the Skia `readPixels()` buffer directly. For a view-shot PNG, decode it first with `pngjs` / `upng-js`, or use base64 with the haroldtran native `printImageBase64` path instead.

**Confidence:** high that the approach works (it is the standard workaround used for Indic, Thai, Arabic and Khmer receipts). Medium on the exact speed and banding behaviour of any given clone, which needs hardware testing.

## 3. Printing from the server (kitchen tickets)

- `node-thermal-printer` (4.6.1, 2026-08, 919 stars, MIT) and ReceiptPrinterEncoder both work in Node and print over TCP 9100.
- **A cloud-hosted server cannot reach `192.168.x.x:9100` on the restaurant LAN.** The printers sit behind NAT/CGNAT. Port-forwarding raw 9100 to the internet is insecure and often impossible with Nepali ISPs (CGNAT). **Confidence: high.**
- Options:
  1. **Phone/tablet as print hub (recommended for v1).** The POS device that receives the order (via the server and a websocket or push) prints the KOT to the kitchen's LAN printer over TCP. The app needs a "print station" role with a foreground service so jobs keep printing while the screen is off.
  2. **Local print agent.** A small Node service on a Raspberry Pi or old Android box at the restaurant. It holds an outbound websocket or MQTT connection to the cloud, pulls jobs, and prints them with ReceiptPrinterEncoder + `net.Socket`. This is the most reliable option for KOTs (always on, no phone battery or OS process killing). Add it in v2. The same encoder code is shared with the app.
  3. **Cloud-print printers** (Epson Server Direct Print, Star CloudPRNT, Sunmi). These poll a URL over HTTP. They are proprietary or pricier, so they fall outside "cheap Chinese printer". Skip.
- Whatever the path, use a **job queue with an ack** (`pending → sent → printed / failed`) so a ticket that fails to print is visible and can be reprinted.

## 4. Printing from the web dashboard

| API | Works with | Browsers | Verdict |
|---|---|---|---|
| **Web Serial** | USB-serial printers. Paired **Bluetooth Classic SPP** on desktop Chrome 117+ (RFCOMM). Android Chrome 148+ (2026, includes BT RFCOMM; verify stable status) | Chromium only (Chrome / Edge / Opera). No Safari or Firefox | Workable for a counter PC |
| **WebUSB** | USB printers with a vendor-class interface. On Windows, the printer driver often claims the device (needs WinUSB/Zadig). Android Chrome supports it | Chromium only | Workable but fiddly on Windows |
| **Web Bluetooth** | **BLE GATT only.** Most cheap SPP printers don't expose BLE, though some newer "dual-mode" ones do | Chromium (not iOS Safari) | Not reliable for this hardware |
| Raw TCP 9100 | browsers cannot open raw sockets | — | Impossible without an agent |
| `window.print()` with a 58/80 mm CSS page | needs an OS printer driver | all | Usable on a PC that has the driver installed, and Devanagari renders natively. Slow print dialog unless kiosk mode (`--kiosk-printing`) |

Ready-made helpers from the same author as the encoder: `@point-of-sale/webserial-receipt-printer`, `webusb-receipt-printer`, `webbluetooth-receipt-printer` (MIT). The WebUSB and WebBluetooth repos were pushed 2026-09. On the web, the receipt image comes from an HTML `<canvas>`, where the browser shapes Devanagari for you.

QZ Tray (1078 stars, active) is a local agent that bridges the browser to raw printers. Its community edition is LGPL but nags without a paid certificate. The local agent from section 3 can also expose a localhost or websocket API for the dashboard.

**Recommendation:** treat browser printing as optional (v2). Use Web Serial or WebUSB on Chromium for an admin or counter PC, or route jobs through the local agent.

## Recommended architecture

```
Receipt data → layout (Skia, Noto Sans Devanagari, 384/576 px)
            → RGBA → ReceiptPrinterEncoder (threshold, GS v 0, banded) + cut
            → Transport adapter interface { connect, write(Uint8Array), close }
                 ├─ TcpTransport      (react-native-tcp-socket, port 9100)
                 ├─ BtSppTransport    (react-native-bluetooth-classic, bonded devices, base64)
                 └─ (v2) AgentTransport (cloud → local agent → TCP)
```

Keep the encoder and layout in a shared TS package so the app, the web dashboard (canvas) and the Node agent all produce identical bytes.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| The transport libraries are legacy-bridge modules. A future RN release could drop the interop layer, or bugs could appear | Medium | Week-1 spike on the target SDK. Both modules are small enough to fork, or rewrite as an Expo Module (Kotlin, about 200 lines each) |
| react-native-bluetooth-classic is published only as release candidates | Medium | Pin the exact version. Fork if it stalls |
| haroldtran has no license file, so legally it is all-rights-reserved | High if used | Don't depend on it. Ask the author to add MIT/ISC, or borrow ideas only from the ISC upstream |
| Clone firmware quirks (GS v 0 buffer limits, 512 vs 576 width, ignored cut command) | High | Per-printer profile (width, band height, image mode, cutter on/off, chunk delay) plus a test-print screen |
| SPP is slow for big raster receipts | Medium | Hybrid text + raster, short layouts, or prefer LAN printers for the kitchen |
| Android kills the print-hub app in the background | Medium | Foreground service, battery-optimisation exemption, and the local agent in v2 |
| Bluetooth pairing / permission UX | Low | Bonded-only device list, plus guided permission requests |

## Confidence summary

- Generic ESC/POS raster over TCP/SPP with a JS encoder: **high**.
- react-native-esc-pos-printer being Epson-only: **high** (it is literally the "EPSON ePOS SDK for React Native").
- Legacy modules working on SDK 55/56 through interop: **medium-high**. Verify with a spike.
- Throughput numbers: **medium**. These are estimates; measure on real hardware.
- Web Serial on Android stable: **medium**. Chrome 148 shipped it in 2026; confirm before relying on it.

## Sources

- ReceiptPrinterEncoder: https://github.com/at-point-of-sale/ReceiptPrinterEncoder (docs: `documentation/commands.md#image`, `documentation/usage.md` "Using with React Native and Expo")
- react-native-tcp-socket: https://github.com/Rapsssito/react-native-tcp-socket (issue #187 New Arch)
- react-native-bluetooth-classic: https://github.com/kenjdavidson/react-native-bluetooth-classic (issue #334 New Arch)
- @haroldtran/react-native-thermal-printer: https://github.com/phattran1201/react-native-thermal-printer (`BLEPrinterAdapter.java` uses RFCOMM UUID 00001101; `UtilsImage.java` uses GS v 0)
- react-native-thermal-receipt-printer: https://github.com/HeligPfleigh/react-native-thermal-receipt-printer
- react-native-thermal-receipt-printer-image-qr: https://github.com/thiendangit/react-native-thermal-receipt-printer-image-qr
- react-native-bluetooth-escpos-printer: https://github.com/januslo/react-native-bluetooth-escpos-printer · fork https://github.com/vardrz/react-native-bluetooth-escpos-printer
- react-native-thermal-pos-printer: https://github.com/CoFixer/react-native-thermal-pos-printer
- react-native-esc-pos-printer (Epson ePOS): https://github.com/tr3v3r/react-native-esc-pos-printer · https://www.npmjs.com/package/react-native-esc-pos-printer
- node-thermal-printer: https://github.com/Klemen1337/node-thermal-printer
- react-native-view-shot: https://github.com/gre/react-native-view-shot · React Native Skia: https://github.com/Shopify/react-native-skia
- Expo New Architecture guide: https://docs.expo.dev/guides/new-architecture · SDK 55 changelog: https://expo.dev/changelog/sdk-55
- Web Serial over Bluetooth: https://developer.chrome.com/blog/serial-over-bluetooth · Android intent to ship: https://groups.google.com/a/chromium.org/g/blink-dev/c/BqUGCcurReE · https://caniuse.com/web-serial
- Web printer helpers: https://github.com/at-point-of-sale/WebSerialReceiptPrinter · https://github.com/at-point-of-sale/WebUSBReceiptPrinter · https://github.com/at-point-of-sale/WebBluetoothReceiptPrinter
- QZ Tray: https://github.com/qzind/tray
