package cmd

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxtronix/phev2mqtt/client"
	"github.com/buxtronix/phev2mqtt/protocol"
)

func TestClimateModeState(t *testing.T) {
	c := new(climate)
	if got := c.mqttStates()["/climate/mode"]; got != "off" {
		t.Errorf("no data: /climate/mode = %q, want off", got)
	}
	c.setMode("heat")
	c.setState(protocol.PreACOn)
	if got := c.mqttStates()["/climate/mode"]; got != "heat" {
		t.Errorf("heating: /climate/mode = %q, want heat", got)
	}
	c.setState(protocol.PreACTerminated)
	if got := c.mqttStates()["/climate/mode"]; got != "off" {
		t.Errorf("terminated: /climate/mode = %q, want off", got)
	}
}

// commandTestClient is connected to a MY14 car that records writes; its
// hook can react to a write like the car would.
func commandTestClient(t *testing.T, react func(m *mqttClient, register byte, data []byte)) (*mqttClient, *fakeMQTT, func() []string) {
	t.Helper()
	m, f := newTestClient()
	m.extraCommands = true
	m.carConnected = true
	m.carModel = client.ModelYear14
	var mu sync.Mutex
	var writes []string
	m.carWriteHook = func(register byte, data []byte) error {
		mu.Lock()
		writes = append(writes, hex.EncodeToString(append([]byte{register}, data...)))
		mu.Unlock()
		if react != nil {
			react(m, register, data)
		}
		return nil
	}
	return m, f, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), writes...)
	}
}

func waitResult(t *testing.T, f *fakeMQTT, want string) string {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		got := f.pub["phev/command/result"]
		f.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("command result = %q, want it to contain %q", f.pub["phev/command/result"], want)
	return ""
}

func press(m *mqttClient, topic string) {
	m.handleCommandButton(fakeMessage{topic: "phev" + topic, payload: "PRESS"})
}

func TestCommandButtons(t *testing.T) {
	defer func(d time.Duration) { flashDuration = d }(flashDuration)
	flashDuration = 20 * time.Millisecond

	tests := []struct {
		topic, result string
		writes        []string
	}{
		{"/set/refresh", "Refresh: done", []string{"0603"}},
		{"/set/flash_lights", "Flash Lights: done", []string{"0a01", "0a02"}},
		{"/set/charge_now", "Charge Now: done", []string{"1701"}},
		{"/set/resume_charge_timer", "Resume Car Charge Timer: done", []string{"1702"}},
	}
	for _, test := range tests {
		m, f, writes := commandTestClient(t, nil)
		press(m, test.topic)
		waitResult(t, f, test.result)
		if got := strings.Join(writes(), ","); got != strings.Join(test.writes, ",") {
			t.Errorf("%s: writes %s, want %v", test.topic, got, test.writes)
		}
	}
}

func TestSyncClock(t *testing.T) {
	m, f, writes := commandTestClient(t, nil)
	press(m, "/set/sync_clock")
	waitResult(t, f, "Sync Car Clock: done, car clock set to")
	w := writes()
	if len(w) != 1 || len(w[0]) != 18 || !strings.HasPrefix(w[0], "05") {
		t.Fatalf("writes = %v", w)
	}
	b, _ := hex.DecodeString(w[0][2:])
	now := time.Now()
	if int(b[0]) != now.Year()-2000 || time.Month(b[1]) != now.Month() || time.Weekday(b[6]) != now.Weekday() || b[7] != 0 {
		t.Errorf("clock bytes = % x for %v", b, now)
	}
}

