package cmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxtronix/phev2mqtt/protocol"
	"github.com/spf13/viper"
)

func settingConfigTopic(component string, id int) string {
	return "homeassistant/" + component + "/" + testVIN + "_" + vehicleSettingKey(id) + "/config"
}

func mustBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// settingNotification is a 0x16 notification reporting one setting.
func settingNotification(id int, s protocol.VehicleSetting) []byte {
	a, b := protocol.EncodeSettingPair(id, s)
	return []byte{2, a, b, 0, 0, 0, 0, 0}
}

type fakeMessage struct {
	topic    string
	payload  string
	retained bool
}

func (f fakeMessage) Duplicate() bool   { return false }
func (f fakeMessage) Qos() byte         { return 0 }
func (f fakeMessage) Retained() bool    { return f.retained }
func (f fakeMessage) Topic() string     { return f.topic }
func (f fakeMessage) MessageID() uint16 { return 0 }
func (f fakeMessage) Payload() []byte   { return []byte(f.payload) }
func (f fakeMessage) Ack()              {}

func TestVehicleSettingsRead(t *testing.T) {
	m, f := newTestClient()
	m.vs.mode = vehicleSettingsRead

	// Settings before VIN/discovery: values only. id 7 = "Off", id 11 = "3 minutes".
	m.publishVehicleSettings(0x16, mustBytes(t, "02073e8bff000000"))
	got := f.reset()
	if got["phev/vehicle_setting/7"] != "Off" || got["phev/vehicle_setting/11"] != "3 minutes" || got["phev/vehicle_settings/status"] != "allowed" {
		t.Errorf("values = %v", got)
	}
	var attrs struct {
		Group     string
		SettingID int `json:"setting_id"`
		Options   []string
	}
	if err := json.Unmarshal([]byte(got["phev/vehicle_setting/7/attributes"]), &attrs); err != nil || attrs.Group != "Lights" || attrs.SettingID != 7 || len(attrs.Options) != 5 {
		t.Errorf("attributes = %s (%v)", got["phev/vehicle_setting/7/attributes"], err)
	}
	if !strings.Contains(got["phev/vehicle_settings/known"], `"7":{"mask":31,"value":0}`) {
		t.Errorf("known = %s", got["phev/vehicle_settings/known"])
	}
	for _, topic := range []string{"phev/vehicle_setting/7", "phev/vehicle_setting/7/attributes", "phev/vehicle_settings/status", "phev/vehicle_settings/known"} {
		if !f.retained(topic) {
			t.Errorf("%s not retained", topic)
		}
	}

	// Discovery announces sensors for the reported settings, removes any
	// write-mode selects and leaves settings not reported yet alone.
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	got = f.reset()
	if !strings.Contains(got[settingConfigTopic("sensor", 7)], "Lights: Charging Lid Light Auto cut-out") {
		t.Errorf("id 7 config = %q", got[settingConfigTopic("sensor", 7)])
	}
	if payload, ok := got[settingConfigTopic("select", 7)]; !ok || payload != "" {
		t.Errorf("read mode: id 7 select config %q (present %v), want removed", payload, ok)
	}
	if _, touched := got[settingConfigTopic("sensor", 13)]; touched {
		t.Error("id 13, not reported yet, was announced or removed")
	}
	if got["homeassistant/sensor/"+testVIN+"_"+vehicleSettingsLastChangeKey+"/config"] != "" {
		t.Error("read mode announced the last change sensor")
	}
	if got["homeassistant/switch/"+testVIN+"_"+vehicleSettingsAllowKey+"/config"] != "" {
		t.Error("read mode announced the Allow Changes switch")
	}

	// A setting reported later is announced as it arrives.
	m.publishVehicleSettings(0x16, mustBytes(t, "02473e0d1e000000"))
	got = f.reset()
	if got[settingConfigTopic("sensor", 13)] == "" || got["phev/vehicle_setting/13"] != "Off" || got["phev/vehicle_setting/7"] != "1 minute" {
		t.Errorf("after second notification: %v", got)
	}
	if _, ok := got[settingConfigTopic("sensor", 7)]; ok {
		t.Error("id 7 announced twice")
	}

	// The car refusing changes shows in the status entity.
	m.publishVehicleSettings(0x16, mustBytes(t, "01073e000000000000"))
	if got := f.reset()["phev/vehicle_settings/status"]; got != "refused" {
		t.Errorf("status = %q, want refused", got)
	}

	// Other registers are ignored.
	m.publishVehicleSettings(0x1d, mustBytes(t, "5a000000"))
	if got := f.reset(); len(got) != 0 {
		t.Errorf("register 0x1d published %v", got)
	}

	// Read mode refuses changes.
	m.carConnected = true
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "Off"})
	if got := f.reset()["phev/vehicle_settings/last_change"]; !strings.Contains(got, "refused, changes are not switched on") {
		t.Errorf("read mode change: %q", got)
	}
}

