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

- Transport is `/dev/cdc-wdm*` + QMUX only for now
- `IMSDCM` isn't supported — it needs a 16-bit service ID / `QRTR` path that a plain wrapper can't provide
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

## Good fit

- 4G/5G always-on dial daemons
- QMI-based SMS gateways
- Voice/USSD control-plane integration
- Services that need to read SIM/UIM files directly

## Not a good fit

- Deep IMS bearer management that needs `QRTR`/`IMSDCM`
- Non-Linux platforms
- Wanting full coverage of every `libqmi` service
- One-off commands where you don't want a code integration

## Development

```bash
cd qmi-go
go test ./...
```

If you're developing against a consumer of this module locally, point it at your working copy with a Go workspace (`go.work`) or a `go.mod` `replace` directive instead of pulling a remote tag.

---

# qmi-go（中文）

面向 Linux 的纯 Go QMI 库和连接管理器，主要用于 Quectel/Qualcomm 蜂窝模组。

它的定位不是"包一层 AT 命令"，而是直接围绕 `/dev/cdc-wdm*` 上的 QMI/QMUX 做协议栈，并提供设备发现、拨号管理、短信、IMS 和 VOICE 能力封装。

## 项目定位

- 纯 Go 实现，不依赖 `libqmi`、`qmicli` 或 `quectel-CM` 运行时
- 以 QMI 为主控制面，适合做长期驻留进程、服务端集成和二次开发
- 提供两层能力：
  - `pkg/qmi`：协议级 service wrapper
  - `pkg/manager`：更高层的拨号、重连、事件和短信管理

## 已实现的核心 service

| Service | 能力概览 |
| --- | --- |
| `DMS` | 设备信息、序列号、运行模式、PIN、ICCID/IMSI、Band/能力、MAC、用户数据 |
| `NAS` | 驻网状态、信号、系统信息、搜网、制式偏好、系统选择偏好、小区、网络时间 |
| `WDS` | 拨号/断开、runtime settings、profile 管理、流量统计、bearer、autoconnect |
| `WDA` | Raw-IP / 数据格式配置 |
| `UIM` | 卡状态、PIN、透明文件/record 读取、逻辑通道、APDU、slot 状态与切换 |
| `WMS` | 短信发送、读取、列举、删除、路由、ACK、存储后发送、短信事件 |
| `IMS` | IMS 服务开关读取/设置、绑定 |
| `IMSA` | IMS 注册状态、IMS 服务状态、状态变更 indication |
| `IMSP` | IMS enabler 状态查询 |
| `VOICE` | 拨号、接听、挂断、DTMF、USSD、补充业务、通话状态 indication |

### `manager` 层额外提供

- 自动拨号与自动重连
- IPv4 / IPv6 双栈
- `QMAP Mux` 多 PDN 拨号
- 设备自动发现
- `WMS` 短信收发与事件
- `IMS/IMSA` 状态事件桥接
- `VOICE` 通话/USSD 事件桥接
- 高层查询接口，方便直接拿设备信息、网络状态、卡状态

## 当前边界

- 当前 transport 以 `/dev/cdc-wdm*` + QMUX 为主
- `IMSDCM` 还不支持，原因是它依赖 16-bit service id / `QRTR` 路径，不是补一个普通 wrapper 就能解决
- 当前更适合 Linux 宿主机或容器内的模组管理进程，不是桌面 GUI 工具

## 目录结构

```text
qmi-go/
├── cmd/
│   ├── cm/         # 主连接管理 CLI
│   ├── dms-tool/   # DMS 调试
│   ├── info-tool/  # 信息查询
│   ├── nas-tool/   # NAS 调试
│   ├── sms-tool/   # 短信调试
│   ├── wda-tool/   # WDA 调试
│   └── wds-tool/   # WDS 调试
├── pkg/
│   ├── device/     # 设备发现
│   ├── manager/    # 高层连接管理器
│   ├── netcfg/     # Linux 网络配置
│   └── qmi/        # 协议栈与各 service wrapper
└── go.mod
```

## 环境要求

- Linux
- Go `1.24+`
- 可访问的 QMI 控制节点，例如 `/dev/cdc-wdm0`
- 可用的网络接口，例如 `wwan0`
- 具备配置地址、路由、DNS 的权限，一般需要 `root` 或等价的 `CAP_NET_ADMIN`

## 编译

```bash
cd qmi-go
go build -o qmi-go ./cmd/cm
```

