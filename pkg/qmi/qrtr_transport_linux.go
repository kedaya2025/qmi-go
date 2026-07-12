//go:build linux

package qmi

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ============================================================================
// qrtrTransport: native QRTR (AF_QIPCRTR) qmiTransport implementation
// qrtrTransport: 原生 QRTR (AF_QIPCRTR) qmiTransport 实现
//
// QRTR has no CTL service and no per-connection client-ID allocation of its
// own, so this transport locally simulates the handful of CTL messages
// qmi-go's Client actually issues (Sync / GetVersionInfo / GetClientID /
// ReleaseClientID) instead of forwarding them over the wire, mirroring
// libqmi's qmi-endpoint-qrtr.c "fake header + local CTL" architecture.
//
// Phase 1a (current): synthesized/rebuilt frames use the real 0x01 QMUX
// header (ServiceType truncated to 8 bits), so pkg/qmi's readLoop and
// UnmarshalPacket require ZERO changes. This covers every service actually
// used by this project (all <= 0x22). Phase 1b (see qrtr_transport_plan.md
// M4) will upgrade to a 16-bit-service-aware 0x02 virtual header to also
// address QRTR services > 255 (e.g. SSC 0x190, IMSDCM 0x302).
//
// QRTR 没有 CTL 服务，也没有自己的客户端 ID 分配机制，因此本传输层在本地
// 模拟 qmi-go Client 实际会发出的少量 CTL 消息（Sync/GetVersionInfo/
// GetClientID/ReleaseClientID），而不是将其转发到线路上，这与 libqmi
// qmi-endpoint-qrtr.c 的"虚拟包头 + 本地 CTL"架构一致。
//
// 当前 Phase 1a：合成/重建的帧使用真实的 0x01 QMUX 头（ServiceType 截断为
// 8 位），因此 pkg/qmi 的 readLoop 与 UnmarshalPacket 无需任何改动。这已
// 覆盖本项目实际使用的全部服务（均 <= 0x22）。Phase 1b（见
// qrtr_transport_plan.md 的 M4）将升级为支持 16 位服务号的 0x02 虚拟头，
// 以支持 >255 的 QRTR 服务（如 SSC 0x190、IMSDCM 0x302）。
// ============================================================================

var (
	// qrtrLookupTimeout bounds how long a synchronous NEW_LOOKUP exchange
	// (GetVersionInfo enumeration, or per-service resolution inside
	// AllocateClientID) may block Write() before giving up.
	qrtrLookupTimeout = 3 * time.Second
	// qrtrCtrlRecvPoll / qrtrClientRecvPoll bound each individual blocking
	// RecvFrom call so callers can periodically re-check closeCh / deadlines
	// instead of blocking indefinitely in a syscall.
	qrtrCtrlRecvPoll   = 200 * time.Millisecond
	qrtrClientRecvPoll = 200 * time.Millisecond
)

const (
	qrtrRxQueueSize  = 64
	qrtrMaxFrameSize = 16384 // matches client.go readLoop's read buffer size
)

// qrtrService is a resolved QRTR service endpoint learned from a NEW_SERVER
// control packet. qrtrService 是从 NEW_SERVER 控制包中解析出的已解析服务端点。
type qrtrService struct {
	node         uint32
	port         uint32
	versionMajor uint16 // best-effort, derived from the low byte of "instance"
}

// qrtrClient is one locally-synthesized QMI client (CID) bound to a single
// dedicated QRTR data socket connected to one service's {node,port}.
// qrtrClient 是一个本地合成的 QMI 客户端（CID），绑定到连接至某个服务
// {node,port} 的专属 QRTR 数据套接字。
type qrtrClient struct {
	clientID uint8
	service  uint16
	sock     qrtrRawSocket
	peer     sockaddrQRTR
}

