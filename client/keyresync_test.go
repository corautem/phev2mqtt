package client

import (
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/buxtronix/phev2mqtt/protocol"
)

// keyCar is a fake MY14 car that checks the XOR key of every packet the
// client sends, like the real car: a packet with the wrong key gets an
// encoding complaint (0xbb) naming the key the car expected.
type keyCar struct {
	t    *testing.T
	l    net.Listener
	conn net.Conn
	wmu  sync.Mutex
	key  *protocol.SecurityKey

	mu         sync.Mutex
	complaints int
	writes     []string
	rekeys     int
	// lostAcks client acks are "lost": not counted, so the client's key
	// index runs ahead of the car's, as in a field log.
	lostAcks int
	// rejectWrites makes the car complain about every write, each time
	// naming a different key.
	rejectWrites bool
	nextBadKey   byte
	seed         byte
	started      bool
}

func newKeyCar(t *testing.T) *keyCar {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	car := &keyCar{t: t, l: l, key: &protocol.SecurityKey{}}
	t.Cleanup(func() { l.Close() })
	return car
}

func (car *keyCar) send(m *protocol.PhevMessage) {
	car.wmu.Lock()
	defer car.wmu.Unlock()
	if car.conn != nil {
		car.conn.Write(m.EncodeToBytes(&protocol.SecurityKey{}))
	}
}

// start accepts the client; like the real car, it starts the session (key
// seeded by seed) once the client pings.
func (car *keyCar) start(seed byte) {
	c, err := car.l.Accept()
	if err != nil {
		return
	}
	car.wmu.Lock()
	car.conn = c
	car.wmu.Unlock()
	car.mu.Lock()
	car.seed = seed
	car.mu.Unlock()
	go car.serve()
}

// newKey sends a start packet; both sides derive the key map from it.
func (car *keyCar) newKey(seed byte) {
	data := []byte{seed, seed + 8, seed + 16, seed + 24, seed + 32, seed + 40, seed + 48, seed + 56, 9, 10}
	start := protocol.NewMessage(protocol.CmdInMy14StartReq, 0x01, false, data)
	b := start.EncodeToBytes(&protocol.SecurityKey{})
	car.mu.Lock()
	car.key.Update(b)
	car.mu.Unlock()
	car.wmu.Lock()
	car.conn.Write(b)
	car.wmu.Unlock()
}

func (car *keyCar) complain(expected byte) {
	car.send(protocol.NewMessage(protocol.CmdInBadEncoding, 0x00, false, []byte{expected}))
}

func (car *keyCar) serve() {
	buf := make([]byte, 4096)
	for {
		n, err := car.conn.Read(buf)
		if err != nil {
			return
		}
		for _, m := range protocol.NewFromBytes(buf[:n], &protocol.SecurityKey{}) {
			switch {
			case m.Type == protocol.CmdOutPingReq:
				car.send(protocol.NewPingResponseMessage(m.Register))
				car.mu.Lock()
				first := !car.started
				car.started = true
				seed := car.seed
				car.mu.Unlock()
				if first {
					car.newKey(seed)
				}
			case m.Type == protocol.CmdOutMy14StartResp && m.Ack == protocol.Request:
				car.mu.Lock()
				car.rekeys++
				car.mu.Unlock()
			case m.Type == protocol.CmdOutSend:
				car.mu.Lock()
				if m.Ack == protocol.Ack && car.lostAcks > 0 {
					car.lostAcks--
					car.mu.Unlock()
					continue
				}
				expected := car.key.SKey(false)
				if car.rejectWrites && m.Ack == protocol.Request {
					car.complaints++
					car.nextBadKey++
					bad := car.nextBadKey
					car.mu.Unlock()
					car.complain(bad)
					continue
				}
				if m.Xor != expected {
					car.complaints++
					car.mu.Unlock()
					car.complain(expected)
					continue
				}
				car.key.SKey(true)
				if m.Ack == protocol.Request {
					car.writes = append(car.writes, hex.EncodeToString([]byte{m.Register})+"="+hex.EncodeToString(m.Data))
				}
				car.mu.Unlock()
				if m.Ack == protocol.Request {
					car.send(protocol.NewMessage(protocol.CmdInResp, m.Register, true, []byte{0x0}))
				}
			}
		}
	}
}

func (car *keyCar) counts() (complaints, rekeys int, writes []string) {
	car.mu.Lock()
	defer car.mu.Unlock()
	return car.complaints, car.rekeys, append([]string(nil), car.writes...)
}

// connect starts a client against the car and a loop that acks the car's
// notifications, like the MQTT gateway; it counts the complaints it sees.
func connect(t *testing.T, car *keyCar, resync bool) (*Client, func() int) {
	cl, err := New(AddressOption(car.l.Addr().String()), KeyResyncOption(resync))
	if err != nil {
		t.Fatal(err)
	}
	go car.start(0x11)
	if err := cl.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := 0
	go func() {
		for m := range cl.Recv {
			if m.Type == protocol.CmdInBadEncoding {
				mu.Lock()
				seen++
				mu.Unlock()
			}
			if m.Type == protocol.CmdInResp && m.Ack == protocol.Request {
				cl.SendMessage(&protocol.PhevMessage{Type: protocol.CmdOutSend, Register: m.Register, Ack: protocol.Ack, Data: []byte{0x0}})
			}
		}
	}()
	return cl, func() int { mu.Lock(); defer mu.Unlock(); return seen }
}

