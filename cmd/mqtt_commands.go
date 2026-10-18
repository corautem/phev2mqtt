package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/buxtronix/phev2mqtt/client"
	"github.com/buxtronix/phev2mqtt/protocol"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Extra commands (--extra_commands, or EXTRA_COMMANDS=true): buttons for
// Refresh, Flash Lights, Sync Car Clock, Charge Now and Resume Car Charge
// Timer, and a "Last Command Result" sensor.
//
// Climate guard (--climate_guard, or CLIMATE_GUARD=true): remote climate
// starts the way the official app does. On 2014-2017 cars the climate write
// echoes the car's own climate timer registers instead of overwriting them;
// a start is refused with a reason while the power switch is on or the car
// is quick charging (the car would accept it and do nothing); and the result
// is confirmed from the car's climate registers.
//
// Sync clock (--sync_clock, or SYNC_CLOCK=true): sets the car's clock from
// the gateway's once per session, as the official app does.
//
// Command layouts follow phev-remote-plus
// (https://github.com/Anwo91/phev-remote-plus, core/model/CarCommand.kt).

const (
	regRequestUpdate = 0x06 // [3]: send all registers
	regClockSync     = 0x05 // [yy mm dd hh mi ss dow 0]
	regHeadlights    = 0x0a // [1] on, [2] off
	regChargeTimer   = 0x17 // [1] charge now, [2] resume the car's timer

	regPreACState  = 0x10 // [0] == 2: climate running
	regClimateProg = 0x19 // the car's climate timer programme
	regIgnitionAC  = 0x1a // [0] power switch, [1] climate operating
	regClimateSets = 0x22 // MY14 climate timer presets
	regDoors       = 0x24 // [9] headlights on

	commandResultTopic = "/command/result"
)

var (
	// flashDuration is how long Flash Lights keeps the headlights on.
	flashDuration = 5 * time.Second
	// climateRegisterWait is how long a MY14 climate start waits for the
	// timer registers it echoes, after asking the car for them.
	climateRegisterWait = 5 * time.Second
	// climateConfirmWait is how long a climate start waits for the car to
	// report the climate running.
	climateConfirmWait = 30 * time.Second
	// clockSyncDelay lets a new session settle before the clock is set.
	clockSyncDelay = 3 * time.Second
)

// regCache holds the latest value of each register in the current session.
type regCache struct {
	mu   sync.Mutex
	data map[byte][]byte
}

func (r *regCache) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = map[byte][]byte{}
}

func (r *regCache) put(register byte, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data == nil {
		r.data = map[byte][]byte{}
	}
	r.data[register] = append([]byte(nil), data...)
}

func (r *regCache) get(register byte) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.data[register]
	return d, ok
}

// commandButtons maps each extra command's topic (after the prefix) to its
// entity.
var commandButtons = map[string]haEntity{
	"/set/refresh":             buttonEntity("refresh", "Refresh", "/set/refresh", "mdi:refresh"),
	"/set/flash_lights":        buttonEntity("flash_lights", "Flash Lights", "/set/flash_lights", "mdi:car-light-high"),
	"/set/sync_clock":          buttonEntity("sync_clock", "Sync Car Clock", "/set/sync_clock", "mdi:clock-check-outline"),
	"/set/charge_now":          buttonEntity("charge_now", "Charge Now", "/set/charge_now", "mdi:ev-station"),
	"/set/resume_charge_timer": buttonEntity("resume_charge_timer", "Resume Car Charge Timer", "/set/resume_charge_timer", "mdi:timer-play-outline"),
}

func buttonEntity(key, name, topic, icon string) haEntity {
	return haEntity{component: "button", key: key, name: name, fields: map[string]interface{}{
		"command_topic": "~" + topic, "payload_press": "PRESS", "icon": icon}}
}

// extraCommandEntities are published with --extra_commands.
var extraCommandEntities = append([]haEntity{
	sensorEntity("last_command_result", "Last Command Result", "~"+commandResultTopic, map[string]interface{}{
		"icon": "mdi:history", "entity_category": "diagnostic"}),
}, commandButtons["/set/refresh"], commandButtons["/set/flash_lights"], commandButtons["/set/sync_clock"],
	commandButtons["/set/charge_now"], commandButtons["/set/resume_charge_timer"])

