package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// Made up, so it can't match a real car.
const testVIN = "PHEVTEST00000000"

func TestOptionalEntityConfigs(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range optionalFeatures {
		for _, e := range f.entities {
			if seen[e.key] {
				t.Errorf("duplicate key %q", e.key)
			}
			seen[e.key] = true

			var c map[string]interface{}
			if err := json.Unmarshal([]byte(e.config(testVIN, "phev", "Phev")), &c); err != nil {
				t.Fatalf("%s: invalid JSON: %v", e.key, err)
			}
			if got, want := c["unique_id"], testVIN+"_"+e.key; got != want {
				t.Errorf("%s: unique_id %v, want %v", e.key, got, want)
			}
			// Sensors have a state topic, buttons only a command topic.
			topic, _ := c["state_topic"].(string)
			if topic == "" {
				topic, _ = c["command_topic"].(string)
			}
			if !strings.HasPrefix(topic, "~/") {
				t.Errorf("%s: topic %q is not under the topic prefix", e.key, topic)
			}
			want := "homeassistant/" + e.component + "/" + testVIN + "_" + e.key + "/config"
			if got := e.discoveryTopic("homeassistant", testVIN); got != want {
				t.Errorf("%s: discovery topic %s, want %s", e.key, got, want)
			}
		}
	}
}

func TestCleanupTarget(t *testing.T) {
	ours := extraSensorEntities[0]
	tests := []struct {
		name    string
		topic   string
		payload string
		want    bool
	}{
		{
			name:    "optional entity",
			topic:   ours.discoveryTopic("homeassistant", testVIN),
			payload: ours.config(testVIN, "phev", "Phev"),
			want:    true,
		}, {
			// Ends in "_charge_timer" but is one of the original entities.
			name:    "original cancel charge timer",
			topic:   "homeassistant/switch/" + testVIN + "_cancel_charge_timer/config",
			payload: `{"unique_id":"` + testVIN + `_cancel_charge_timer","dev":{"identifiers":["phev-` + testVIN + `"]}}`,
			want:    false,
		}, {
			name:    "original entity using device",
			topic:   "homeassistant/binary_sensor/" + testVIN + "_door_locked/config",
			payload: `{"device":{"identifiers":["phev-` + testVIN + `"]}}`,
			want:    false,
		}, {
			name:    "same object id, other device",
			topic:   ours.discoveryTopic("homeassistant", testVIN),
			payload: `{"dev":{"identifiers":["other-device"]}}`,
			want:    false,
		}, {
			name:    "already removed",
			topic:   ours.discoveryTopic("homeassistant", testVIN),
			payload: "",
			want:    false,
		}, {
			name:    "not a config topic",
			topic:   "homeassistant/status",
			payload: "online",
			want:    false,
		},
	}
	for _, test := range tests {
		if got := cleanupTarget(test.topic, []byte(test.payload)); got != test.want {
			t.Errorf("%s: cleanupTarget = %v, want %v", test.name, got, test.want)
		}
	}
}
