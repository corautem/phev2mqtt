package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/buxtronix/phev2mqtt/protocol"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Vehicle settings (--vehicle_settings, or VEHICLE_SETTINGS): the car's own
// customisable functions from register 0x16, decoded by
// protocol.VehicleSettings. "off" (default) publishes nothing and removes the
// entities; "read" shows every setting the car offers as a read-only sensor.

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

// publishVehicleSettingsDiscovery announces a sensor for every setting the
// car has reported so far and removes the others, or all of them when the
// feature is off. Settings reported later are announced as they arrive.
func (m *mqttClient) publishVehicleSettingsDiscovery(vin, topic, name string) {
	m.vs.mu.Lock()
	defer m.vs.mu.Unlock()
	m.vs.target = &discoveryTarget{vin, topic, name}
	m.vs.announced = map[int]bool{}
	on := m.vs.mode == vehicleSettingsRead
	m.announce(vehicleSettingsStatusEntity, vin, topic, name, on)
	for _, id := range protocol.SettingIDs() {
		s, ok := m.vs.settings.Get(id)
		show := on && ok && protocol.SettingAvailable(id, s)
		m.announce(vehicleSettingEntity(id), vin, topic, name, show)
		m.vs.announced[id] = show
	}
}

// publishVehicleSettings folds a settings notification in and publishes what
// changed, announcing settings Home Assistant doesn't know yet.
func (m *mqttClient) publishVehicleSettings(register byte, data []byte) {
	if register != protocol.SettingsRegister || m.vs.mode == vehicleSettingsOff {
		return
	}
	values := map[string]string{}
	newEntities := false

	m.vs.mu.Lock()
	for _, id := range m.vs.settings.Update(data) {
		s, _ := m.vs.settings.Get(id)
		if !protocol.SettingAvailable(id, s) {
			continue
		}
		if t := m.vs.target; t != nil && !m.vs.announced[id] {
			m.announce(vehicleSettingEntity(id), t.vin, t.topic, t.name, true)
			m.vs.announced[id] = true
			newEntities = true
		}
		var labels []string
		for _, o := range protocol.SettingOptions(id, s) {
			labels = append(labels, protocol.SettingOptionLabel(id, o, &s))
		}
		d, _ := protocol.SettingDefinition(id)
		attrs, _ := json.Marshal(map[string]interface{}{"group": d.Group, "setting_id": id, "options": labels})
		values[fmt.Sprintf("/vehicle_setting/%d", id)] = protocol.SettingOptionLabel(id, s.Value, &s)
		values[fmt.Sprintf("/vehicle_setting/%d/attributes", id)] = string(attrs)
	}
	if m.vs.settings.Known() {
		status := "allowed"
		if !protocol.SettingsStatusAllowsChanges(m.vs.settings.Status) {
			status = "refused"
		}
		values["/vehicle_settings/status"] = status
	}
	m.vs.mu.Unlock()

	for t, p := range values {
		m.publish(t, p)
	}
	if newEntities {
		// Home Assistant subscribes to the new entities' topics only now.
		time.AfterFunc(stateResendDelay, m.republishState)
	}
}

func init() {
	mqttCmd.Flags().String("vehicle_settings", vehicleSettingsOff, "Vehicle settings in Home Assistant: off, or read (show them as read-only sensors)")
	viper.BindPFlag("vehicle_settings", mqttCmd.Flags().Lookup("vehicle_settings"))
}
