//go:build linux

package qmi

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ============================================================================
// qrtrRawSocket: minimal AF_QIPCRTR socket operations needed by qrtrTransport
// qrtrRawSocket: qrtrTransport 所需的最小 AF_QIPCRTR 套接字操作集
//
// golang.org/x/sys/unix defines AF_QIPCRTR but has no SockaddrQIPCRTR type
// implementing the (unexported) unix.Sockaddr interface, so unix.Sendto /
// unix.Recvfrom cannot be used directly. This file talks to the kernel via
// raw syscalls with a hand-packed struct sockaddr_qrtr (see qrtr_wire.go).
//
// golang.org/x/sys/unix 定义了 AF_QIPCRTR，但没有实现其（未导出）
// unix.Sockaddr 接口的 SockaddrQIPCRTR 类型，因此无法直接使用 unix.Sendto /
// unix.Recvfrom。本文件通过原始系统调用，配合手工打包的 struct sockaddr_qrtr
// （见 qrtr_wire.go）直接与内核通信。
// ============================================================================

// qrtrRawSocket abstracts the kernel socket calls qrtrTransport depends on so
// that tests can substitute an in-memory fake instead of requiring a real
// AF_QIPCRTR-capable kernel (CONFIG_QRTR). qrtrRawSocket 对 qrtrTransport
// 依赖的内核套接字调用做了抽象，便于测试用内存态假实现替换，而无需真实的
// 支持 AF_QIPCRTR 的内核（CONFIG_QRTR）。
type qrtrRawSocket interface {
	// LocalAddr returns the address the kernel auto-assigned to this socket.
	LocalAddr() (sockaddrQRTR, error)
	// SendTo sends data to dst. / SendTo 向 dst 发送数据。
	SendTo(data []byte, dst sockaddrQRTR) error
	// RecvFrom blocks (up to the configured receive timeout) for one datagram.
	// A timeout is reported via unix.EAGAIN/unix.EWOULDBLOCK.
	// RecvFrom 阻塞等待（直至配置的接收超时）一个数据报；超时通过
	// unix.EAGAIN/unix.EWOULDBLOCK 报告。
	RecvFrom(buf []byte) (n int, from sockaddrQRTR, err error)
	// SetRecvTimeout bounds how long RecvFrom blocks, so a reader goroutine
	// can periodically re-check for shutdown. SetRecvTimeout 限制 RecvFrom
	// 的最长阻塞时间，使读取协程能周期性检查关闭信号。
	SetRecvTimeout(d time.Duration) error
	Close() error
}

// newQRTRRawSocket opens a real AF_QIPCRTR/SOCK_DGRAM kernel socket.
// newQRTRRawSocket 打开一个真实的 AF_QIPCRTR/SOCK_DGRAM 内核套接字。
func newQRTRRawSocket() (qrtrRawSocket, error) {
	fd, err := unix.Socket(qrtrAddressFamily, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("qrtr: socket(AF_QIPCRTR, SOCK_DGRAM): %w", err)
	}
	return &qrtrKernelSocket{fd: fd}, nil
}

type qrtrKernelSocket struct {
	fd int
}

func bufPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

func (s *qrtrKernelSocket) LocalAddr() (sockaddrQRTR, error) {
	var addrBuf [sockaddrQRTRSize]byte
	addrLen := uint32(sockaddrQRTRSize)

	_, _, errno := unix.Syscall(
		unix.SYS_GETSOCKNAME,
		uintptr(s.fd),
		uintptr(unsafe.Pointer(&addrBuf[0])),
		uintptr(unsafe.Pointer(&addrLen)),
	)
	if errno != 0 {
		return sockaddrQRTR{}, fmt.Errorf("qrtr: getsockname: %w", errno)
	}
	return unmarshalSockaddrQRTR(addrBuf[:])
}

func (s *qrtrKernelSocket) SendTo(data []byte, dst sockaddrQRTR) error {
	addr := marshalSockaddrQRTR(dst)
	_, _, errno := unix.Syscall6(
		unix.SYS_SENDTO,
		uintptr(s.fd),
		uintptr(bufPtr(data)),
		uintptr(len(data)),
		0, // flags
		uintptr(unsafe.Pointer(&addr[0])),
		uintptr(len(addr)),
	)
	if errno != 0 {
		return fmt.Errorf("qrtr: sendto: %w", errno)
	}
	return nil
}

func (s *qrtrKernelSocket) RecvFrom(buf []byte) (int, sockaddrQRTR, error) {
	var addrBuf [sockaddrQRTRSize]byte
	addrLen := uint32(sockaddrQRTRSize)

	n, _, errno := unix.Syscall6(
		unix.SYS_RECVFROM,
		uintptr(s.fd),
		uintptr(bufPtr(buf)),
		uintptr(len(buf)),
		0, // flags
		uintptr(unsafe.Pointer(&addrBuf[0])),
		uintptr(unsafe.Pointer(&addrLen)),
	)
	if errno != 0 {
		return 0, sockaddrQRTR{}, errno
	}
	from, err := unmarshalSockaddrQRTR(addrBuf[:])
	if err != nil {
		// Datagram was received successfully; a malformed source address
		// shouldn't discard the payload, just report an empty peer.
		from = sockaddrQRTR{}
	}
	return int(n), from, nil
}

func (s *qrtrKernelSocket) SetRecvTimeout(d time.Duration) error {
	tv := unix.NsecToTimeval(d.Nanoseconds())
	return unix.SetsockoptTimeval(s.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

func (s *qrtrKernelSocket) Close() error {
	return unix.Close(s.fd)
}

// isQRTRRetryable reports whether a RecvFrom error is a transient condition
// (receive-timeout expiry from SO_RCVTIMEO, or a signal interruption) that a
// reader loop should treat as "no data yet" rather than a fatal socket error.
// isQRTRRetryable 判断 RecvFrom 错误是否为瞬时状况（SO_RCVTIMEO 超时或信号
// 中断），读取循环应将其视为"暂无数据"而非致命的套接字错误。
func isQRTRRetryable(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR)
}