func TestVehicleSettingsOff(t *testing.T) {
	m, f := newTestClient()
	m.vs.mode = vehicleSettingsOff
	m.publishVehicleSettings(0x16, mustBytes(t, "02073e8bff000000"))
	if got := f.reset(); len(got) != 0 {
		t.Errorf("off mode published %v", got)
	}
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	got := f.reset()
	for _, topic := range []string{
		settingConfigTopic("sensor", 7), settingConfigTopic("select", 7),
		"phev/vehicle_setting/7", "phev/vehicle_setting/7/attributes",
		"phev/vehicle_settings/status", "phev/vehicle_settings/known", "phev/vehicle_settings/last_change",
		"phev/vehicle_settings/allow_changes",
	} {
		if payload, ok := got[topic]; !ok || payload != "" || !f.retained(topic) {
			t.Errorf("off mode: %s = %q (present %v), want an empty retained message", topic, payload, ok)
		}
	}
	for topic, payload := range got {
		if payload != "" {
			t.Errorf("off mode announced %s", topic)
		}
	}
}

// writeTestClient is in write mode with id 7 (mask 31, "Off") and id 11
// reported; its car reports a written setting unless silent is set.
func writeTestClient(t *testing.T) (*mqttClient, *fakeMQTT, *[]string, *bool) {
	t.Helper()
	viper.Set("state_dir", t.TempDir())
	t.Cleanup(func() { viper.Set("state_dir", nil) })
	m, f := newTestClient()
	m.vs.mode = vehicleSettingsWrite
	m.carConnected = true
	m.vs.confirmTimeout = 300 * time.Millisecond
	var mu sync.Mutex
	writes := &[]string{}
	silent := new(bool)
	m.vs.setRegister = func(register byte, data []byte) error {
		mu.Lock()
		*writes = append(*writes, hex.EncodeToString(append([]byte{register}, data...)))
		mu.Unlock()
		if register == protocol.SetVehicleSettingRegister && !*silent {
			m.vs.mu.Lock()
			s, _ := m.vs.settings.Get(int(data[0]))
			m.vs.mu.Unlock()
			s.Value = int(data[1])
			m.publishVehicleSettings(0x16, settingNotification(int(data[0]), s))
		}
		return nil
	}
	m.publishVehicleSettings(0x16, mustBytes(t, "02073e8bff000000"))
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	m.setAllowChanges(true)
	f.reset()
	return m, f, writes, silent
}

// waitLastChange waits for the "last change" text to contain want.
func waitLastChange(t *testing.T, f *fakeMQTT, want string) string {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		f.mu.Lock()
		got := f.pub["phev/vehicle_settings/last_change"]
		f.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("last change = %q, want it to contain %q", f.pub["phev/vehicle_settings/last_change"], want)
	return ""
}

func TestVehicleSettingsWriteDiscovery(t *testing.T) {
	m, f, _, _ := writeTestClient(t)
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	got := f.reset()
	var c struct {
		CommandTopic   string   `json:"command_topic"`
		Options        []string `json:"options"`
		EntityCategory string   `json:"entity_category"`
	}
	if err := json.Unmarshal([]byte(got[settingConfigTopic("select", 7)]), &c); err != nil {
		t.Fatalf("select config %q: %v", got[settingConfigTopic("select", 7)], err)
	}
	if c.CommandTopic != "~/set/vehicle_setting/7" || len(c.Options) != 5 || c.Options[0] != "Off" || c.EntityCategory != "config" {
		t.Errorf("select config = %+v", c)
	}
	if payload, ok := got[settingConfigTopic("sensor", 7)]; !ok || payload != "" {
		t.Errorf("write mode: read-only sensor %q (present %v), want removed", payload, ok)
	}
	if got["homeassistant/sensor/"+testVIN+"_"+vehicleSettingsLastChangeKey+"/config"] == "" {
		t.Error("last change sensor not announced")
	}
	if !strings.Contains(got["homeassistant/switch/"+testVIN+"_"+vehicleSettingsAllowKey+"/config"], `"command_topic":"~/set/vehicle_settings_allow_changes"`) {
		t.Errorf("Allow Changes switch config = %q", got["homeassistant/switch/"+testVIN+"_"+vehicleSettingsAllowKey+"/config"])
	}
	if got["phev/vehicle_settings/allow_changes"] != "ON" || !f.retained("phev/vehicle_settings/allow_changes") {
		t.Errorf("Allow Changes state = %q", got["phev/vehicle_settings/allow_changes"])
	}
}

