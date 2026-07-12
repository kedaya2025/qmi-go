# qmi-go

A pure-Go QMI library and connection manager for Linux, built for Quectel/Qualcomm cellular modules.

It doesn't wrap AT commands — it speaks QMI/QMUX directly over `/dev/cdc-wdm*`, providing a protocol stack plus device discovery, dial management, SMS, IMS, and VOICE support.

## Design goals

- Pure Go, no dependency on `libqmi`, `qmicli`, or the `quectel-CM` runtime
- QMI as the primary control plane — suited to long-running daemons, server-side integration, and custom tooling
- Two layers:
  - `pkg/qmi`: protocol-level service wrappers
  - `pkg/manager`: a higher-level connection manager (dial, reconnect, events, SMS)

## Supported services

| Service | Coverage |
| --- | --- |
| `DMS` | Device info, serial numbers, operating mode, PIN, ICCID/IMSI, band capabilities, MAC, user data |
| `NAS` | Registration state, signal, system info, network scan, tech/system-selection preference, cell info, network time |
| `WDS` | Connect/disconnect, runtime settings, profile management, traffic stats, bearer, autoconnect |
| `WDA` | Raw-IP / data format configuration |
| `UIM` | Card status, PIN, transparent file/record reads, logical channels, APDU, slot status and switching |
| `WMS` | SMS send/read/list/delete, routing, ACK, send-from-storage, SMS events |
| `IMS` | IMS service enable/disable, binding |
| `IMSA` | IMS registration status, service status, change indications |
| `IMSP` | IMS enabler status queries |
| `VOICE` | Dial, answer, hang up, DTMF, USSD, supplementary services, call-status indications |

### The `manager` layer adds

- Automatic dial and reconnect
- IPv4 / IPv6 dual-stack
- `QMAP` multiplexed multi-PDN dialing
- Automatic device discovery
- `WMS` SMS send/receive and events
- `IMS`/`IMSA` status event bridging
- `VOICE` call/USSD event bridging
- High-level query APIs for device, network, and SIM state

## Current limits

- Native `QRTR` (`AF_QIPCRTR`) transport is available (see below) but requires a kernel with `CONFIG_QRTR` and is less battle-tested than the default `/dev/cdc-wdm*` + QMUX path
- Aimed at Linux host/container modem-management daemons, not a desktop GUI tool

## Layout

```text
qmi-go/
├── cmd/
│   ├── cm/         # main connection-manager CLI
│   ├── dms-tool/   # DMS debugging
│   ├── info-tool/  # info queries
│   ├── nas-tool/   # NAS debugging
│   ├── sms-tool/   # SMS debugging
│   ├── wda-tool/   # WDA debugging
│   └── wds-tool/   # WDS debugging
├── pkg/
│   ├── device/     # device discovery
│   ├── manager/    # high-level connection manager
│   ├── netcfg/     # Linux network configuration
│   └── qmi/        # protocol stack and service wrappers
└── go.mod
```

## Requirements

- Linux
- Go `1.24+`
- A reachable QMI control node, e.g. `/dev/cdc-wdm0`
- A usable network interface, e.g. `wwan0`
- Permission to configure addresses/routes/DNS — usually `root` or equivalent `CAP_NET_ADMIN`

## Build

```bash
cd qmi-go
go build -o qmi-go ./cmd/cm
```

## CLI quick start

### Basic usage

```bash
# auto-discover the first modem, dual-stack dial
sudo ./qmi-go -s internet

# pin a network interface
sudo ./qmi-go -i wwan0 -s internet

# pin a control node
sudo ./qmi-go -d /dev/cdc-wdm0 -s internet

# with auth
sudo ./qmi-go -s myapn -u user -p pass -a 1

# IPv4 only
sudo ./qmi-go -s internet -4

# IPv6 only
sudo ./qmi-go -s internet -6

# specific ProfileIndex and MuxID, QMAP multi-PDN dial
sudo ./qmi-go -s ims -n 2 -m 2
```

### Flags

