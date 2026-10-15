package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buxtronix/phev2mqtt/protocol"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Vehicle settings (--vehicle_settings, or VEHICLE_SETTINGS): the car's own
// customisable functions from register 0x16, decoded by
// protocol.VehicleSettings.
//
//   - "off" (default) publishes nothing and removes the entities.
//   - "read" shows every setting the car offers as a read-only sensor.
//   - "write" shows them as selects that change the setting on the car.
//
// The car sends its settings a few at a time, carrying on through the list
// over many minutes rather than all at session start. So an entity, once
// announced, is never removed while the feature is on, setting values are
// retained on the broker, and the settings known so far are kept retained in
// <prefix>/vehicle_settings/known, which seeds the next run.
//
// Before the first change of a run, every known setting is saved to a
// snapshot file in --state_dir, and a setting the car reports only later is
// added before its first change; "client restore-settings" puts them back.

const (
	vehicleSettingsOff   = "off"
	vehicleSettingsRead  = "read"
	vehicleSettingsWrite = "write"
)

func vehicleSettingsMode() string {
	switch mode := strings.ToLower(strings.TrimSpace(viper.GetString("vehicle_settings"))); mode {
	case "", vehicleSettingsOff:
		return vehicleSettingsOff
	case vehicleSettingsRead, vehicleSettingsWrite:
		return mode
	default:
		log.Warnf("Unknown vehicle_settings %q (use off, read or write), vehicle settings stay off", mode)
		return vehicleSettingsOff
	}
}

// discoveryTarget is what Home Assistant discovery was last published with.
type discoveryTarget struct{ vin, topic, name string }

// vehicleSettingsState is used by the car loop (notifications) and by the
// MQTT callbacks (discovery, remembered settings, change commands).
type vehicleSettingsState struct {
	mu       sync.Mutex
	mode     string
	settings protocol.VehicleSettings
	target   *discoveryTarget
	// announced holds the mask each announced entity was built with.
	announced map[int]int
	// snapshot is this run's snapshot file, holding snapshotData: every
	// setting changed in this run, with its value from before the change.
	snapshot     string
	snapshotData settingsSnapshot
	snapshotIDs  map[int]bool
	// pendingSave is set when a change reached the car but the save didn't,
	// because the connection dropped; the save is sent on reconnect.
	pendingSave bool

	// changeMu makes changes run one at a time.
	changeMu sync.Mutex
	// setRegister writes a register on the car; tests replace it.
	setRegister func(register byte, data []byte) error
	// confirmTimeout is how long a change waits for the car to report it.
	confirmTimeout time.Duration
}

const (
	vehicleSettingsStatusKey     = "vehicle_settings_status"
	vehicleSettingsLastChangeKey = "vehicle_settings_last_change"

	vehicleSettingsStatusTopic     = "/vehicle_settings/status"
	vehicleSettingsLastChangeTopic = "/vehicle_settings/last_change"
	vehicleSettingsKnownTopic      = "/vehicle_settings/known"
	vehicleSettingCommandPrefix    = "/set/vehicle_setting/"

	defaultConfirmTimeout = 8 * time.Second
)

func vehicleSettingKey(id int) string { return fmt.Sprintf("vehicle_setting_%d", id) }

// vehicleSettingStateTopics are the state topics of a setting's entity,
// relative to the topic prefix.
func vehicleSettingStateTopics(id int) (value, attributes string) {
	return fmt.Sprintf("/vehicle_setting/%d", id), fmt.Sprintf("/vehicle_setting/%d/attributes", id)
}

func vehicleSettingName(id int) string {
	d, _ := protocol.SettingDefinition(id)
	return d.Group + ": " + d.Name
}

func vehicleSettingLabels(id int, s protocol.VehicleSetting) []string {
	var labels []string
	for _, o := range protocol.SettingOptions(id, s) {
		labels = append(labels, protocol.SettingOptionLabel(id, o, &s))
	}
	return labels
}

// vehicleSettingSensor is a setting's entity in read mode.
func vehicleSettingSensor(id int) haEntity {
	value, attributes := vehicleSettingStateTopics(id)
	return sensorEntity(vehicleSettingKey(id), vehicleSettingName(id), "~"+value, map[string]interface{}{
		"icon": "mdi:car-cog", "entity_category": "diagnostic", "json_attributes_topic": "~" + attributes})
}