// carWrite writes a register on the car; tests replace m.carWriteHook.
func (m *mqttClient) carWrite(register byte, data []byte) error {
	if m.carWriteHook != nil {
		return m.carWriteHook(register, data)
	}
	return m.phev.SetRegister(register, data)
}

func (m *mqttClient) reportCommand(text string) {
	text = time.Now().Format("2006-01-02 15:04") + " " + text
	log.Infof("Command: %s", text)
	m.publish(commandResultTopic, text)
}

// isCommandButton reports whether a topic is an extra command's button.
func (m *mqttClient) isCommandButton(topic string) bool {
	_, ok := commandButtons[strings.TrimPrefix(topic, m.prefix)]
	return ok && strings.HasPrefix(topic, m.prefix)
}

func (m *mqttClient) handleCommandButton(msg mqtt.Message) {
	if !m.extraCommands {
		log.Infof("Ignoring %s: extra commands are not switched on (EXTRA_COMMANDS=true)", msg.Topic())
		return
	}
	if msg.Retained() {
		log.Warnf("Ignoring retained command on %s", msg.Topic())
		return
	}
	go m.runCommandButton(strings.TrimPrefix(msg.Topic(), m.prefix))
}

// runCommandButton runs an extra command; commands run one at a time.
func (m *mqttClient) runCommandButton(topic string) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	name := commandButtons[topic].name
	if !m.carConnected {
		m.reportCommand(name + ": refused, the car is not connected")
		return
	}
	var err error
	done := name + ": done"
	switch topic {
	case "/set/refresh":
		err = m.carWrite(regRequestUpdate, []byte{0x03})
	case "/set/flash_lights":
		if d, ok := m.regs.get(regDoors); ok && len(d) >= 10 && d[9] == 1 {
			m.reportCommand(name + ": refused, the headlights are on")
			return
		}
		if err = m.carWrite(regHeadlights, []byte{0x01}); err == nil {
			time.Sleep(flashDuration)
			err = m.carWrite(regHeadlights, []byte{0x02})
		}
	case "/set/sync_clock":
		now := time.Now()
		err = m.carWrite(regClockSync, clockBytes(now))
		done = name + ": done, car clock set to " + now.Format("15:04")
	case "/set/charge_now":
		// No "plugged in" check: with a smart charger holding off, the car
		// reports unplugged with the cable in.
		err = m.carWrite(regChargeTimer, []byte{0x01})
	case "/set/resume_charge_timer":
		err = m.carWrite(regChargeTimer, []byte{0x02})
	}
	if err != nil {
		m.reportCommand(name + ": failed, " + err.Error())
		return
	}
	m.reportCommand(done)
}

// clockBytes encodes a time as the car's clock: [yy mm dd hh mi ss dow 0],
// with Sunday = 0.
func clockBytes(t time.Time) []byte {
	return []byte{byte(t.Year() - 2000), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), byte(t.Weekday()), 0}
}

// syncClockOnConnect sets the car's clock once a new session has settled.
func (m *mqttClient) syncClockOnConnect() {
	time.Sleep(clockSyncDelay)
	m.runCommandButton("/set/sync_clock")
}

func climateLabel(mode, duration byte) string {
	if mode == 0 {
		return "Climate off"
	}
	names := map[byte]string{1: "Cool", 2: "Heat", 3: "Windscreen"}
	return fmt.Sprintf("%s %d min", names[mode], 10*(int(duration)+1))
}

// climateRefusal is why the car would accept a climate start and do nothing.
func (m *mqttClient) climateRefusal() string {
	if d, ok := m.regs.get(regIgnitionAC); ok && len(d) >= 1 && (d[0] == 3 || d[0] == 4) {
		return "the car only runs the climate remotely with the power switch off"
	}
	quickD, _ := m.regs.get(protocol.QuickChargeStateRegister)
	quickC, _ := m.regs.get(protocol.QuickChargePhaseRegister)
	if (len(quickD) > 0 && quickD[0] == 3) || (len(quickC) > 0 && quickC[0] == 5) {
		return "the car doesn't run the climate while quick charging"
	}
	return ""
}

