package cmd

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func settingConfigTopic(id int) string {
	return "homeassistant/sensor/" + testVIN + "_" + vehicleSettingKey(id) + "/config"
}

func mustBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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
	if _, ok := got[settingConfigTopic(7)]; ok {
		t.Error("announced before discovery")
	}

	// Discovery announces the reported settings and removes the rest.
	m.publishVehicleSettingsDiscovery(testVIN, "phev", "Phev")
	got = f.reset()
	if !strings.Contains(got[settingConfigTopic(7)], "Lights: Charging Lid Light Auto cut-out") {
		t.Errorf("id 7 config = %q", got[settingConfigTopic(7)])
	}
	if got[settingConfigTopic(11)] == "" || got[settingConfigTopic(13)] != "" {
		t.Errorf("id 11 config %q, id 13 config %q", got[settingConfigTopic(11)], got[settingConfigTopic(13)])
	}
	if got["homeassistant/sensor/"+testVIN+"_"+vehicleSettingsStatusKey+"/config"] == "" {
		t.Error("status entity not announced")
	}

	// A setting reported later is announced as it arrives; a changed one republished.
	m.publishVehicleSettings(0x16, mustBytes(t, "02473e0d1e000000"))
	got = f.reset()
	if got[settingConfigTopic(13)] == "" || got["phev/vehicle_setting/13"] != "Off" || got["phev/vehicle_setting/7"] != "1 minute" {
		t.Errorf("after second notification: %v", got)
	}
	if _, ok := got[settingConfigTopic(7)]; ok {
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
	if payload, ok := got[settingConfigTopic(7)]; !ok || payload != "" {
		t.Errorf("off mode: id 7 config %q (present %v), want an empty config", payload, ok)
	}
	for topic, payload := range got {
		if payload != "" {
			t.Errorf("off mode announced %s", topic)
		}
	}
}

func TestVehicleSettingsCleanup(t *testing.T) {
	e := vehicleSettingEntity(7)
	if !cleanupTarget(e.discoveryTopic("homeassistant", testVIN), []byte(e.config(testVIN, "phev", "Phev"))) {
		t.Error("cleanup misses vehicle setting entities")
	}
	s := vehicleSettingsStatusEntity
	if !cleanupTarget(s.discoveryTopic("homeassistant", testVIN), []byte(s.config(testVIN, "phev", "Phev"))) {
		t.Error("cleanup misses the vehicle settings status entity")
	}
}

func TestVehicleSettingsMode(t *testing.T) {
	defer viper.Set("vehicle_settings", nil)
	for in, want := range map[string]string{"": "off", "off": "off", "READ": "read", " read ": "read", "true": "off", "write": "off"} {
		viper.Set("vehicle_settings", in)
		if got := vehicleSettingsMode(); got != want {
			t.Errorf("vehicleSettingsMode(%q) = %q, want %q", in, got, want)
		}
	}
}