// driftKeys has the car lose two of the client's acks, so the client's key
// index is two ahead of the car's.
func driftKeys(t *testing.T, car *keyCar) {
	car.mu.Lock()
	car.lostAcks = 2
	car.mu.Unlock()
	car.send(protocol.NewMessage(protocol.CmdInResp, 0x1d, false, []byte{0x50, 0, 0, 0}))
	car.send(protocol.NewMessage(protocol.CmdInResp, 0x24, false, make([]byte, 10)))
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		car.mu.Lock()
		lost := car.lostAcks
		car.mu.Unlock()
		if lost == 0 {
			return
		}
	}
	t.Fatal("the client never acked the notifications")
}

// Without resync, a drifted key makes every resend wrong: the complaints
// spiral until the write times out (in the field: "Disconnecting due to
// too many errors").
func TestKeyDriftSpiralsWithoutResync(t *testing.T) {
	car := newKeyCar(t)
	cl, _ := connect(t, car, false)
	driftKeys(t, car)

	done := make(chan error, 1)
	go func() { done <- cl.SetRegister(0x0a, []byte{0x01}) }()
	time.Sleep(time.Second)
	// Every resend moves one key further on, so the write gets through only
	// when the index wraps round, after up to 255 complaints. The gateway
	// gives up after 50 in 15 seconds.
	if complaints, _, _ := car.counts(); complaints < 50 {
		t.Errorf("complaints = %d: expected the spiral this test documents", complaints)
	}
	cl.Close()
	<-done
}

func TestKeyResyncRecoversFromDrift(t *testing.T) {
	car := newKeyCar(t)
	cl, _ := connect(t, car, true)
	driftKeys(t, car)

	if err := cl.SetRegister(0x0a, []byte{0x01}); err != nil {
		t.Fatalf("SetRegister: %v", err)
	}
	complaints, rekeys, writes := car.counts()
	if complaints != 1 || len(writes) != 1 || writes[0] != "0a=01" || rekeys != 0 {
		t.Errorf("complaints %d, rekeys %d, writes %v; want 1 complaint, the write, no rekey", complaints, rekeys, writes)
	}
	// The keys stay in step: the next write goes through first time.
	if err := cl.SetRegister(0x0b, []byte{0x02}); err != nil {
		t.Fatalf("second SetRegister: %v", err)
	}
	if complaints, _, writes := car.counts(); complaints != 1 || len(writes) != 2 {
		t.Errorf("after a second write: complaints %d, writes %v", complaints, writes)
	}
}

func TestKeyResyncDropsRepeatedComplaints(t *testing.T) {
	car := newKeyCar(t)
	_, seen := connect(t, car, true)
	car.complain(0x42)
	car.complain(0x42) // the same complaint again: refers to a packet already resynced
	car.complain(0x43)
	time.Sleep(300 * time.Millisecond)
	if got := seen(); got != 2 {
		t.Errorf("complaints passed on = %d, want 2 (the repeat dropped)", got)
	}
}

// Many complaints ask the car for a new key, once; the car's new start
// packets (one more than the start signal's buffer) don't block the client.
func TestKeyResyncRequestsNewKey(t *testing.T) {
	car := newKeyCar(t)
	cl, _ := connect(t, car, true)
	for i := byte(0); i < 7; i++ {
		car.complain(0x30 + i)
	}
	time.Sleep(300 * time.Millisecond)
	if _, rekeys, _ := car.counts(); rekeys != 1 {
		t.Errorf("new key requests = %d, want 1", rekeys)
	}

	for i := byte(0); i < 3; i++ {
		car.newKey(0x50 + i)
		time.Sleep(50 * time.Millisecond)
	}
	if err := cl.SetRegister(0x0a, []byte{0x01}); err != nil {
		t.Fatalf("SetRegister after new keys: %v", err)
	}
	if complaints, _, writes := car.counts(); complaints != 0 || len(writes) != 1 {
		t.Errorf("after new keys: complaints %d, writes %v", complaints, writes)
	}
}

// A car that rejects every resend makes the write fail quickly, instead of
// resending until the timeout.
func TestKeyResyncCapsResends(t *testing.T) {
	car := newKeyCar(t)
	cl, _ := connect(t, car, true)
	car.mu.Lock()
	car.rejectWrites = true
	car.nextBadKey = 0x60
	car.mu.Unlock()
	start := time.Now()
	err := cl.SetRegister(0x0a, []byte{0x01})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("SetRegister = %v after %v, want a quick failure", err, time.Since(start))
	}
	if complaints, _, _ := car.counts(); complaints != maxComplaintResends+1 {
		t.Errorf("complaints = %d, want the first send plus %d resends", complaints, maxComplaintResends)
	}
}