func TestVehicleSettingsChange(t *testing.T) {
	m, f, writes, _ := writeTestClient(t)

	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "3 minutes"})
	got := waitLastChange(t, f, "done")
	if !strings.Contains(got, "Lights: Charging Lid Light Auto cut-out -> 3 minutes: done") {
		t.Errorf("last change = %q", got)
	}
	if want := "0f0702,0e00"; strings.Join(*writes, ",") != want {
		t.Errorf("writes = %v, want %s", *writes, want)
	}
	f.mu.Lock()
	value := f.pub["phev/vehicle_setting/7"]
	f.mu.Unlock()
	if value != "3 minutes" {
		t.Errorf("value after change = %q", value)
	}

	// The snapshot holds the settings as they were before the first change.
	files, _ := filepath.Glob(filepath.Join(viper.GetString("state_dir"), "vehicle-settings-*.json"))
	if len(files) != 1 {
		t.Fatalf("snapshots = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	var snap settingsSnapshot
	if err := json.Unmarshal(data, &snap); err != nil || len(snap.Settings) != 2 || snap.Settings[0].ID != 7 || snap.Settings[0].Label != "Off" {
		t.Errorf("snapshot = %s (%v)", data, err)
	}

	// An option number (from restore-settings) works too; still one snapshot.
	*writes = nil
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "0"})
	waitLastChange(t, f, "-> Off: done")
	if want := "0f0700,0e00"; strings.Join(*writes, ",") != want {
		t.Errorf("writes = %v, want %s", *writes, want)
	}
	if files, _ := filepath.Glob(filepath.Join(viper.GetString("state_dir"), "vehicle-settings-*.json")); len(files) != 1 {
		t.Errorf("second change took another snapshot: %v", files)
	}
}

func TestVehicleSettingsChangeUnconfirmed(t *testing.T) {
	m, f, writes, silent := writeTestClient(t)
	*silent = true
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, "did not confirm")
	// Saved anyway, like the official app; the select shows what the car has.
	if want := "0f0701,0e00"; strings.Join(*writes, ",") != want {
		t.Errorf("writes = %v, want %s", *writes, want)
	}
	f.mu.Lock()
	value := f.pub["phev/vehicle_setting/7"]
	f.mu.Unlock()
	if value != "Off" {
		t.Errorf("value = %q, want the unchanged Off", value)
	}
}