// qrtrTransport implements qmiTransport over AF_QIPCRTR.
// qrtrTransport 基于 AF_QIPCRTR 实现 qmiTransport。
//
// qmi-go's Client only ever holds at most one client ID per service at a
// time (see Client.clientIDs, keyed by service only), so this transport
// keys open data sockets by service rather than by (service, clientID).
// qmi-go 的 Client 在任意时刻每个服务最多只持有一个客户端 ID（见
// Client.clientIDs，仅以 service 为键），因此本传输层以 service 而非
// (service, clientID) 作为已打开数据套接字的键。
type qrtrTransport struct {
	newSocket  func() (qrtrRawSocket, error)
	ctrlSock   qrtrRawSocket
	ctrlTarget sockaddrQRTR
	logf       ClientLogFunc

	mu       sync.Mutex
	closed   bool
	services map[uint16]qrtrService
	clients  map[uint16]*qrtrClient
	nextCID  uint8
	// enumGen increments each time runCtrlReader observes the nameserver's
	// zero-server sentinel that terminates a wildcard enumeration; it lets
	// enumerateServices know a NEW_LOOKUP(0) has fully completed.
	enumGen uint64
	// notifyCh is closed (and replaced) by broadcastLocked to wake every
	// goroutine waiting on a services/enumGen change. A close-and-replace
	// channel is used instead of sync.Cond because callers need timed waits.
	notifyCh chan struct{}

	rxCh      chan []byte
	closeCh   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	readDeadlineMu sync.Mutex
	readDeadline   time.Time
}

// openQRTRTransport is the production entry point wired into
// NewClientWithOptions via openQRTRTransportHook. openQRTRTransport 是通过
// openQRTRTransportHook 接入 NewClientWithOptions 的生产环境入口。
func openQRTRTransport(ctx context.Context, opts ClientOptions) (qmiTransport, error) {
	return newQRTRTransport(newQRTRRawSocket, opts.Logf)
}

func newQRTRTransport(newSocket func() (qrtrRawSocket, error), logf ClientLogFunc) (*qrtrTransport, error) {
	ctrlSock, err := newSocket()
	if err != nil {
		return nil, err
	}
	if err := ctrlSock.SetRecvTimeout(qrtrCtrlRecvPoll); err != nil {
		ctrlSock.Close()
		return nil, fmt.Errorf("qrtr: set control socket recv timeout: %w", err)
	}
	local, err := ctrlSock.LocalAddr()
	if err != nil {
		ctrlSock.Close()
		return nil, fmt.Errorf("qrtr: get local address: %w", err)
	}

	t := &qrtrTransport{
		newSocket: newSocket,
		ctrlSock:  ctrlSock,
		// NEW_LOOKUP requests are unicast to the nameserver (ns) running on
		// our own local node, not broadcast -- confirmed against upstream
		// tools/net/qrtr lookup.c, which reuses getsockname()'s node and
		// only overrides the port to QRTR_PORT_CTRL.
		// NEW_LOOKUP 请求是单播给运行在本机节点上的命名服务器（ns），而非
		// 广播——已对照上游 tools/net/qrtr 的 lookup.c 确认：它复用
		// getsockname() 得到的节点号，仅将端口覆盖为 QRTR_PORT_CTRL。
		ctrlTarget: sockaddrQRTR{node: local.node, port: qrtrPortCtrl},
		logf:       logf,
		services:   make(map[uint16]qrtrService),
		clients:    make(map[uint16]*qrtrClient),
		notifyCh:   make(chan struct{}),
		rxCh:       make(chan []byte, qrtrRxQueueSize),
		closeCh:    make(chan struct{}),
	}

	// A single dedicated reader owns ctrlSock: it continuously drains
	// NEW_SERVER/DEL_SERVER control messages and keeps t.services fresh
	// (handling the async DEL_SERVER notifications the old synchronous drain
	// ignored), while resolveService/enumerateServices only ever *send*
	// NEW_LOOKUP on ctrlSock. One reader + one sender avoids the
	// cross-lookup packet-contamination hazards of draining inline.
	t.wg.Add(1)
	go t.runCtrlReader()

	return t, nil
}

