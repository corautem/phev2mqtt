package cmd

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var mqttCleanupCmd = &cobra.Command{
	Use:   "mqtt-cleanup",
	Short: "Remove the Home Assistant entities added by optional features.",
	Long: `Removes every Home Assistant discovery entity that the optional features
(--extra_sensors and later ones) may have created, whatever flags the running
gateway uses. The entities phev2mqtt has always published are left alone.

Run it before rolling back to an older phev2mqtt build, so Home Assistant is
not left with entities that nothing updates any more.
`,
	SilenceUsage: true,
	RunE:         runMqttCleanup,
}

// optionalObjectIDs returns, for a VIN, the discovery object ids of every
// entity an optional feature can add.
func optionalObjectIDs(vin string) map[string]bool {
	ids := map[string]bool{}
	for _, f := range optionalFeatures {
		for _, e := range f.entities {
			ids[vin+"_"+e.key] = true
		}
	}
	for _, key := range vehicleSettingsKeys() {
		ids[vin+"_"+key] = true
	}
	return ids
}

// cleanupTarget reports whether a retained discovery config belongs to an
// optional phev2mqtt entity. The VIN comes from the config's device
// identifiers ("phev-<VIN>"), so the match on <VIN>_<key> is exact.
func cleanupTarget(topic string, payload []byte) bool {
	parts := strings.Split(topic, "/")
	if len(parts) < 4 || parts[len(parts)-1] != "config" || len(payload) == 0 {
		return false
	}
	var c struct {
		Dev    struct{ Identifiers []string } `json:"dev"`
		Device struct{ Identifiers []string } `json:"device"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return false
	}
	objectID := parts[len(parts)-2]
	for _, id := range append(c.Dev.Identifiers, c.Device.Identifiers...) {
		if vin := strings.TrimPrefix(id, "phev-"); vin != id && optionalObjectIDs(vin)[objectID] {
			return true
		}
	}
	return false
}

func runMqttCleanup(cmd *cobra.Command, args []string) error {
	server, _ := cmd.Flags().GetString("mqtt_server")
	username, _ := cmd.Flags().GetString("mqtt_username")
	password, _ := cmd.Flags().GetString("mqtt_password")
	prefix, _ := cmd.Flags().GetString("ha_discovery_prefix")
	wait, _ := cmd.Flags().GetDuration("wait")

	// A client id of its own, so the running gateway is not disconnected.
	opts := mqtt.NewClientOptions().
		AddBroker(server).
		SetClientID("phev2mqtt-cleanup").
		SetUsername(username).
		SetPassword(password)
	c := mqtt.NewClient(opts)
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer c.Disconnect(250)

	var mu sync.Mutex
	targets := map[string]bool{}
	collect := func(_ mqtt.Client, msg mqtt.Message) {
		if msg.Retained() && cleanupTarget(msg.Topic(), msg.Payload()) {
			mu.Lock()
			targets[msg.Topic()] = true
			mu.Unlock()
		}
	}
	if token := c.Subscribe(prefix+"/+/+/config", 0, collect); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	// Retained messages arrive right after subscribing.
	time.Sleep(wait)
	c.Unsubscribe(prefix + "/+/+/config").Wait()

	mu.Lock()
	defer mu.Unlock()
	for topic := range targets {
		if token := c.Publish(topic, 1, true, ""); token.Wait() && token.Error() != nil {
			log.Errorf("Removing %s: %v", topic, token.Error())
			continue
		}
		log.Infof("Removed %s", topic)
	}
	log.Infof("Removed %d optional Home Assistant entities", len(targets))
	return nil
}

func init() {
	clientCmd.AddCommand(mqttCleanupCmd)
	mqttCleanupCmd.Flags().String("mqtt_server", "tcp://127.0.0.1:1883", "Address of MQTT server")
	mqttCleanupCmd.Flags().String("mqtt_username", "", "Username to login to MQTT server")
	mqttCleanupCmd.Flags().String("mqtt_password", "", "Password to login to MQTT server")
	mqttCleanupCmd.Flags().String("ha_discovery_prefix", "homeassistant", "Prefix for Home Assistant MQTT discovery")
	mqttCleanupCmd.Flags().Duration("wait", 3*time.Second, "How long to collect retained discovery configs")
}