## CLI 快速开始

```bash
# 默认自动发现第一个模组，双栈拨号
sudo ./qmi-go -s internet

# 指定网络接口
sudo ./qmi-go -i wwan0 -s internet

# 指定控制节点
sudo ./qmi-go -d /dev/cdc-wdm0 -s internet

# 带认证
sudo ./qmi-go -s myapn -u user -p pass -a 1

# 仅 IPv4 / 仅 IPv6
sudo ./qmi-go -s internet -4
sudo ./qmi-go -s internet -6

# 指定 ProfileIndex 和 MuxID，发起 QMAP 多路拨号
sudo ./qmi-go -s ims -n 2 -m 2
```

### 常用参数

| 参数 | 说明 |
| --- | --- |
| `-s` | APN |
| `-u` | 认证用户名 |
| `-p` | 认证密码 |
| `-a` | 认证类型：`0=none`、`1=PAP`、`2=CHAP`、`3=PAP|CHAP` |
| `-pin` | SIM PIN |
| `-i` | 网络接口名，例如 `wwan0` |
| `-d` | 控制设备路径，例如 `/dev/cdc-wdm0` |
| `-4` / `-6` | 仅 IPv4 / 仅 IPv6 |
| `-set-route` | 写默认路由，默认关闭 |
| `-set-dns` | 写 DNS，默认关闭 |
| `-n` | PDN Profile 索引 |
| `-m` | `QMAP Mux ID` |
| `-v` | 输出调试日志 |
| `-version` | 输出版本 |

说明：不传 `-4`/`-6` 默认双栈；`-set-route`/`-set-dns` 默认关闭，更适合调试和接入自定义网络编排；`-n`/`-m` 一般配合多 PDN / QMAP 使用。

## 作为库使用

### 1. 最小拨号示例

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

### 2. 只启动 QMI Core，不立即拨号

适合做"查询型"程序，例如设备详情、SIM 文件访问、IMS 状态页：

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

### 3. 短信示例

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

### 4. 事件示例

`manager` 统一把连接、短信、IMS、VOICE 事件桥接成回调：

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

也可以使用专门的便捷回调：

```go
mgr.OnIMSRegistrationStatus(func(info *qmi.IMSARegistrationStatus) {
	fmt.Printf("ims registered: %+v\n", info)
})

mgr.OnVoiceUSSD(func(info *qmi.VoiceUSSDIndication) {
	fmt.Printf("ussd: %+v\n", info)
})
```

## `manager.Config` 关键字段

| 字段 | 说明 |
| --- | --- |
| `Device` | `manager.ModemDevice`，可由 `device.Discover()` 获取，也可由调用方显式注入 |
| `APN` | 拨号使用的 APN |
| `Username` / `Password` / `AuthType` | 认证参数 |
| `EnableIPv4` / `EnableIPv6` | 双栈控制 |
| `PINCode` | SIM PIN |
| `AutoReconnect` | 断线自动重连 |
| `NoRoute` | 不自动添加默认路由 |
| `NoDNS` | 不自动写 DNS |
| `DisableWMSInd` | 禁用短信 indication |
| `DisableIMSAInd` | 禁用 IMSA indication |
| `DisableVOICEInd` | 禁用 VOICE indication |
| `ProfileIndex` | PDN Profile 索引 |
| `MuxID` | QMAP 多路复用 ID |
| `NoDial` | 只初始化 QMI core，不发起 WDS 拨号 |

## 调试工具

仓库内置了一组轻量 CLI，方便联调协议层：`cmd/cm`、`cmd/dms-tool`、`cmd/info-tool`、`cmd/nas-tool`、`cmd/sms-tool`、`cmd/wda-tool`、`cmd/wds-tool`。

如果你要把某个 service 接进上层业务，通常可以先用这些小工具确认模组返回，再写正式集成代码。

## 适合 / 不适合的场景

适合：4G/5G 拨号常驻进程、QMI 短信网关、语音/USSD 控制面集成、需要直接读 SIM/UIM 文件的服务。

不适合：依赖 `QRTR`/`IMSDCM` 的深度 IMS bearer 管理、非 Linux 平台、期望完全覆盖 `libqmi` 全部 service 的场景、只想临时执行几个一次性命令而不想引入代码集成。

## 开发说明

```bash
cd qmi-go
go test ./...
```

如果你同时在本地联调上层项目，建议使用 `go work` 或 `replace` 指向本地路径，而不是依赖远端 tag。