func TestCommandButtonsRefused(t *testing.T) {
	// Flash lights with the headlights on would leave them off.
	m, f, writes := commandTestClient(t, nil)
	m.regs.put(regDoors, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	press(m, "/set/flash_lights")
	waitResult(t, f, "Flash Lights: refused, the headlights are on")

	m.carConnected = false
	press(m, "/set/refresh")
	waitResult(t, f, "Refresh: refused, the car is not connected")
	if len(writes()) != 0 {
		t.Errorf("refused commands wrote %v", writes())
	}

	// Switched off, or retained: nothing happens.
	m, f, writes = commandTestClient(t, nil)
	m.extraCommands = false
	press(m, "/set/refresh")
	m.extraCommands = true
	m.handleCommandButton(fakeMessage{topic: "phev/set/refresh", payload: "PRESS", retained: true})
	time.Sleep(100 * time.Millisecond)
	if len(writes()) != 0 || f.reset()["phev/command/result"] != "" {
		t.Errorf("ignored commands wrote %v", writes())
	}
}

func TestResumeChargeTimerNeedsAProgramme(t *testing.T) {
	m, f, writes := commandTestClient(t, nil)
	m.regs.put(protocol.ChargeTimerProgRegister, mustBytes(t, "ffffffff07ffff1fffffffff07ffff1f"))
	press(m, "/set/resume_charge_timer")
	waitResult(t, f, "Resume Car Charge Timer: refused, the car has no charge timer programmed")
	if len(writes()) != 0 {
		t.Errorf("wrote %v", writes())
	}
	// Monday 22:30-06:00 programmed: allowed.
	m.regs.put(protocol.ChargeTimerProgRegister, mustBytes(t, "dffeffff07dfff1fdffcffff07c7ff1f"))
	press(m, "/set/resume_charge_timer")
	waitResult(t, f, "Resume Car Charge Timer: done")
	if got := strings.Join(writes(), ","); got != "1702" {
		t.Errorf("writes %s, want 1702", got)
	}
}

func TestIsCommandButton(t *testing.T) {
	m, _ := newTestClient()
	for topic, want := range map[string]bool{
		"phev/set/refresh":      true,
		"phev/set/charge_now":   true,
		"phev/set/register/06":  false,
		"phev/set/climate/heat": false,
		"other/set/refresh":     false,
	} {
		if got := m.isCommandButton(topic); got != want {
			t.Errorf("isCommandButton(%s) = %v", topic, got)
		}
	}
}

// startsClimate is a car that reports the climate running once switched on.
func startsClimate(m *mqttClient, register byte, data []byte) {
	if register == protocol.SetACEnabledRegisterMY14 && data[0] == 2 {
		m.regs.put(regPreACState, []byte{2})
	}
	if register == protocol.SetACModeRegisterMY18 && data[0] == 2 {
		m.regs.put(regIgnitionAC, []byte{0, 1, 0, 0, 0})
	}
}

func TestClimateGuardKeepsCarTimers(t *testing.T) {
	m, f, writes := commandTestClient(t, startsClimate)
	m.regs.put(regIgnitionAC, []byte{0, 0})
	m.regs.put(regClimateSets, mustBytes(t, "000102030405"))
	m.regs.put(regClimateProg, mustBytes(t, "011122334455667788"))
	m.setClimateGuarded(2, 1) // heat, 20 min
	waitResult(t, f, "Heat 20 min: done, the climate is running")
	want := "02" + "000102030405" + "12" + "1122334455667788" + "," + "0402"
	if got := strings.Join(writes(), ","); got != want {
		t.Errorf("writes %s, want %s", got, want)
	}
}

func TestClimateGuardFallbackAndUnconfirmed(t *testing.T) {
	defer func(a, b time.Duration) { climateRegisterWait, climateConfirmWait = a, b }(climateRegisterWait, climateConfirmWait)
	climateRegisterWait, climateConfirmWait = 100*time.Millisecond, 100*time.Millisecond
	m, f, writes := commandTestClient(t, nil)
	m.setClimateGuarded(3, 0) // windscreen, 10 min; the car never reports it running
	waitResult(t, f, "Windscreen 10 min: the car accepted it but did not start the climate")
	// No timer registers known: ask for them, then the original pattern.
	want := "0603," + "02" + "0000ffffffff" + "03" + "ffffffffffffffff" + "," + "0402"
	if got := strings.Join(writes(), ","); got != want {
		t.Errorf("writes %s, want %s", got, want)
	}
}

func TestClimateGuardRefusals(t *testing.T) {
	refused := func(name string, prepare func(m *mqttClient), want string) {
		t.Helper()
		m, f, writes := commandTestClient(t, nil)
		prepare(m)
		m.setClimateGuarded(2, 0)
		waitResult(t, f, want)
		if len(writes()) != 0 {
			t.Errorf("%s: wrote %v", name, writes())
		}
	}
	refused("power on", func(m *mqttClient) { m.regs.put(regIgnitionAC, []byte{4, 0}) }, "Heat 10 min: refused, the car only runs the climate remotely with the power switch off")
	refused("acc", func(m *mqttClient) { m.regs.put(regIgnitionAC, []byte{3, 0}) }, "power switch off")
	refused("quick charging", func(m *mqttClient) { m.regs.put(protocol.QuickChargeStateRegister, []byte{3}) }, "while quick charging")
	refused("model year unknown", func(m *mqttClient) { m.carModel = client.ModelYearUnknown }, "model year isn't known yet")
	refused("car away", func(m *mqttClient) { m.carConnected = false }, "not connected")
}

func TestClimateGuardOff(t *testing.T) {
	// Switching off is always allowed and only flips the switch.
	m, f, writes := commandTestClient(t, nil)
	m.regs.put(regIgnitionAC, []byte{4, 0})
	m.setClimateGuarded(0, 0)
	waitResult(t, f, "Climate off: done")
	if got := strings.Join(writes(), ","); got != "0401" {
		t.Errorf("writes %s, want 0401", got)
	}
}

func TestClimateGuardMY18(t *testing.T) {
	m, f, writes := commandTestClient(t, startsClimate)
	m.carModel = client.ModelYear18
	m.setClimateGuarded(1, 2) // cool, 30 min
	waitResult(t, f, "Cool 30 min: done, the climate is running")
	if got := strings.Join(writes(), ","); got != "1b02010200" {
		t.Errorf("writes %s, want 1b02010200", got)
	}
}

func TestClimateGuardFromMQTT(t *testing.T) {
	m, f, writes := commandTestClient(t, startsClimate)
	m.climateGuard = true
	m.regs.put(regClimateSets, mustBytes(t, "000102030405"))
	m.regs.put(regClimateProg, mustBytes(t, "011122334455667788"))
	m.handleIncomingMqtt(nil, fakeMessage{topic: "phev/set/climate/heat", payload: "on"})
	waitResult(t, f, "Heat 10 min: done")
	if w := writes(); len(w) != 2 || !strings.HasPrefix(w[0], "02000102030405") {
		t.Errorf("writes %v", w)
	}
}