func (t *qrtrTransport) logging(level ClientLogLevel, format string, args ...any) {
	if t.logf != nil {
		t.logf(level, format, args...)
	}
}

// ============================================================================
// qmiTransport implementation / qmiTransport 接口实现
// ============================================================================

func (t *qrtrTransport) Read(p []byte) (int, error) {
	t.readDeadlineMu.Lock()
	dl := t.readDeadline
	t.readDeadlineMu.Unlock()

	var timerC <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case frame, ok := <-t.rxCh:
		if !ok {
			return 0, io.EOF
		}
		if len(frame) > len(p) {
			return 0, fmt.Errorf("qrtr: synthesized frame of %d bytes exceeds read buffer of %d bytes", len(frame), len(p))
		}
		return copy(p, frame), nil
	case <-timerC:
		return 0, os.ErrDeadlineExceeded
	case <-t.closeCh:
		return 0, io.EOF
	}
}

func (t *qrtrTransport) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)

	// Dispatches on the marker byte, so a caller building a Packet for a
	// service > 0xFF (e.g. IMSDCM 0x302) -- which Packet.Marshal() frames
	// with the 0x02 QRTR virtual header, not 0x01 QMUX -- is parsed
	// correctly here too. / 依据标记字节分流，因此调用方为 >0xFF 的服务
	// （如 IMSDCM 0x302）构建的 Packet——Packet.Marshal() 会用 0x02 QRTR
	// 虚拟头而非 0x01 QMUX 头封装——在此处也能被正确解析。
	fh, err := unmarshalFrameHeader(frame)
	if err != nil {
		return 0, fmt.Errorf("qrtr: write: %w", err)
	}
	if len(frame) < fh.headerSize {
		return 0, fmt.Errorf("qrtr: write: frame of %d bytes shorter than header size %d", len(frame), fh.headerSize)
	}
	body := frame[fh.headerSize:]

	if fh.serviceType == ServiceControl {
		if err := t.handleCTLWrite(body); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if err := t.handleDataWrite(fh.serviceType, fh.clientID, body); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *qrtrTransport) SetReadDeadline(dl time.Time) error {
	t.readDeadlineMu.Lock()
	t.readDeadline = dl
	t.readDeadlineMu.Unlock()
	return nil
}

func (t *qrtrTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closeCh)
		t.mu.Lock()
		// Set closed before releasing the lock (and before wg.Wait below) so
		// a concurrent handleAllocateClientID -- which may still run once from
		// writerLoop after closeCh is closed -- observes it and refuses to
		// touch the (about-to-be-nil) map or call wg.Add after wg.Wait.
		t.closed = true
		for _, cl := range t.clients {
			cl.sock.Close()
		}
		t.clients = nil
		t.broadcastLocked() // wake any resolveService/enumerateServices waiters
		t.mu.Unlock()
		if t.ctrlSock != nil {
			t.ctrlSock.Close() // unblocks runCtrlReader
		}
		t.wg.Wait()
	})
	return nil
}

// ============================================================================
// Local CTL simulation / 本地 CTL 模拟
// ============================================================================

func (t *qrtrTransport) handleCTLWrite(body []byte) error {
	ctlH, err := UnmarshalCTLHeader(body)
	if err != nil {
		return fmt.Errorf("qrtr: ctl header: %w", err)
	}
	tlvData := body[CTLHeaderSize:]
	if int(ctlH.Length) > len(tlvData) {
		return fmt.Errorf("qrtr: ctl tlv truncated: need %d, have %d", ctlH.Length, len(tlvData))
	}
	tlvs, err := ParseTLVs(tlvData[:ctlH.Length])
	if err != nil {
		return fmt.Errorf("qrtr: ctl tlv parse: %w", err)
	}

	switch ctlH.MessageID {
	case CTLSync:
		return t.replyCTL(ctlH.TransactionID, CTLSync, []TLV{qrtrSuccessResultTLV()})
	case CTLGetVersionInfo:
		return t.handleGetVersionInfo(ctlH.TransactionID)
	case CTLGetClientID:
		return t.handleAllocateClientID(ctlH.TransactionID, tlvs)
	case CTLReleaseClientID:
		return t.handleReleaseClientID(ctlH.TransactionID, tlvs)
	default:
		// Notably CTLInternalProxyOpen: qmi-proxy multiplexing is meaningless
		// over QRTR (there is no shared cdc-wdm device to multiplex), and
		// ClientOptions wiring (NewClientWithOptions) never issues it when
		// UseQRTR is set. Reaching here means a misconfiguration.
		return fmt.Errorf("qrtr: CTL message 0x%04x is not supported over QRTR transport", ctlH.MessageID)
	}
}