// my14ClimatePayload builds the MY14 climate register like the official app:
// the car's register 0x22 (6 bytes), the mode byte, then bytes 1..8 of
// register 0x19, which leaves the car's own climate timers as they are.
// Without those registers it falls back to phev2mqtt's original pattern.
func (m *mqttClient) my14ClimatePayload(modeByte byte) []byte {
	presets := func() ([]byte, []byte, bool) {
		r22, ok22 := m.regs.get(regClimateSets)
		r19, ok19 := m.regs.get(regClimateProg)
		return r22, r19, ok22 && ok19 && len(r22) >= 6 && len(r19) >= 9
	}
	r22, r19, ok := presets()
	if !ok {
		m.carWrite(regRequestUpdate, []byte{0x03})
		for deadline := time.Now().Add(climateRegisterWait); !ok && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			r22, r19, ok = presets()
		}
	}
	d := bytes.Repeat([]byte{0xff}, 15)
	if ok {
		copy(d[0:6], r22[:6])
		copy(d[7:15], r19[1:9])
	} else {
		log.Infof("Climate: the car's climate timer registers are unknown, sending the original pattern")
		d[0], d[1] = 0, 0
	}
	d[6] = modeByte
	return d
}

// climateRunning waits for the car to report the climate running.
func (m *mqttClient) climateRunning() bool {
	for deadline := time.Now().Add(climateConfirmWait); ; time.Sleep(200 * time.Millisecond) {
		if d, ok := m.regs.get(regPreACState); ok && len(d) >= 1 && d[0] == 2 {
			return true
		}
		if d, ok := m.regs.get(regIgnitionAC); ok && len(d) >= 2 && d[1] == 1 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// setClimateGuarded starts or stops the climate with the climate guard.
func (m *mqttClient) setClimateGuarded(mode, duration byte) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	what := climateLabel(mode, duration)
	if !m.carConnected {
		m.reportCommand(what + ": refused, the car is not connected")
		return
	}
	if mode != 0 {
		if reason := m.climateRefusal(); reason != "" {
			m.reportCommand(what + ": refused, " + reason)
			return
		}
	}
	var err error
	switch m.carModel {
	case client.ModelYear14:
		if mode == 0 {
			// The official app only flips the switch to stop.
			err = m.carWrite(protocol.SetACEnabledRegisterMY14, []byte{0x01})
		} else if err = m.carWrite(protocol.SetACModeRegisterMY14, m.my14ClimatePayload(mode|duration<<4)); err == nil {
			err = m.carWrite(protocol.SetACEnabledRegisterMY14, []byte{0x02})
		}
	case client.ModelYear18, client.ModelYear24:
		state := byte(0x02)
		if mode == 0 {
			state = 0x01
		}
		err = m.carWrite(protocol.SetACModeRegisterMY18, []byte{state, mode, duration, 0x0})
	default:
		m.reportCommand(what + ": refused, the car's model year isn't known yet")
		return
	}
	if err != nil {
		m.reportCommand(what + ": failed, " + err.Error())
		return
	}
	if mode == 0 {
		m.reportCommand(what + ": done")
		return
	}
	if !m.climateRunning() {
		m.reportCommand(what + ": the car accepted it but did not start the climate (a door open, or the battery low?)")
		return
	}
	m.reportCommand(what + ": done, the climate is running")
}

func init() {
	mqttCmd.Flags().Bool("extra_commands", false, "Buttons for refresh, flash lights, sync car clock, charge now and resume charge timer, and a last command result sensor")
	mqttCmd.Flags().Bool("climate_guard", false, "Start the climate like the official app: keep the car's climate timers, refuse when it can't run, confirm it started")
	mqttCmd.Flags().Bool("sync_clock", false, "Set the car's clock once per session")
	viper.BindPFlag("extra_commands", mqttCmd.Flags().Lookup("extra_commands"))
	viper.BindPFlag("climate_guard", mqttCmd.Flags().Lookup("climate_guard"))
	viper.BindPFlag("sync_clock", mqttCmd.Flags().Lookup("sync_clock"))
}