// vehicleSettingSelect is a setting's entity in write mode.
func vehicleSettingSelect(id int, s protocol.VehicleSetting) haEntity {
	value, attributes := vehicleSettingStateTopics(id)
	return haEntity{component: "select", key: vehicleSettingKey(id), name: vehicleSettingName(id), fields: map[string]interface{}{
		"state_topic": "~" + value, "command_topic": "~" + vehicleSettingCommandPrefix + strconv.Itoa(id),
		"options": vehicleSettingLabels(id, s), "icon": "mdi:car-cog", "entity_category": "config",
		"json_attributes_topic": "~" + attributes}}
}

var vehicleSettingsStatusEntity = sensorEntity(vehicleSettingsStatusKey, "Vehicle Settings Changes", "~"+vehicleSettingsStatusTopic, map[string]interface{}{
	"device_class": "enum", "options": []string{"allowed", "refused"}, "icon": "mdi:car-cog", "entity_category": "diagnostic"})

var vehicleSettingsLastChangeEntity = sensorEntity(vehicleSettingsLastChangeKey, "Vehicle Settings Last Change", "~"+vehicleSettingsLastChangeTopic, map[string]interface{}{
	"icon": "mdi:history", "entity_category": "diagnostic"})

// vehicleSettingsKeys are the keys of every entity vehicle settings can add.
func vehicleSettingsKeys() []string {
	keys := []string{vehicleSettingsStatusKey, vehicleSettingsLastChangeKey}
	for _, id := range protocol.SettingIDs() {
		keys = append(keys, vehicleSettingKey(id))
	}
	return keys
}

// announceSettingLocked announces a setting's entity for the current mode.
func (m *mqttClient) announceSettingLocked(id int, s protocol.VehicleSetting) {
	t := m.vs.target
	if t == nil {
		return
	}
	if m.vs.mode == vehicleSettingsWrite {
		m.announce(vehicleSettingSelect(id, s), t.vin, t.topic, t.name, true)
	} else {
		m.announce(vehicleSettingSensor(id), t.vin, t.topic, t.name, true)
	}
	m.vs.announced[id] = s.Mask
}

// publishVehicleSettingsDiscovery announces an entity for every setting known
// so far, sensors in read mode and selects in write mode, and removes the
// other mode's entities. Settings reported later are announced as they
// arrive; entities announced before stay. When the feature is off, every
// vehicle settings entity and retained value is removed.
func (m *mqttClient) publishVehicleSettingsDiscovery(vin, topic, name string) {
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	m.vs.target = &discoveryTarget{vin, topic, name}
	m.vs.announced = map[int]int{}
	mode := m.vs.mode

	m.announce(vehicleSettingsStatusEntity, vin, topic, name, mode != vehicleSettingsOff)
	m.announce(vehicleSettingsLastChangeEntity, vin, topic, name, mode == vehicleSettingsWrite)
	for _, id := range protocol.SettingIDs() {
		if mode != vehicleSettingsRead {
			m.announce(vehicleSettingSensor(id), vin, topic, name, false)
		}
		if mode != vehicleSettingsWrite {
			m.announce(haEntity{component: "select", key: vehicleSettingKey(id)}, vin, topic, name, false)
		}
	}
	if mode == vehicleSettingsOff {
		for _, t := range []string{vehicleSettingsStatusTopic, vehicleSettingsLastChangeTopic, vehicleSettingsKnownTopic} {
			m.client.Publish(m.topic(t), 0, true, "")
		}
		for _, id := range protocol.SettingIDs() {
			value, attributes := vehicleSettingStateTopics(id)
			m.client.Publish(m.topic(value), 0, true, "")
			m.client.Publish(m.topic(attributes), 0, true, "")
		}
		return
	}
	for _, id := range protocol.SettingIDs() {
		if s, ok := m.vs.settings.Get(id); ok && protocol.SettingAvailable(id, s) {
			m.announceSettingLocked(id, s)
		}
	}
}

// settingValuesLocked returns the retained state messages for a setting.
func settingValuesLocked(id int, s protocol.VehicleSetting) map[string]string {
	d, _ := protocol.SettingDefinition(id)
	attrs, _ := json.Marshal(map[string]interface{}{"group": d.Group, "setting_id": id, "options": vehicleSettingLabels(id, s)})
	value, attributes := vehicleSettingStateTopics(id)
	return map[string]string{value: protocol.SettingOptionLabel(id, s.Value, &s), attributes: string(attrs)}
}

