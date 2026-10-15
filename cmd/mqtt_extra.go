package cmd

import (
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Optional features are off by default and each switched on with its own
// flag, or the matching environment variable (EXTRA_SENSORS=true). Older
// builds ignore the environment variables but refuse unknown flags, so the
// variables keep a rollback to an older build working. When a feature is off,
// its Home Assistant entities are removed by publishing empty discovery
// configs, so switching it off and restarting cleans up after it.

// haEntity is a Home Assistant MQTT discovery entity added by an optional
// feature. Its object id and unique id are <VIN>_<key>.
type haEntity struct {
	component string
	key       string
	name      string
	fields    map[string]interface{}
}

func (e haEntity) discoveryTopic(prefix, vin string) string {
	return fmt.Sprintf("%s/%s/%s_%s/config", prefix, e.component, vin, e.key)
}

func (e haEntity) config(vin, topic, name string) string {
	c := map[string]interface{}{
		"name":      name + " " + e.name,
		"unique_id": vin + "_" + e.key,
		"avty_t":    "~/available",
		"~":         topic,
		"dev": map[string]interface{}{
			"name":         "PHEV " + vin,
			"identifiers":  []string{"phev-" + vin},
			"manufacturer": "Mitsubishi",
			"model":        "Outlander PHEV",
		},
	}
	for k, v := range e.fields {
		c[k] = v
	}
	b, err := json.Marshal(c)
	if err != nil {
		log.Errorf("Discovery config for %s: %v", e.key, err)
		return ""
	}
	return string(b)
}

func sensorEntity(key, name, stateTopic string, fields map[string]interface{}) haEntity {
	f := map[string]interface{}{"state_topic": stateTopic}
	for k, v := range fields {
		f[k] = v
	}
	return haEntity{component: "sensor", key: key, name: name, fields: f}
}

func binarySensorEntity(key, name, stateTopic string, fields map[string]interface{}) haEntity {
	f := map[string]interface{}{"state_topic": stateTopic, "payload_on": "on", "payload_off": "off"}
	for k, v := range fields {
		f[k] = v
	}
	return haEntity{component: "binary_sensor", key: key, name: name, fields: f}
}

// extraSensorEntities are published with --extra_sensors. Their values come
// from protocol.StatusTracker.
var extraSensorEntities = []haEntity{
	sensorEntity("power_switch", "Power Switch", "~/power/switch", map[string]interface{}{
		"device_class": "enum", "options": []string{"off", "acc", "on", "unknown"}, "icon": "mdi:car-key"}),
	sensorEntity("alarm_state", "Security Alarm", "~/alarm/state", map[string]interface{}{
		"device_class": "enum", "options": []string{"disarmed", "armed", "triggered", "unknown"}, "icon": "mdi:shield-car"}),
	sensorEntity("alarm_last_event", "Last Alarm Event", "~/alarm/last_event", map[string]interface{}{
		"device_class": "timestamp", "icon": "mdi:alarm-light"}),
	sensorEntity("charge_type", "Charge Type", "~/charge/type", map[string]interface{}{
		"device_class": "enum", "options": []string{"none", "normal", "trickle", "quick", "unknown"}, "icon": "mdi:ev-station"}),
	binarySensorEntity("charge_timer_waiting", "Waiting For Car Charge Timer", "~/charge/timer_waiting", map[string]interface{}{
		"icon": "mdi:timer-sand"}),
	binarySensorEntity("charge_timer_failed", "Scheduled Charge Failed", "~/charge/timer_failed", map[string]interface{}{
		"device_class": "problem"}),
	binarySensorEntity("car_charge_timer", "Car Charge Timer", "~/charge/timer", map[string]interface{}{
		"icon": "mdi:timer-outline", "json_attributes_topic": "~/charge/timer_schedule"}),
	binarySensorEntity("car_climate_timer", "Car Climate Timer", "~/climate/timer", map[string]interface{}{
		"icon": "mdi:fan-clock", "json_attributes_topic": "~/climate/timer_schedule"}),
	binarySensorEntity("battery_heater", "Battery Heater", "~/battery/heater", map[string]interface{}{
		"device_class": "heat"}),
	binarySensorEntity("battery_too_cold", "Battery Too Cold", "~/battery/too_cold", map[string]interface{}{
		"device_class": "problem", "icon": "mdi:snowflake-alert"}),
	binarySensorEntity("ev_ecu_error", "EV-ECU Error", "~/car/ev_ecu_error", map[string]interface{}{
		"device_class": "problem", "entity_category": "diagnostic"}),
	binarySensorEntity("screen_in_use", "Car Screen In Use", "~/car/screen_in_use", map[string]interface{}{
		"icon": "mdi:monitor-dashboard"}),
	binarySensorEntity("registration_standby", "Registration Standby", "~/car/registration_standby", map[string]interface{}{
		"icon": "mdi:cellphone-link", "entity_category": "diagnostic"}),
	sensorEntity("car_clock", "Car Clock", "~/car/clock", map[string]interface{}{
		"device_class": "timestamp", "icon": "mdi:clock-outline", "entity_category": "diagnostic"}),
}

// optionalFeature groups the entities of one flag-controlled feature.
type optionalFeature struct {
	flag     string
	entities []haEntity
}

var optionalFeatures = []optionalFeature{
	{"extra_sensors", extraSensorEntities},
}

// publishOptionalDiscovery announces the entities of every enabled optional
// feature and removes those of disabled ones.
func (m *mqttClient) publishOptionalDiscovery(vin, topic, name string) {
	for _, f := range optionalFeatures {
		enabled := viper.GetBool(f.flag)
		for _, e := range f.entities {
			m.announce(e, vin, topic, name, enabled)
		}
	}
	m.publishVehicleSettingsDiscovery(vin, topic, name)
}

// announce publishes an entity's discovery config, or an empty one to remove
// the entity from Home Assistant.
func (m *mqttClient) announce(e haEntity, vin, topic, name string, enabled bool) {
	payload := ""
	if enabled {
		payload = e.config(vin, topic, name)
	}
	if token := m.client.Publish(e.discoveryTopic(m.haDiscoveryPrefix, vin), 1, true, payload); token.Wait() && token.Error() != nil {
		log.Error(token.Error())
	}
}

// publishExtraStatus publishes the --extra_sensors values derived from a
// register notification.
func (m *mqttClient) publishExtraStatus(register byte, data []byte) {
	if !m.extraSensors {
		return
	}
	for t, p := range m.status.Update(register, data) {
		m.publish(t, p)
	}
}

func init() {
	mqttCmd.Flags().Bool("extra_sensors", false, "Publish extra status sensors (power switch, alarm, charge type, car timers, ...)")
	viper.BindPFlag("extra_sensors", mqttCmd.Flags().Lookup("extra_sensors"))
	mqttCmd.Flags().Bool("key_resync", false, "Recover from the car's encoding complaints: resync to the key it expects, cap resends, ask for a new key")
	viper.BindPFlag("key_resync", mqttCmd.Flags().Lookup("key_resync"))
}