func (t *qrtrTransport) handleGetVersionInfo(txID uint8) error {
	t.enumerateServices()

	// Snapshot the services runCtrlReader has recorded, keeping only those
	// addressable through the real 1-byte-service CTL_GET_VERSION_INFO reply
	// format. Services > 0xFF (QRTR-only) can't be reported here -- a
	// permanent constraint of the real QMI CTL message, not a gap; HasService/
	// ensureServiceAllocatable stay optimistic for them instead.
	t.mu.Lock()
	type versionEntry struct {
		service uint16
		major   uint16
	}
	list := make([]versionEntry, 0, len(t.services))
	for id, s := range t.services {
		if id > 0xff {
			continue
		}
		list = append(list, versionEntry{service: id, major: s.versionMajor})
	}
	t.mu.Unlock()

	var entries []byte
	for _, e := range list {
		entry := make([]byte, 5)
		entry[0] = byte(e.service)
		binary.LittleEndian.PutUint16(entry[1:3], e.major) // best-effort major version (NEW_SERVER instance low byte)
		binary.LittleEndian.PutUint16(entry[3:5], 0)       // minor version is not carried by QRTR NEW_SERVER
		entries = append(entries, entry...)
	}

	tlvValue := append([]byte{byte(len(list))}, entries...)
	return t.replyCTL(txID, CTLGetVersionInfo, []TLV{qrtrSuccessResultTLV(), {Type: 0x01, Value: tlvValue}})
}

func (t *qrtrTransport) handleAllocateClientID(txID uint8, tlvs []TLV) error {
	tlv := FindTLV(tlvs, 0x01)
	if tlv == nil {
		return t.replyCTLError(txID, CTLGetClientID, QMIErrMalformedMsg)
	}
	service, ok := decodeCTLServiceOnlyTLV(tlv.Value)
	if !ok {
		return t.replyCTLError(txID, CTLGetClientID, QMIErrMalformedMsg)
	}

	srv, err := t.resolveService(service)
	if err != nil {
		t.logging(ClientLogLevelDebug, "QMI/QRTR: NEW_LOOKUP for service 0x%04x failed: %v", service, err)
		return t.replyCTLError(txID, CTLGetClientID, QMIErrDeviceNotReady)
	}

	sock, err := t.newSocket()
	if err != nil {
		return fmt.Errorf("qrtr: open data socket for service 0x%04x: %w", service, err)
	}
	if err := sock.SetRecvTimeout(qrtrClientRecvPoll); err != nil {
		sock.Close()
		return fmt.Errorf("qrtr: set data socket recv timeout: %w", err)
	}

	cid := t.allocateCID()
	cl := &qrtrClient{
		clientID: cid,
		service:  service,
		sock:     sock,
		peer:     sockaddrQRTR{node: srv.node, port: srv.port},
	}

	t.mu.Lock()
	if t.closed {
		// Raced with Close(): don't write to the nil-ed map or Add to the
		// WaitGroup that Close is already Wait-ing on. Drop the fresh socket.
		t.mu.Unlock()
		sock.Close()
		return t.replyCTLError(txID, CTLGetClientID, QMIErrDeviceNotReady)
	}
	if old, exists := t.clients[service]; exists {
		// Re-allocating without a prior release (e.g. a retried request):
		// close the stale socket instead of leaking it.
		old.sock.Close()
	}
	t.clients[service] = cl
	// wg.Add is done under t.mu while closed==false; Close sets closed==true
	// under the same lock before it calls wg.Wait, so Add can never race with
	// (or follow) that Wait.
	t.wg.Add(1)
	t.mu.Unlock()

	go t.runClientReader(service, cl)

	return t.replyCTL(txID, CTLGetClientID, []TLV{qrtrSuccessResultTLV(), encodeCTLServiceClientIDTLV(service, cid)})
}