| Flag | Meaning |
| --- | --- |
| `-s` | APN |
| `-u` | auth username |
| `-p` | auth password |
| `-a` | auth type: `0=none`, `1=PAP`, `2=CHAP`, `3=PAP|CHAP` |
| `-pin` | SIM PIN |
| `-i` | network interface name, e.g. `wwan0` |
| `-d` | control device path, e.g. `/dev/cdc-wdm0` |
| `-qrtr` | use native `QRTR` (`AF_QIPCRTR`) transport for the QMI control channel instead of `-d`'s cdc-wdm device (see [QRTR transport](#qrtr-transport)) |
| `-4` | IPv4 only |
| `-6` | IPv6 only |
| `-set-route` | write the default route (off by default) |
| `-set-dns` | write DNS (off by default) |
| `-n` | PDN profile index |
| `-m` | `QMAP` mux ID |
| `-v` | verbose/debug logging |
| `-version` | print version |

Notes:

- If neither `-4` nor `-6` is passed, dual-stack is enabled by default
- `-set-route` and `-set-dns` default to off, which fits debugging and integrating with your own network orchestration
- `-n` and `-m` are typically used together for multi-PDN / QMAP setups

## Using it as a library

### 1. Minimal dial example

```go
package main

import (
	"fmt"
	"log"

	"github.com/iniwex5/qmi-go/pkg/device"
	"github.com/iniwex5/qmi-go/pkg/manager"
	"github.com/iniwex5/qmi-go/pkg/qmi"
)

func main() {
	modems, err := device.Discover()
	if err != nil {
		log.Fatal(err)
	}

	mgr := manager.New(manager.Config{
		Device:        modems[0],
		APN:           "internet",
		EnableIPv4:    true,
		EnableIPv6:    false,
		AutoReconnect: true,
	}, nil)

	mgr.OnConnect(func(s *qmi.RuntimeSettings) {
		fmt.Printf("connected: %s\n", s.IPv4Address)
	})

	if err := mgr.Start(); err != nil {
		log.Fatal(err)
	}
	defer mgr.Stop()

	select {}
}
```

### 2. Start QMI core only, without dialing

Useful for "query-only" programs — device details, SIM file access, an IMS status page:

```go
mgr := manager.New(manager.Config{
	Device:  modems[0],
	NoDial:  true,
	NoRoute: true,
	NoDNS:   true,
}, nil)

if err := mgr.StartCore(); err != nil {
	log.Fatal(err)
}
defer mgr.Stop()

ctx := context.Background()
manufacturer, _ := mgr.GetManufacturer(ctx)
model, _ := mgr.GetModel(ctx)
serving, _ := mgr.GetServingSystem(ctx)

fmt.Println(manufacturer, model, serving.RegistrationState)
```

### 3. SMS example

```go
if err := mgr.SendSMS("+8613800138000", "hello from qmi-go"); err != nil {
	log.Fatal(err)
}

list, err := mgr.ListSMS(0, qmi.MessageTagTypeMTRead)
if err != nil {
	log.Fatal(err)
}

for _, item := range list {
	msg, err := mgr.ReadSMS(0, item.Index)
	if err != nil {
		continue
	}
	fmt.Printf("%s: %s\n", msg.Sender, msg.Message)
}
```

### 4. Events

`manager` bridges connection, SMS, IMS, and VOICE events into a single callback:

```go
mgr.OnEvent(func(e manager.Event) {
	switch e.Type {
	case manager.EventConnected:
		fmt.Println("data connected")
	case manager.EventNewSMS:
		fmt.Printf("new sms index=%d storage=%d\n", e.SMSIndex, e.StorageType)
	case manager.EventIMSRegistrationStatus:
		fmt.Printf("ims status=%v\n", e.IMSRegistration)
	case manager.EventVoiceCallStatus:
		fmt.Printf("voice calls=%v\n", e.VoiceCalls)
	}
})
```

Dedicated convenience callbacks are also available:

```go
mgr.OnIMSRegistrationStatus(func(info *qmi.IMSARegistrationStatus) {
	fmt.Printf("ims registered: %+v\n", info)
})

mgr.OnVoiceUSSD(func(info *qmi.VoiceUSSDIndication) {
	fmt.Printf("ussd: %+v\n", info)
})
```