func TestVehicleSettingsChangeRefused(t *testing.T) {
	m, f, writes, _ := writeTestClient(t)
	refuse := func(name string, msg fakeMessage, want string) {
		t.Helper()
		f.reset()
		m.handleVehicleSettingCommand(msg)
		time.Sleep(50 * time.Millisecond)
		if got := f.reset()["phev/vehicle_settings/last_change"]; !strings.Contains(got, want) {
			t.Errorf("%s: last change %q, want %q", name, got, want)
		}
		if len(*writes) != 0 {
			t.Errorf("%s: wrote %v", name, *writes)
		}
	}
	refuse("same value", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "Off"}, "-> Off: already set, nothing to change")
	refuse("same value by number", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "0"}, "-> Off: already set")
	refuse("unknown label", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "2 hours"}, "not an option this car offers")
	refuse("option outside mask", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "6"}, "-> 6: refused, not an option this car offers")
	refuse("not reported", fakeMessage{topic: "phev/set/vehicle_setting/13", payload: "Off"}, "has not reported")
	refuse("theft alarm", fakeMessage{topic: "phev/set/vehicle_setting/23", payload: "0"}, "not a setting that can be changed")

	m.carConnected = false
	refuse("car away", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"}, "not connected")
	m.carConnected = true

	m.publishVehicleSettings(0x16, mustBytes(t, "01073e000000000000"))
	refuse("car refuses", fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"}, "does not accept changes")

	// Retained commands are ignored silently.
	f.reset()
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute", retained: true})
	time.Sleep(50 * time.Millisecond)
	if got := f.reset(); len(got) != 0 || len(*writes) != 0 {
		t.Errorf("retained command: published %v, wrote %v", got, *writes)
	}
}

func TestVehicleSettingsRemembered(t *testing.T) {
	// A first run reports settings; the known record goes to the broker.
	first, f1 := newTestClient()
	first.vs.mode = vehicleSettingsWrite
	first.publishVehicleSettings(0x16, mustBytes(t, "02073e8bff000000"))
	known := f1.reset()["phev/vehicle_settings/known"]

	// The next run reads it back: entities and changes work before the car
	// comes round to those settings again.
	m, f := newTestClient()
	m.vs.mode = vehicleSettingsWrite
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	f.reset()
	m.handleKnownVehicleSettings(nil, fakeMessage{topic: "phev/vehicle_settings/known", payload: known, retained: true})
	got := f.reset()
	if got[settingConfigTopic("select", 7)] == "" || got[settingConfigTopic("select", 11)] == "" {
		t.Errorf("remembered settings not announced: %v", got)
	}
	m.vs.mu.Lock()
	s, ok := m.vs.settings.Get(11)
	m.vs.mu.Unlock()
	if !ok || s != (protocol.VehicleSetting{Mask: 127, Value: 6}) {
		t.Errorf("id 11 = %+v %v", s, ok)
	}
	// Garbage is ignored.
	m.handleKnownVehicleSettings(nil, fakeMessage{payload: "not json"})
	m.handleKnownVehicleSettings(nil, fakeMessage{payload: ""})
}

func TestVehicleSettingsCleanup(t *testing.T) {
	for _, e := range []haEntity{
		vehicleSettingSensor(7), vehicleSettingSelect(7, protocol.VehicleSetting{Mask: 31}),
		vehicleSettingsStatusEntity, vehicleSettingsLastChangeEntity, vehicleSettingsAllowEntity,
	} {
		if !cleanupTarget(e.discoveryTopic("homeassistant", testVIN), []byte(e.config(testVIN, "phev", "Phev"))) {
			t.Errorf("cleanup misses %s %s", e.component, e.key)
		}
	}
}

func TestVehicleSettingsMode(t *testing.T) {
	defer viper.Set("vehicle_settings", nil)
	for in, want := range map[string]string{"": "off", "off": "off", "READ": "read", " read ": "read", "write": "write", "true": "off"} {
		viper.Set("vehicle_settings", in)
		if got := vehicleSettingsMode(); got != want {
			t.Errorf("vehicleSettingsMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRestoreSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vehicle-settings-test.json")
	os.WriteFile(path, []byte(`{"taken":"2026-10-15T10:00:00+01:00","settings":[
		{"id":32,"name":"Keyless entry & operation: Time for Remote Unlock Inactivation after Locking","value":1,"label":"3 seconds"},
		{"id":7,"name":"Lights: Charging Lid Light Auto cut-out","value":1,"label":"1 minute"}]}`), 0600)
	snap, err := loadSnapshot(path)
	if err != nil || len(snap.Settings) != 2 || snap.Settings[0].ID != 7 {
		t.Fatalf("loadSnapshot = %+v, %v", snap, err)
	}
	if topic, payload := restoreCommand("phev", snap.Settings[1]); topic != "phev/set/vehicle_setting/32" || payload != "1" {
		t.Errorf("restore command = %s %s", topic, payload)
	}
	os.WriteFile(path, []byte(`{"taken":"x","settings":[]}`), 0600)
	if _, err := loadSnapshot(path); err == nil {
		t.Error("empty snapshot accepted")
	}
	os.WriteFile(path, []byte(`nope`), 0600)
	if _, err := loadSnapshot(path); err == nil {
		t.Error("garbage accepted")
	}

	// A snapshot the gateway wrote reads back.
	m, f, _, _ := writeTestClient(t)
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, "done")
	files, _ := filepath.Glob(filepath.Join(viper.GetString("state_dir"), "vehicle-settings-*.json"))
	if len(files) != 1 {
		t.Fatalf("snapshots = %v", files)
	}
	if snap, err := loadSnapshot(files[0]); err != nil || snap.Settings[0].ID != 7 || snap.Settings[0].Value != 0 {
		t.Errorf("gateway snapshot = %+v, %v", snap, err)
	}
}

// A setting the car reports after the first change is added to the
// snapshot, with its old value, before it is changed.
func TestVehicleSettingsSnapshotGrows(t *testing.T) {
	m, f, _, _ := writeTestClient(t)
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "3 minutes"})
	waitLastChange(t, f, "-> 3 minutes: done")

	// id 13 arrives later: mask 15, "Off".
	m.publishVehicleSettings(0x16, settingNotification(13, protocol.VehicleSetting{Mask: 15, Value: 0}))
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/13", payload: "30 minutes"})
	waitLastChange(t, f, "-> 30 minutes: done")
	// A second change of id 7 doesn't overwrite its original value.
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, "-> 1 minute: done")

	files, _ := filepath.Glob(filepath.Join(viper.GetString("state_dir"), "vehicle-settings-*.json"))
	if len(files) != 1 {
		t.Fatalf("snapshots = %v", files)
	}
	snap, err := loadSnapshot(files[0])
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, s := range snap.Settings {
		got[s.ID] = s.Label
	}
	want := map[int]string{7: "Off", 11: "3 minutes", 13: "Off"}
	if len(got) != len(want) {
		t.Errorf("snapshot = %v, want %v", got, want)
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("snapshot id %d = %q, want %q", id, got[id], label)
		}
	}
}