// knownSettingsLocked is the retained record of every setting known so far.
func (m *mqttClient) knownSettingsLocked() string {
	known := map[string]map[string]int{}
	for id, s := range m.vs.settings.All() {
		known[strconv.Itoa(id)] = map[string]int{"mask": s.Mask, "value": s.Value}
	}
	b, _ := json.Marshal(known)
	return string(b)
}

// publishVehicleSettings folds a settings notification in and publishes what
// changed, retained, announcing settings Home Assistant doesn't know yet.
func (m *mqttClient) publishVehicleSettings(register byte, data []byte) {
	if register != protocol.SettingsRegister || m.vs.mode == vehicleSettingsOff {
		return
	}
	values := map[string]string{}

	m.vs.mu.Lock()
	changed := m.vs.settings.Update(data)
	for _, id := range changed {
		s, _ := m.vs.settings.Get(id)
		if !protocol.SettingAvailable(id, s) {
			continue
		}
		if mask, ok := m.vs.announced[id]; !ok || mask != s.Mask {
			m.announceSettingLocked(id, s)
		}
		for t, p := range settingValuesLocked(id, s) {
			values[t] = p
		}
	}
	if len(changed) > 0 {
		values[vehicleSettingsKnownTopic] = m.knownSettingsLocked()
	}
	if m.vs.settings.Known() {
		status := "allowed"
		if !protocol.SettingsStatusAllowsChanges(m.vs.settings.Status) {
			status = "refused"
		}
		values[vehicleSettingsStatusTopic] = status
	}
	m.vs.mu.Unlock()

	// Retained: a setting may not come round again for many minutes.
	for t, p := range values {
		m.client.Publish(m.topic(t), 0, true, p)
	}
}

// subscribeVehicleSettings reads back the settings remembered by earlier
// runs, so they are usable before the car comes round to them again.
func (m *mqttClient) subscribeVehicleSettings(c mqtt.Client) {
	if m.vs.mode == vehicleSettingsOff {
		return
	}
	if token := c.Subscribe(m.topic(vehicleSettingsKnownTopic), 0, m.handleKnownVehicleSettings); token.Wait() && token.Error() != nil {
		log.Errorf("Error subscribing to %s: %v", m.topic(vehicleSettingsKnownTopic), token.Error())
	}
}

func (m *mqttClient) handleKnownVehicleSettings(_ mqtt.Client, msg mqtt.Message) {
	var known map[string]struct{ Mask, Value int }
	if len(msg.Payload()) == 0 || json.Unmarshal(msg.Payload(), &known) != nil {
		return
	}
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	seeded := 0
	for key, k := range known {
		id, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		s := protocol.VehicleSetting{Mask: k.Mask, Value: k.Value}
		if m.vs.settings.Seed(id, s) {
			seeded++
			if _, ok := m.vs.announced[id]; !ok && protocol.SettingAvailable(id, s) {
				m.announceSettingLocked(id, s)
			}
		}
	}
	if seeded > 0 {
		log.Infof("Vehicle settings: %d remembered from earlier runs", seeded)
	}
}

