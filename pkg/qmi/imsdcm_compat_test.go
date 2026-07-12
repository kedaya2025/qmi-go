package qmi

import (
	"reflect"
	"testing"
)

// This file originally documented that IMSDCM (QRTR service 0x302, > 0xFF)
// could not be addressed because ServiceType was uint8-only. That migration
// (qrtr_transport_plan.md Phase 1b / M4) has landed: these tests now assert
// the flipped, positive behavior instead of the old tripwire.

func TestIMSDCMServiceIDExceeds8BitQMUXRange(t *testing.T) {
	const imsdcmServiceID uint16 = 0x302

	if imsdcmServiceID <= 0xFF {
		t.Fatalf("expected IMSDCM service ID to exceed uint8, got 0x%X", imsdcmServiceID)
	}
}

// TestClientAPIsAddress16BitServiceIDs is the flipped form of the old
// TestCurrentQMUXClientUses8BitServiceIdentifiers tripwire: it now asserts
// that the Client API speaks 16-bit service IDs end to end, while
// QmuxHeader intentionally stays 8-bit (see marshalFrameHeader in frame.go)
// because that is the real QMUX wire format's own permanent ceiling, not
// something this migration could or should change.
func TestClientAPIsAddress16BitServiceIDs(t *testing.T) {
	if got := reflect.TypeOf(Packet{}.ServiceType).Kind(); got != reflect.Uint16 {
		t.Fatalf("Packet.ServiceType kind = %v, want uint16", got)
	}
	if got := reflect.TypeOf(Event{}.ServiceID).Kind(); got != reflect.Uint16 {
		t.Fatalf("Event.ServiceID kind = %v, want uint16", got)
	}

	var c Client
	if got := reflect.TypeOf(c.clientIDs).Key().Kind(); got != reflect.Uint16 {
		t.Fatalf("Client.clientIDs key kind = %v, want uint16", got)
	}

	sendRequestType := reflect.TypeOf((*Client).SendRequest)
	if got := sendRequestType.In(2).Kind(); got != reflect.Uint16 {
		t.Fatalf("SendRequest service arg kind = %v, want uint16", got)
	}
	if got := sendRequestType.In(3).Kind(); got != reflect.Uint8 {
		t.Fatalf("SendRequest clientID arg kind = %v, want uint8 (client IDs remain 1 byte on both QMUX and QRTR)", got)
	}

	allocateType := reflect.TypeOf((*Client).AllocateClientID)
	if got := allocateType.In(1).Kind(); got != reflect.Uint16 {
		t.Fatalf("AllocateClientID service arg kind = %v, want uint16", got)
	}

	releaseType := reflect.TypeOf((*Client).ReleaseClientID)
	if got := releaseType.In(1).Kind(); got != reflect.Uint16 {
		t.Fatalf("ReleaseClientID service arg kind = %v, want uint16", got)
	}

	// QmuxHeader mirrors the real wire-format QMUX header and intentionally
	// stays 8-bit: that is the actual protocol's ceiling, not a code choice.
	if got := reflect.TypeOf(QmuxHeader{}.ServiceType).Kind(); got != reflect.Uint8 {
		t.Fatalf("QmuxHeader.ServiceType kind = %v, want uint8 (real QMUX wire format is 8-bit)", got)
	}
}

// TestPacketMarshalUsesQMUXHeaderForNarrowServices is the flipped form of
// the old TestCurrentFramingIsQMUXOnly: services that fit in 8 bits still
// get the byte-identical real QMUX (0x01) wire header -- zero behavior
// change for real cdc-wdm/qmi-proxy devices, none of which ever expose a
// service > 0xFF.
func TestPacketMarshalUsesQMUXHeaderForNarrowServices(t *testing.T) {
	packet := Packet{
		ServiceType: ServiceIMS,
		ClientID:    0x01,
		MessageID:   0x1234,
	}

	raw := packet.Marshal()
	if len(raw) < QmuxHeaderSize {
		t.Fatalf("marshal returned %d bytes, want at least %d", len(raw), QmuxHeaderSize)
	}
	if raw[0] != 0x01 {
		t.Fatalf("marker = 0x%02X, want 0x01 (QMUX) for narrow service 0x%02x", raw[0], packet.ServiceType)
	}

	// The low-level, wire-format-specific QMUX header parser must remain
	// QMUX-only; the 0x01/0x02 dispatch lives one layer up in
	// UnmarshalPacket (via unmarshalFrameHeader), not inside
	// UnmarshalQmuxHeader itself.
	raw[0] = 0x02
	if _, err := UnmarshalQmuxHeader(raw[:QmuxHeaderSize]); err == nil {
		t.Fatal("expected QRTR-style marker to be rejected by UnmarshalQmuxHeader")
	}
}

