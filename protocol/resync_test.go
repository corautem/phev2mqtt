package protocol

import "testing"

func TestResyncSend(t *testing.T) {
	k := &SecurityKey{}
	if k.ResyncSend(0x42) {
		t.Error("resynced without a key map")
	}
	k.Update(mustHex(t, "4e0d000102030405060708090a0b"))
	var keys []byte
	for i := 0; i < 8; i++ {
		keys = append(keys, k.SKey(true))
	}
	// Point back at the fifth key: the next send uses it, then moves on.
	if !k.ResyncSend(keys[4]) {
		t.Fatal("key from the key map not found")
	}
	if got := k.SKey(true); got != keys[4] {
		t.Errorf("after resync: key 0x%02x, want 0x%02x", got, keys[4])
	}
	if got := k.SKey(false); got != keys[5] {
		t.Errorf("then: key 0x%02x, want 0x%02x", got, keys[5])
	}
}