func (t *qrtrTransport) handleReleaseClientID(txID uint8, tlvs []TLV) error {
	tlv := FindTLV(tlvs, 0x01)
	if tlv == nil {
		return t.replyCTLError(txID, CTLReleaseClientID, QMIErrMalformedMsg)
	}
	service, _, ok := decodeCTLServiceClientIDTLV(tlv.Value)
	if !ok {
		return t.replyCTLError(txID, CTLReleaseClientID, QMIErrMalformedMsg)
	}

	t.mu.Lock()
	cl, ok := t.clients[service]
	if ok {
		delete(t.clients, service)
	}
	t.mu.Unlock()

	if ok {
		cl.sock.Close() // unblocks runClientReader's RecvFrom, letting it exit
	}

	return t.replyCTL(txID, CTLReleaseClientID, []TLV{qrtrSuccessResultTLV()})
}

func (t *qrtrTransport) allocateCID() uint8 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextCID++
	if t.nextCID == 0 {
		t.nextCID = 1
	}
	return t.nextCID
}

// broadcastLocked wakes every goroutine waiting on a services/enumGen change.
// Caller must hold t.mu.
func (t *qrtrTransport) broadcastLocked() {
	close(t.notifyCh)
	t.notifyCh = make(chan struct{})
}

// runCtrlReader is the sole reader of ctrlSock. It continuously drains the
// nameserver's control stream, keeping t.services current: NEW_SERVER adds
// (or refreshes) an entry, DEL_SERVER removes one, and the all-zero
// NEW_SERVER sentinel bumps enumGen to mark a wildcard enumeration complete.
// Every mutation broadcasts so resolveService/enumerateServices waiters wake.
func (t *qrtrTransport) runCtrlReader() {
	defer t.wg.Done()
	buf := make([]byte, qrtrCtrlPktSize)
	for {
		n, _, err := t.ctrlSock.RecvFrom(buf)
		if err != nil {
			if isQRTRRetryable(err) {
				select {
				case <-t.closeCh:
					return
				default:
					continue
				}
			}
			return // ctrlSock closed (transport Close) or a fatal error
		}
		pkt, perr := unmarshalQRTRCtrlPkt(buf[:n])
		if perr != nil {
			continue // short/malformed control datagram: ignore
		}
		switch pkt.cmd {
		case qrtrTypeNewServer:
			t.mu.Lock()
			if pkt.isZeroServer() {
				t.enumGen++
			} else {
				t.services[uint16(pkt.service)] = qrtrService{
					node:         pkt.node,
					port:         pkt.port,
					versionMajor: uint16(pkt.instance & 0xff),
				}
			}
			t.broadcastLocked()
			t.mu.Unlock()
		case qrtrTypeDelServer:
			t.mu.Lock()
			delete(t.services, uint16(pkt.service))
			t.broadcastLocked()
			t.mu.Unlock()
		}
	}
}