// TestPacketMarshalUsesQRTRVirtualHeaderForWideServices proves services
// beyond the 8-bit QMUX range now get the synthetic QRTR virtual (0x02)
// header instead of being truncated/misrouted.
func TestPacketMarshalUsesQRTRVirtualHeaderForWideServices(t *testing.T) {
	const imsdcmServiceID uint16 = 0x302
	packet := Packet{
		ServiceType: imsdcmServiceID,
		ClientID:    0x05,
		MessageID:   0x0001,
		TLVs:        []TLV{NewTLVUint8(0x10, 0x42)},
	}

	raw := packet.Marshal()
	if len(raw) < QrtrHeaderSize {
		t.Fatalf("marshal returned %d bytes, want at least %d", len(raw), QrtrHeaderSize)
	}
	if raw[0] != 0x02 {
		t.Fatalf("marker = 0x%02X, want 0x02 (QRTR virtual header) for wide service 0x%04x", raw[0], imsdcmServiceID)
	}
}

// TestPacketRoundTripsThroughQRTRVirtualHeader proves a wide-service packet
// survives a full Marshal -> UnmarshalPacket round trip without truncation.
func TestPacketRoundTripsThroughQRTRVirtualHeader(t *testing.T) {
	const imsdcmServiceID uint16 = 0x302
	original := Packet{
		ServiceType:   imsdcmServiceID,
		ClientID:      0x09,
		TransactionID: 0xBEEF,
		MessageID:     0x0021,
		TLVs:          []TLV{NewTLVUint32(0x01, 0xDEADBEEF)},
	}
	raw := original.Marshal()

	got, err := UnmarshalPacket(raw)
	if err != nil {
		t.Fatalf("UnmarshalPacket() error = %v", err)
	}
	if got.ServiceType != original.ServiceType {
		t.Fatalf("ServiceType = 0x%04x, want 0x%04x", got.ServiceType, original.ServiceType)
	}
	if got.ClientID != original.ClientID {
		t.Fatalf("ClientID = 0x%02x, want 0x%02x", got.ClientID, original.ClientID)
	}
	if got.TransactionID != original.TransactionID {
		t.Fatalf("TransactionID = 0x%04x, want 0x%04x", got.TransactionID, original.TransactionID)
	}
	if got.MessageID != original.MessageID {
		t.Fatalf("MessageID = 0x%04x, want 0x%04x", got.MessageID, original.MessageID)
	}
	if len(got.TLVs) != 1 || got.TLVs[0].Type != 0x01 {
		t.Fatalf("TLVs = %+v, want one TLV of type 0x01", got.TLVs)
	}
}

// TestReadLoopFrameSyncAcceptsBothMarkers documents that Client.readLoop's
// frame resynchronization (client.go) accepts both the 0x01 QMUX and 0x02
// QRTR-virtual marker bytes -- exercised indirectly here via
// unmarshalFrameHeader, the same dispatcher UnmarshalPacket and qrtrTransport
// use, since readLoop's own resync loop is unexported and only reachable via
// a live transport.
func TestUnmarshalFrameHeaderDispatchesOnMarkerByte(t *testing.T) {
	narrow := (&Packet{ServiceType: ServiceIMS, MessageID: 0x1}).Marshal()
	if narrow[0] != 0x01 {
		t.Fatalf("narrow marker = 0x%02x, want 0x01", narrow[0])
	}

	const imsdcmServiceID uint16 = 0x302
	wide := (&Packet{ServiceType: imsdcmServiceID, MessageID: 0x1}).Marshal()
	if wide[0] != 0x02 {
		t.Fatalf("wide marker = 0x%02x, want 0x02", wide[0])
	}

	if _, err := UnmarshalPacket(narrow); err != nil {
		t.Fatalf("UnmarshalPacket(narrow) error = %v", err)
	}
	if _, err := UnmarshalPacket(wide); err != nil {
		t.Fatalf("UnmarshalPacket(wide) error = %v", err)
	}
}