// A change whose save failed because the car disconnected is saved when the
// car reconnects.
func TestVehicleSettingsSaveAfterReconnect(t *testing.T) {
	defer func(d time.Duration) { pendingSaveDelay = d }(pendingSaveDelay)
	pendingSaveDelay = 0
	m, f, writes, _ := writeTestClient(t)
	inner := m.vs.setRegister
	failSave := true
	m.vs.setRegister = func(register byte, data []byte) error {
		if register == protocol.SaveVehicleSettingsRegister && failSave {
			return fmt.Errorf("client closed")
		}
		return inner(register, data)
	}

	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, "sent but not saved (client closed), saving when the car reconnects")

	// Reconnected, still failing: stays pending.
	m.savePendingVehicleSettings()
	failSave = false
	m.savePendingVehicleSettings()
	waitLastChange(t, f, "saved the last change after the car reconnected")
	if want := "0f0701,0e00"; strings.Join(*writes, ",") != want {
		t.Errorf("writes = %v, want %s", *writes, want)
	}
	// Nothing pending any more.
	*writes = nil
	m.savePendingVehicleSettings()
	if len(*writes) != 0 {
		t.Errorf("saved again: %v", *writes)
	}
}

func allowState(f *fakeMQTT) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pub["phev/vehicle_settings/allow_changes"]
}

func TestVehicleSettingsAllowChanges(t *testing.T) {
	m, f, writes, _ := writeTestClient(t)
	allow := func(payload string, retained bool) {
		m.handleAllowChangesCommand(fakeMessage{topic: "phev/set/vehicle_settings_allow_changes", payload: payload, retained: retained})
	}

	// Locked: changes are refused.
	allow("OFF", false)
	if allowState(f) != "OFF" {
		t.Errorf("state after OFF = %q", allowState(f))
	}
	waitLastChange(t, f, "changes locked")
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, `refused, changes are locked, switch on "Allow Changes" first`)
	if len(*writes) != 0 {
		t.Errorf("locked: wrote %v", *writes)
	}

	// A retained ON (a replay) doesn't unlock.
	allow("ON", true)
	if allowState(f) != "OFF" {
		t.Error("retained ON unlocked changes")
	}

	// Unlocked: the change goes through.
	allow("ON", false)
	waitLastChange(t, f, "changes unlocked for 10 minutes")
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "1 minute"})
	waitLastChange(t, f, "-> 1 minute: done")

	// Changes lock again by themselves...
	m.vs.mu.Lock()
	m.vs.allowWindow = 300 * time.Millisecond
	m.vs.mu.Unlock()
	allow("ON", false)
	if allowState(f) != "ON" {
		t.Errorf("state after ON = %q", allowState(f))
	}
	// ...but a change restarts the window.
	time.Sleep(200 * time.Millisecond)
	m.handleVehicleSettingCommand(fakeMessage{topic: "phev/set/vehicle_setting/7", payload: "Off"})
	waitLastChange(t, f, "-> Off: done")
	time.Sleep(200 * time.Millisecond)
	if allowState(f) != "ON" {
		t.Error("a change didn't extend the window")
	}
	time.Sleep(400 * time.Millisecond)
	if allowState(f) != "OFF" {
		t.Error("changes didn't lock again by themselves")
	}
	waitLastChange(t, f, "changes locked")
}

func TestVehicleSettingsAllowChangesOnlyInWriteMode(t *testing.T) {
	m, f := newTestClient()
	m.vs.mode = vehicleSettingsRead
	m.setAllowChanges(true)
	if got := f.reset(); len(got) != 0 {
		t.Errorf("read mode published %v", got)
	}
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	if m.vs.allowChanges {
		t.Error("read mode unlocked changes")
	}
}