## `manager.Config` reference

| Field | Meaning |
| --- | --- |
| `Device` | `manager.ModemDevice`, from `device.Discover()` or injected explicitly by the caller |
| `APN` | APN used for dialing |
| `Username` / `Password` / `AuthType` | auth parameters |
| `EnableIPv4` / `EnableIPv6` | dual-stack control |
| `PINCode` | SIM PIN |
| `AutoReconnect` | reconnect automatically on drop |
| `NoRoute` | don't add the default route automatically |
| `NoDNS` | don't write DNS automatically |
| `DisableWMSInd` | disable SMS indications |
| `DisableIMSAInd` | disable IMSA indications |
| `DisableVOICEInd` | disable VOICE indications |
| `ProfileIndex` | PDN profile index |
| `MuxID` | QMAP mux ID |
| `NoDial` | initialize QMI core only, skip the WDS dial |

## Debug tools

A set of small CLIs ship in the repo for protocol-level debugging:

- `cmd/cm`
- `cmd/dms-tool`
- `cmd/info-tool`
- `cmd/nas-tool`
- `cmd/sms-tool`
- `cmd/wda-tool`
- `cmd/wds-tool`

If you're wiring a service into higher-level code, it's usually worth checking the modem's raw response with one of these first.

## QRTR transport

Besides the default `/dev/cdc-wdm*` + QMUX path, `qmi-go` can talk QMI natively over `QRTR` (`AF_QIPCRTR`), bypassing the cdc-wdm character device entirely. It's implemented as a second `qmiTransport` — the transaction engine, TLV parsing, and indication dispatch in `pkg/qmi` are unchanged either way.

Requirements:

- Linux kernel with `CONFIG_QRTR` (and the relevant transport, e.g. `qrtr-mhi`) enabled and loaded
- `root`, since `AF_QIPCRTR` sockets require it

Enable it with `-qrtr` on any of the CLIs, or `UseQRTR: true` in code:

```bash
sudo ./qmi-go -qrtr -s internet
sudo ./dms-tool -qrtr -action serial
```

```go
client, err := qmi.NewClientWithOptions(ctx, "", qmi.ClientOptions{UseQRTR: true})
```

```go
mgr := manager.New(manager.Config{
	Device:        modems[0],
	APN:           "internet",
	ClientOptions: qmi.ClientOptions{UseQRTR: true},
}, nil)
```

QRTR has no `CTL` service and no client-ID allocation of its own, so `qmi-go` locally simulates the handful of `CTL` messages it needs (`Sync`, `GetVersionInfo`, `GetClientID`, `ReleaseClientID`) instead of sending them over the wire, mirroring `libqmi`'s `qmi-endpoint-qrtr.c` approach. Service discovery is done via `QRTR` `NEW_LOOKUP`/`NEW_SERVER`, and each allocated client gets its own dedicated `QRTR` socket connected to the resolved service.

This also lifts the 8-bit `QMUX` service-ID ceiling: `Packet.ServiceType` is 16-bit, so QRTR-only services beyond `0xFF` (e.g. `IMSDCM` at `0x302`) are addressable. Real `QMUX`/`qmi-proxy` traffic is unaffected — no service it ever exposes exceeds `0xFF`, so that path stays byte-for-byte identical to before.

`-qrtr` and `-d`/`UseProxy` are mutually exclusive; `-qrtr` wins if both are set. `cmd/cm`'s device discovery (`pkg/device`) still scans USB/sysfs for a network interface and currently expects to find a `cdc-wdm` control path too, so a modem that exposes `QRTR` but no `cdc-wdm` node at all isn't discoverable yet by the `cm` CLI — use `qmi.NewClientWithOptions` directly against a manually-constructed `manager.ModemDevice` in that case.

## Good fit

- 4G/5G always-on dial daemons
- QMI-based SMS gateways
- Voice/USSD control-plane integration
- Services that need to read SIM/UIM files directly

## Not a good fit

- Non-Linux platforms
- Wanting full coverage of every `libqmi` service
- One-off commands where you don't want a code integration

## Development

```bash
cd qmi-go
go test ./...
```