// resolveService returns the {node,port} for a service, sending a
// NEW_LOOKUP(service) and waiting (up to qrtrLookupTimeout) for runCtrlReader
// to record the reply. It returns as soon as the service appears, so it never
// stalls waiting for a terminating sentinel the nameserver may not send for a
// filtered lookup.
func (t *qrtrTransport) resolveService(service uint16) (qrtrService, error) {
	t.mu.Lock()
	if srv, ok := t.services[service]; ok {
		t.mu.Unlock()
		return srv, nil
	}
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return qrtrService{}, fmt.Errorf("qrtr: transport closed")
	}

	req := marshalQRTRCtrlPkt(newLookupRequest(uint32(service)))
	if err := t.ctrlSock.SendTo(req[:], t.ctrlTarget); err != nil {
		return qrtrService{}, fmt.Errorf("qrtr: send NEW_LOOKUP(service=%d): %w", service, err)
	}

	deadline := time.Now().Add(qrtrLookupTimeout)
	t.mu.Lock()
	for {
		if srv, ok := t.services[service]; ok {
			t.mu.Unlock()
			return srv, nil
		}
		if t.closed {
			t.mu.Unlock()
			return qrtrService{}, fmt.Errorf("qrtr: transport closed")
		}
		if !t.waitChangeLocked(deadline) {
			t.mu.Unlock()
			return qrtrService{}, fmt.Errorf("qrtr: service 0x%04x not found via NEW_LOOKUP", service)
		}
	}
}

// enumerateServices sends a wildcard NEW_LOOKUP(0) and waits (up to
// qrtrLookupTimeout) for runCtrlReader to observe the terminating zero-server
// sentinel (enumGen bump) that means every currently-registered service has
// been reported. Best-effort: on timeout it simply returns with whatever
// runCtrlReader has recorded so far.
func (t *qrtrTransport) enumerateServices() {
	t.mu.Lock()
	startGen := t.enumGen
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return
	}

	req := marshalQRTRCtrlPkt(newLookupRequest(0))
	if err := t.ctrlSock.SendTo(req[:], t.ctrlTarget); err != nil {
		t.logging(ClientLogLevelDebug, "QMI/QRTR: send wildcard NEW_LOOKUP failed: %v", err)
		return
	}

	deadline := time.Now().Add(qrtrLookupTimeout)
	t.mu.Lock()
	for t.enumGen == startGen && !t.closed {
		if !t.waitChangeLocked(deadline) {
			break
		}
	}
	t.mu.Unlock()
}

// waitChangeLocked releases t.mu, blocks until broadcastLocked fires, the
// deadline passes, or the transport closes, then re-acquires t.mu. It returns
// false if it stopped because of the deadline (or close) rather than a
// broadcast. Caller must hold t.mu.
func (t *qrtrTransport) waitChangeLocked(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	ch := t.notifyCh
	t.mu.Unlock()

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	woke := false
	select {
	case <-ch:
		woke = true
	case <-timer.C:
	case <-t.closeCh:
	}

	t.mu.Lock()
	return woke
}

// ============================================================================
// Data path / 数据通路
// ============================================================================

func (t *qrtrTransport) handleDataWrite(service uint16, clientID uint8, body []byte) error {
	t.mu.Lock()
	cl, ok := t.clients[service]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("qrtr: write to service 0x%04x with no open client socket (missing AllocateClientID?)", service)
	}
	if cl.clientID != clientID {
		t.logging(ClientLogLevelWarn, "QMI/QRTR: write clientID=%d does not match allocated clientID=%d for service 0x%04x; routing by service anyway",
			clientID, cl.clientID, service)
	}
	return cl.sock.SendTo(body, cl.peer)
}

