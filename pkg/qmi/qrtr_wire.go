package qmi

import (
	"encoding/binary"
	"fmt"
)

// ============================================================================
// QRTR (AF_QIPCRTR) wire-protocol primitives / QRTR (AF_QIPCRTR) 线格式基础定义
//
// 常量与结构布局均镜像自 Linux 内核 UAPI 头文件
// include/uapi/linux/qrtr.h。此文件只做字节编解码，不依赖任何平台专属 API，
// 因此没有 //go:build linux 限制，可在任意平台上进行单元测试。
// ============================================================================

const (
	// qrtrAddressFamily is AF_QIPCRTR / QRTR 地址族
	qrtrAddressFamily = 0x2a

	// qrtrNodeBcast is QRTR_NODE_BCAST / QRTR 广播节点
	qrtrNodeBcast uint32 = 0xffffffff
	// qrtrPortCtrl is QRTR_PORT_CTRL, the nameserver's well-known port /
	// QRTR_PORT_CTRL，命名服务器（ns）的知名端口
	qrtrPortCtrl uint32 = 0xfffffffe
)

// QRTR control packet types (enum qrtr_pkt_type) / QRTR 控制包类型
const (
	qrtrTypeData      uint32 = 1
	qrtrTypeHello     uint32 = 2
	qrtrTypeBye       uint32 = 3
	qrtrTypeNewServer uint32 = 4
	qrtrTypeDelServer uint32 = 5
	qrtrTypeDelClient uint32 = 6
	qrtrTypeResumeTx  uint32 = 7
	qrtrTypeExit      uint32 = 8
	qrtrTypePing      uint32 = 9
	qrtrTypeNewLookup uint32 = 10
	qrtrTypeDelLookup uint32 = 11
)

// sockaddrQRTRSize matches sizeof(struct sockaddr_qrtr) on Linux:
//
//	struct sockaddr_qrtr {
//	        __kernel_sa_family_t sq_family; // 2 bytes (unsigned short)
//	        __u32 sq_node;                  // 4 bytes, naturally aligned -> 2 bytes padding before it
//	        __u32 sq_port;                  // 4 bytes
//	};
//
// The struct is NOT __packed, so natural alignment inserts 2 padding bytes
// after sq_family, giving a total size of 12 bytes.
// sockaddrQRTRSize 对应 Linux 上 struct sockaddr_qrtr 的大小：该结构体未加
// __packed，按自然对齐 sq_family 之后有 2 字节填充，总大小为 12 字节。
const sockaddrQRTRSize = 12

// sockaddrQRTR mirrors struct sockaddr_qrtr (sq_family is implied to always
// be AF_QIPCRTR and is not stored here). / sockaddrQRTR 对应 struct
// sockaddr_qrtr（sq_family 恒为 AF_QIPCRTR，不在此结构中存储）。
type sockaddrQRTR struct {
	node uint32
	port uint32
}

func marshalSockaddrQRTR(a sockaddrQRTR) [sockaddrQRTRSize]byte {
	var buf [sockaddrQRTRSize]byte
	binary.LittleEndian.PutUint16(buf[0:2], qrtrAddressFamily)
	// buf[2:4] left zero: struct padding before the 4-byte-aligned sq_node.
	binary.LittleEndian.PutUint32(buf[4:8], a.node)
	binary.LittleEndian.PutUint32(buf[8:12], a.port)
	return buf
}

func unmarshalSockaddrQRTR(data []byte) (sockaddrQRTR, error) {
	if len(data) < sockaddrQRTRSize {
		return sockaddrQRTR{}, fmt.Errorf("qrtr: sockaddr_qrtr too short: got %d, want %d", len(data), sockaddrQRTRSize)
	}
	family := binary.LittleEndian.Uint16(data[0:2])
	if family != qrtrAddressFamily {
		return sockaddrQRTR{}, fmt.Errorf("qrtr: unexpected sa_family 0x%04x, want 0x%04x", family, qrtrAddressFamily)
	}
	return sockaddrQRTR{
		node: binary.LittleEndian.Uint32(data[4:8]),
		port: binary.LittleEndian.Uint32(data[8:12]),
	}, nil
}