// handleVehicleSettingCommand changes a setting from Home Assistant (the
// payload is an option label) or from restore-settings (an option number).
func (m *mqttClient) handleVehicleSettingCommand(msg mqtt.Message) {
	id, err := strconv.Atoi(strings.TrimPrefix(msg.Topic(), m.topic(vehicleSettingCommandPrefix)))
	if err != nil {
		log.Errorf("Bad vehicle setting topic %s", msg.Topic())
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	// Retained commands would replay on every reconnect.
	if msg.Retained() {
		log.Warnf("Ignoring retained vehicle setting command on %s", msg.Topic())
		return
	}
	value, reason := m.vehicleSettingTarget(id, payload)
	switch reason {
	case "":
		go m.changeVehicleSetting(id, value)
	case alreadySet:
		m.reportVehicleSettingChange(fmt.Sprintf("%s -> %s: already set, nothing to change", vehicleSettingName(id), m.optionDescription(id, payload)))
	default:
		m.reportVehicleSettingChange(fmt.Sprintf("%s -> %s: refused, %s", vehicleSettingName(id), m.optionDescription(id, payload), reason))
		m.publishVehicleSettingValue(id)
	}
}

const alreadySet = "already set"

// optionDescription names the option a command asks for: restore-settings
// sends option numbers, which read better as their labels.
func (m *mqttClient) optionDescription(id int, payload string) string {
	n, err := strconv.Atoi(payload)
	if err != nil {
		return payload
	}
	m.vs.mu.Lock()
	s, ok := m.vs.settings.Get(id)
	m.vs.mu.Unlock()
	if !ok || !offersOption(id, s, n) {
		return payload
	}
	return protocol.SettingOptionLabel(id, n, &s)
}

// vehicleSettingTarget checks a change and returns the option to set, or why
// it can't be made.
func (m *mqttClient) vehicleSettingTarget(id int, payload string) (int, string) {
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	if m.vs.mode != vehicleSettingsWrite {
		return 0, "changes are not switched on (VEHICLE_SETTINGS=write)"
	}
	if _, ok := protocol.SettingDefinition(id); !ok {
		return 0, "not a setting that can be changed"
	}
	s, ok := m.vs.settings.Get(id)
	if !ok || !protocol.SettingAvailable(id, s) {
		return 0, "the car has not reported this setting"
	}
	if m.vs.settings.Known() && !protocol.SettingsStatusAllowsChanges(m.vs.settings.Status) {
		return 0, "the car does not accept changes right now"
	}
	if !m.carConnected {
		return 0, "the car is not connected"
	}
	value, ok := protocol.OptionByLabel(id, s, payload)
	if !ok {
		n, err := strconv.Atoi(payload)
		if err != nil || !offersOption(id, s, n) {
			return 0, "not an option this car offers"
		}
		value = n
	}
	if value == s.Value {
		return 0, alreadySet
	}
	return value, ""
}

func offersOption(id int, s protocol.VehicleSetting, option int) bool {
	for _, o := range protocol.SettingOptions(id, s) {
		if o == option {
			return true
		}
	}
	return false
}

// changeVehicleSetting writes a setting the way the official app does: write
// [id, value] to 0x0f, wait for the car to report the new value, then save
// with 0x0e [0], which is sent even when the car didn't confirm.
func (m *mqttClient) changeVehicleSetting(id, value int) {
	m.vs.changeMu.Lock()
	defer m.vs.changeMu.Unlock()
	defer m.publishVehicleSettingValue(id)

	m.vs.mu.Lock()
	s, _ := m.vs.settings.Get(id)
	m.vs.mu.Unlock()
	change := fmt.Sprintf("%s -> %s", vehicleSettingName(id), protocol.SettingOptionLabel(id, value, &s))

	if err := m.snapshotVehicleSettings(id); err != nil {
		m.reportVehicleSettingChange(change + ": not changed, the snapshot could not be saved: " + err.Error())
		return
	}
	log.Infof("Vehicle settings: changing %s", change)
	if err := m.writeVehicleRegister(protocol.SetVehicleSettingRegister, []byte{byte(id), byte(value)}); err != nil {
		m.reportVehicleSettingChange(change + ": failed, " + err.Error())
		return
	}
	confirmed := m.waitVehicleSetting(id, value)
	if err := m.writeVehicleRegister(protocol.SaveVehicleSettingsRegister, []byte{0}); err != nil {
		m.vs.mu.Lock()
		m.vs.pendingSave = true
		m.vs.mu.Unlock()
		m.reportVehicleSettingChange(change + ": sent but not saved (" + err.Error() + "), saving when the car reconnects")
		return
	}
	if !confirmed {
		m.reportVehicleSettingChange(change + ": the car did not confirm it, check the setting in the car")
		return
	}
	m.reportVehicleSettingChange(change + ": done")
}

// pendingSaveDelay lets a new session with the car settle before a save.
var pendingSaveDelay = 5 * time.Second

// savePendingVehicleSettings sends the save that a change couldn't send
// because the car disconnected. It runs when a session with the car starts;
// a save on its own only stores the car's current settings.
func (m *mqttClient) savePendingVehicleSettings() {
	m.vs.mu.Lock()
	pending := m.vs.pendingSave
	m.vs.mu.Unlock()
	if !pending {
		return
	}
	time.Sleep(pendingSaveDelay)
	m.vs.changeMu.Lock()
	defer m.vs.changeMu.Unlock()
	if err := m.writeVehicleRegister(protocol.SaveVehicleSettingsRegister, []byte{0}); err != nil {
		log.Warnf("Vehicle settings: saving after reconnect failed, trying again next time: %v", err)
		return
	}
	m.vs.mu.Lock()
	m.vs.pendingSave = false
	m.vs.mu.Unlock()
	m.reportVehicleSettingChange("saved the last change after the car reconnected")
}

func (m *mqttClient) writeVehicleRegister(register byte, data []byte) error {
	if m.vs.setRegister != nil {
		return m.vs.setRegister(register, data)
	}
	return m.phev.SetRegister(register, data)
}

// waitVehicleSetting waits for the car to report the setting at value.
func (m *mqttClient) waitVehicleSetting(id, value int) bool {
	timeout := m.vs.confirmTimeout
	if timeout == 0 {
		timeout = defaultConfirmTimeout
	}
	for deadline := time.Now().Add(timeout); ; time.Sleep(100 * time.Millisecond) {
		m.vs.mu.Lock()
		s, _ := m.vs.settings.Get(id)
		m.vs.mu.Unlock()
		if s.Value == value {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// publishVehicleSettingValue publishes a setting's current value again, so a
// select in Home Assistant shows what the car has after a change attempt.
func (m *mqttClient) publishVehicleSettingValue(id int) {
	m.vs.mu.Lock()
	s, ok := m.vs.settings.Get(id)
	var values map[string]string
	if ok && protocol.SettingAvailable(id, s) {
		values = settingValuesLocked(id, s)
	}
	m.vs.mu.Unlock()
	for t, p := range values {
		m.client.Publish(m.topic(t), 0, true, p)
	}
}

func (m *mqttClient) reportVehicleSettingChange(text string) {
	text = time.Now().Format("2006-01-02 15:04") + " " + text
	if len(text) > 250 {
		text = text[:250]
	}
	log.Infof("Vehicle settings: %s", text)
	m.client.Publish(m.topic(vehicleSettingsLastChangeTopic), 0, true, text)
}

// settingsSnapshot is the file format of a vehicle settings snapshot.
type settingsSnapshot struct {
	Taken    string            `json:"taken"`
	Settings []snapshotSetting `json:"settings"`
}

type snapshotSetting struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Value int    `json:"value"`
	Label string `json:"label"`
}

func snapshotEntry(id int, s protocol.VehicleSetting) snapshotSetting {
	return snapshotSetting{ID: id, Name: vehicleSettingName(id), Value: s.Value, Label: protocol.SettingOptionLabel(id, s.Value, &s)}
}

// snapshotVehicleSettings makes sure this run's snapshot holds the setting
// about to change, with its value from before any change. The first change
// of a run saves every known setting; a setting the car reports only later
// is added the first time it is changed. Changes run one at a time, so this
// never races with itself.
func (m *mqttClient) snapshotVehicleSettings(id int) error {
	m.vs.mu.Lock()
	if m.vs.snapshot != "" && m.vs.snapshotIDs[id] {
		m.vs.mu.Unlock()
		return nil
	}
	snap := m.vs.snapshotData
	ids := map[int]bool{}
	for k := range m.vs.snapshotIDs {
		ids[k] = true
	}
	if m.vs.snapshot == "" {
		snap = settingsSnapshot{Taken: time.Now().Format(time.RFC3339)}
		for sid, s := range m.vs.settings.All() {
			if protocol.SettingAvailable(sid, s) {
				snap.Settings = append(snap.Settings, snapshotEntry(sid, s))
				ids[sid] = true
			}
		}
	}
	if !ids[id] {
		s, _ := m.vs.settings.Get(id)
		snap.Settings = append(append([]snapshotSetting(nil), snap.Settings...), snapshotEntry(id, s))
		ids[id] = true
	}
	path := m.vs.snapshot
	m.vs.mu.Unlock()
	sort.Slice(snap.Settings, func(i, j int) bool { return snap.Settings[i].ID < snap.Settings[j].ID })

	if path == "" {
		dir := viper.GetString("state_dir")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		path = filepath.Join(dir, "vehicle-settings-"+time.Now().Format("20060102-150405")+".json")
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return err
	}
	m.vs.mu.Lock()
	m.vs.snapshot, m.vs.snapshotData, m.vs.snapshotIDs = path, snap, ids
	m.vs.mu.Unlock()
	log.Infof("Vehicle settings: snapshot %s holds %d settings", path, len(snap.Settings))
	return nil
}

func init() {
	mqttCmd.Flags().String("vehicle_settings", vehicleSettingsOff, "Vehicle settings in Home Assistant: off, read (read-only sensors) or write (selects that change them)")
	mqttCmd.Flags().String("state_dir", "/var/lib/phev2mqtt", "Directory for vehicle settings snapshots")
	viper.BindPFlag("vehicle_settings", mqttCmd.Flags().Lookup("vehicle_settings"))
	viper.BindPFlag("state_dir", mqttCmd.Flags().Lookup("state_dir"))
}
