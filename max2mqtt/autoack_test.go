package main

import "testing"

func TestGatewayAddrHex(t *testing.T) {
	old := config.GatewayID
	defer func() { config.GatewayID = old }()

	cases := map[string]string{
		"123456": "123456",
		"0a1b2c": "0A1B2C",
		"bad":    "123456", // invalid config falls back to the default address
	}
	for in, want := range cases {
		config.GatewayID = in
		if got := gatewayAddrHex(); got != want {
			t.Errorf("gatewayAddrHex(%q) = %q, want %q", in, got, want)
		}
	}
}

// The CUL reports each auto-Ack it sends as a normal "Z..." line.
// Its source is the gateway address, so handleSerialMessage can recognise it.
func TestOwnAutoAckIsFromGateway(t *testing.T) {
	old := config.GatewayID
	defer func() { config.GatewayID = old }()
	config.GatewayID = "123456"

	// Ack (type 02) from gateway 123456 to device 0A0B0C, counter 0x79, payload 00
	pkt, err := parseMaxMessage("Z0B7900021234560A0B0C0000")
	if err != nil {
		t.Fatalf("parseMaxMessage: %v", err)
	}
	if pkt.Type != 0x02 || pkt.SrcAddr != gatewayAddrHex() || pkt.DstAddr != "0A0B0C" {
		t.Errorf("got type %#x src %s dst %s, want 0x02 from %s to 0A0B0C", pkt.Type, pkt.SrcAddr, pkt.DstAddr, gatewayAddrHex())
	}
}