// qrtrCtrlPktSize matches sizeof(struct qrtr_ctrl_pkt), which IS __packed:
//
//	struct qrtr_ctrl_pkt {
//	        __le32 cmd;
//	        union {
//	                struct { __le32 service, instance, node, port; } server;
//	                struct { __le32 node, port; } client;
//	        };
//	} __packed;
//
// The union's largest member (server, 16 bytes) plus cmd (4 bytes) gives 20
// bytes total. Every control packet sent/received uses this fixed size,
// mirroring upstream qrtr-lookup.c which always does sendto/recv with
// sizeof(pkt) regardless of which union member is semantically populated.
// qrtrCtrlPktSize 对应 struct qrtr_ctrl_pkt（该结构体标记了 __packed）的大小：
// cmd(4) + 联合体中最大的 server 变体(16) = 20 字节。参考上游 qrtr-lookup.c，
// 无论逻辑上使用哪个联合体成员，发送/接收时始终使用固定的 sizeof(pkt)。
const qrtrCtrlPktSize = 20

// qrtrCtrlPkt is the decoded form of struct qrtr_ctrl_pkt using the "server"
// union variant (service/instance/node/port), which is what both NEW_LOOKUP
// requests and NEW_SERVER/DEL_SERVER responses use.
// qrtrCtrlPkt 是 struct qrtr_ctrl_pkt 按 "server" 联合体变体解码后的形式
// （service/instance/node/port），NEW_LOOKUP 请求与 NEW_SERVER/DEL_SERVER
// 响应均使用此变体。
type qrtrCtrlPkt struct {
	cmd      uint32
	service  uint32
	instance uint32
	node     uint32
	port     uint32
}

func marshalQRTRCtrlPkt(p qrtrCtrlPkt) [qrtrCtrlPktSize]byte {
	var buf [qrtrCtrlPktSize]byte
	binary.LittleEndian.PutUint32(buf[0:4], p.cmd)
	binary.LittleEndian.PutUint32(buf[4:8], p.service)
	binary.LittleEndian.PutUint32(buf[8:12], p.instance)
	binary.LittleEndian.PutUint32(buf[12:16], p.node)
	binary.LittleEndian.PutUint32(buf[16:20], p.port)
	return buf
}

func unmarshalQRTRCtrlPkt(data []byte) (qrtrCtrlPkt, error) {
	if len(data) < qrtrCtrlPktSize {
		return qrtrCtrlPkt{}, fmt.Errorf("qrtr: ctrl packet too short: got %d, want %d", len(data), qrtrCtrlPktSize)
	}
	return qrtrCtrlPkt{
		cmd:      binary.LittleEndian.Uint32(data[0:4]),
		service:  binary.LittleEndian.Uint32(data[4:8]),
		instance: binary.LittleEndian.Uint32(data[8:12]),
		node:     binary.LittleEndian.Uint32(data[12:16]),
		port:     binary.LittleEndian.Uint32(data[16:20]),
	}, nil
}

// isZeroServer reports whether this is the all-zero NEW_SERVER sentinel that
// the QRTR nameserver (ns) sends to terminate a NEW_LOOKUP enumeration, as
// documented and implemented by upstream tools/net/qrtr / qrtr-lookup.c:
//
//	if (!pkt.server.service && !pkt.server.instance &&
//	    !pkt.server.node && !pkt.server.port)
//	        break;
//
// isZeroServer 判断是否为 QRTR 命名服务器（ns）用于结束 NEW_LOOKUP 枚举的
// 全零 NEW_SERVER 哨兵包，行为对齐上游 qrtr-lookup.c。
func (p qrtrCtrlPkt) isZeroServer() bool {
	return p.service == 0 && p.instance == 0 && p.node == 0 && p.port == 0
}

// newLookupRequest builds a NEW_LOOKUP control packet. service==0 means
// "enumerate all services" (wildcard), matching upstream lookup.c semantics.
// newLookupRequest 构造一个 NEW_LOOKUP 控制包。service==0 表示"枚举全部
// 服务"（通配符），行为对齐上游 lookup.c。
func newLookupRequest(service uint32) qrtrCtrlPkt {
	return qrtrCtrlPkt{cmd: qrtrTypeNewLookup, service: service}
}
