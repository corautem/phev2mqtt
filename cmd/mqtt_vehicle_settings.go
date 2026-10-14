package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/buxtronix/phev2mqtt/protocol"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Vehicle settings (--vehicle_settings, or VEHICLE_SETTINGS): the car's own
// customisable functions from register 0x16, decoded by
// protocol.VehicleSettings. "off" (default) publishes nothing and removes the
// entities; "read" shows every setting the car offers as a read-only sensor.
//
// The car sends its settings a few at a time, carrying on through the list
// over many minutes rather than all at session start. So in read mode an
// entity, once announced, is never removed, and its value is retained on the
// broker: Home Assistant keeps every setting across restarts of either side
// while the car works through its list again.

const (
	vehicleSettingsOff  = "off"
	vehicleSettingsRead = "read"
)

func vehicleSettingsMode() string {
	switch mode := strings.ToLower(strings.TrimSpace(viper.GetString("vehicle_settings"))); mode {
	case "", vehicleSettingsOff:
		return vehicleSettingsOff
	case vehicleSettingsRead:
		return vehicleSettingsRead
	default:
		log.Warnf("Unknown vehicle_settings %q (use off or read), vehicle settings stay off", mode)
		return vehicleSettingsOff
	}
}

// discoveryTarget is what Home Assistant discovery was last published with.
type discoveryTarget struct{ vin, topic, name string }

// vehicleSettingsState is used by the car loop (notifications) and by the
// MQTT callbacks (discovery after a reconnect or a Home Assistant restart).
type vehicleSettingsState struct {
	mu        sync.Mutex
	mode      string
	settings  protocol.VehicleSettings
	target    *discoveryTarget
	announced map[int]bool
}

const vehicleSettingsStatusKey = "vehicle_settings_status"

func vehicleSettingKey(id int) string { return fmt.Sprintf("vehicle_setting_%d", id) }

func vehicleSettingEntity(id int) haEntity {
	d, _ := protocol.SettingDefinition(id)
	return sensorEntity(vehicleSettingKey(id), d.Group+": "+d.Name, fmt.Sprintf("~/vehicle_setting/%d", id), map[string]interface{}{
		"icon": "mdi:car-cog", "entity_category": "diagnostic",
		"json_attributes_topic": fmt.Sprintf("~/vehicle_setting/%d/attributes", id)})
}

var vehicleSettingsStatusEntity = sensorEntity(vehicleSettingsStatusKey, "Vehicle Settings Changes", "~/vehicle_settings/status", map[string]interface{}{
	"device_class": "enum", "options": []string{"allowed", "refused"}, "icon": "mdi:car-cog", "entity_category": "diagnostic"})

// vehicleSettingsKeys are the keys of every entity vehicle settings can add.
func vehicleSettingsKeys() []string {
	keys := []string{vehicleSettingsStatusKey}
	for _, id := range protocol.SettingIDs() {
		keys = append(keys, vehicleSettingKey(id))
	}
	return keys
}

// vehicleSettingStateTopics are the state topics of a setting's entity,
// relative to the topic prefix.
func vehicleSettingStateTopics(id int) (value, attributes string) {
	return fmt.Sprintf("/vehicle_setting/%d", id), fmt.Sprintf("/vehicle_setting/%d/attributes", id)
}

const vehicleSettingsStatusTopic = "/vehicle_settings/status"

// publishVehicleSettingsDiscovery announces a sensor for every setting the
// car has reported so far; settings reported later are announced as they
// arrive. Entities announced before stay. When the feature is off, every
// vehicle settings entity and its retained values are removed.
func (m *mqttClient) publishVehicleSettingsDiscovery(vin, topic, name string) {
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	m.vs.target = &discoveryTarget{vin, topic, name}
	m.vs.announced = map[int]bool{}
	if m.vs.mode == vehicleSettingsOff {
		m.announce(vehicleSettingsStatusEntity, vin, topic, name, false)
		m.client.Publish(m.topic(vehicleSettingsStatusTopic), 0, true, "")
		for _, id := range protocol.SettingIDs() {
			m.announce(vehicleSettingEntity(id), vin, topic, name, false)
			value, attributes := vehicleSettingStateTopics(id)
			m.client.Publish(m.topic(value), 0, true, "")
			m.client.Publish(m.topic(attributes), 0, true, "")
		}
		return
	}
	m.announce(vehicleSettingsStatusEntity, vin, topic, name, true)
	for _, id := range protocol.SettingIDs() {
		if s, ok := m.vs.settings.Get(id); ok && protocol.SettingAvailable(id, s) {
			m.announce(vehicleSettingEntity(id), vin, topic, name, true)
			m.vs.announced[id] = true
		}
	}
}

// publishVehicleSettings folds a settings notification in and publishes what
// changed, retained, announcing settings Home Assistant doesn't know yet.
func (m *mqttClient) publishVehicleSettings(register byte, data []byte) {
	if register != protocol.SettingsRegister || m.vs.mode == vehicleSettingsOff {
		return
	}
	values := map[string]string{}

	m.vs.mu.Lock()
	for _, id := range m.vs.settings.Update(data) {
		s, _ := m.vs.settings.Get(id)
		if !protocol.SettingAvailable(id, s) {
			continue
		}
		if t := m.vs.target; t != nil && !m.vs.announced[id] {
			m.announce(vehicleSettingEntity(id), t.vin, t.topic, t.name, true)
			m.vs.announced[id] = true
		}
		var labels []string
		for _, o := range protocol.SettingOptions(id, s) {
			labels = append(labels, protocol.SettingOptionLabel(id, o, &s))
		}
		d, _ := protocol.SettingDefinition(id)
		attrs, _ := json.Marshal(map[string]interface{}{"group": d.Group, "setting_id": id, "options": labels})
		value, attributes := vehicleSettingStateTopics(id)
		values[value] = protocol.SettingOptionLabel(id, s.Value, &s)
		values[attributes] = string(attrs)
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

func init() {
	mqttCmd.Flags().String("vehicle_settings", vehicleSettingsOff, "Vehicle settings in Home Assistant: off, or read (show them as read-only sensors)")
	viper.BindPFlag("vehicle_settings", mqttCmd.Flags().Lookup("vehicle_settings"))
}
