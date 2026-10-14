package protocol

import "testing"

func TestChargePlugDecode(t *testing.T) {
	tests := []struct {
		data string
		want bool
	}{
		{"0000", false},
		{"0001", true}, // plugged, waiting for the charge timer
		{"0002", true}, // plugged inside the charge window, not charging
		{"0202", true}, // charging
		{"0003", false},
	}
	for _, test := range tests {
		r := &RegisterChargePlug{}
		r.Decode(&PhevMessage{Register: ChargePlugRegister, Data: mustHex(t, test.data)})
		if r.Connected != test.want {
			t.Errorf("0x1e=%s: Connected=%v, want %v", test.data, r.Connected, test.want)
		}
	}
}

func TestLightStatusDecode(t *testing.T) {
	tests := []struct {
		data             string
		interior, hazard bool
	}{
		{"0000000202", false, false},
		{"0000000101", true, true},
		{"0000000203", true, false}, // interior light value 3 also means on
	}
	for _, test := range tests {
		r := &RegisterLightStatus{}
		r.Decode(&PhevMessage{Register: LightStatusRegister, Data: mustHex(t, test.data)})
		if r.Interior != test.interior || r.Hazard != test.hazard {
			t.Errorf("0x23=%s: interior=%v hazard=%v, want %v %v", test.data, r.Interior, r.Hazard, test.interior, test.hazard)
		}
	}
}