func (t *qrtrTransport) runClientReader(service uint16, cl *qrtrClient) {
	defer t.wg.Done()
	// Reserve header room: rebuildFrame prepends up to QrtrHeaderSize bytes,
	// and the resulting frame must fit the client readLoop's read buffer
	// (also qrtrMaxFrameSize). Without this headroom, an SDU in
	// [qrtrMaxFrameSize-QrtrHeaderSize, qrtrMaxFrameSize] would produce a
	// frame larger than that buffer; Read() would then return a non-timeout
	// error, which readLoop treats as fatal and would permanently kill the
	// transport. A datagram larger than this is truncated by recvfrom -- the
	// same effective single-read ceiling the QMUX path has -- and later
	// dropped as a parse error rather than crashing the transport.
	buf := make([]byte, qrtrMaxFrameSize-QrtrHeaderSize)
	for {
		n, _, err := cl.sock.RecvFrom(buf)
		if err != nil {
			if isQRTRRetryable(err) {
				select {
				case <-t.closeCh:
					return
				default:
					continue
				}
			}
			return // socket closed (release/Close) or a fatal error: stop reading
		}
		if n <= 0 {
			continue
		}
		t.pushRx(rebuildFrame(service, cl.clientID, buf[:n]))
	}
}

func (t *qrtrTransport) pushRx(frame []byte) {
	select {
	case t.rxCh <- frame:
	case <-t.closeCh:
	}
}

// rebuildFrame prepends the outer frame header to a raw QRTR QMI SDU via the
// same marshalFrameHeader helper Packet.Marshal() uses: a real 6-byte QMUX
// (0x01) header for service <= 0xFF (indistinguishable from what a real
// cdc-wdm device would have sent, so pkg/qmi's readLoop needs no awareness
// of QRTR at all), or the synthetic 7-byte QRTR virtual (0x02) header for
// services beyond the 8-bit QMUX range. rebuildFrame 通过与
// Packet.Marshal() 相同的 marshalFrameHeader 辅助函数为原始 QRTR QMI SDU
// 补上外层帧头：service <= 0xFF 时使用真实的 6 字节 QMUX（0x01）头（与真实
// cdc-wdm 设备发出的字节完全一致，因此 pkg/qmi 的 readLoop 完全不需要感知
// QRTR 的存在），超出 8 位 QMUX 范围的服务则使用合成的 7 字节 QRTR 虚拟
// （0x02）头。
func rebuildFrame(service uint16, clientID uint8, sdu []byte) []byte {
	return append(marshalFrameHeader(service, clientID, len(sdu)), sdu...)
}

func qrtrSuccessResultTLV() TLV {
	return TLV{Type: 0x02, Value: []byte{0x00, 0x00, 0x00, 0x00}}
}

func qrtrErrorResultTLV(code uint16) TLV {
	return TLV{Type: 0x02, Value: []byte{0x01, 0x00, byte(code), byte(code >> 8)}}
}

func (t *qrtrTransport) replyCTL(txID uint8, msgID uint16, tlvs []TLV) error {
	t.pushRx(marshalCTLResponseFrame(txID, msgID, tlvs))
	return nil
}

// marshalCTLResponseFrame builds a CTL response frame the way a real modem
// would: ControlFlags 0x01 (response), not 0x00 (request). Packet.Marshal()
// always emits 0x00 (it is meant for outgoing requests), so the synthesized
// CTL replies are assembled directly here instead. Functionally the readLoop
// only inspects the indication bit (0x02), but emitting the correct response
// flag keeps the synthesized frames faithful to the wire format.
func marshalCTLResponseFrame(txID uint8, msgID uint16, tlvs []TLV) []byte {
	var tlvBytes []byte
	for _, tv := range tlvs {
		tlvBytes = append(tlvBytes, tv.Marshal()...)
	}
	ctlH := CTLHeader{
		ControlFlags:  0x01, // response
		TransactionID: txID,
		MessageID:     msgID,
		Length:        uint16(len(tlvBytes)),
	}
	body := append(ctlH.Marshal(), tlvBytes...)
	// ServiceControl (0) always fits the 8-bit QMUX header, so this yields a
	// byte-identical 0x01 QMUX frame -- exactly what a cdc-wdm CTL response
	// looks like.
	return append(marshalFrameHeader(ServiceControl, 0, len(body)), body...)
}

func (t *qrtrTransport) replyCTLError(txID uint8, msgID uint16, errCode uint16) error {
	return t.replyCTL(txID, msgID, []TLV{qrtrErrorResultTLV(errCode)})
}
