package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var restoreSettingsCmd = &cobra.Command{
	Use:   "restore-settings",
	Short: "Put the car's vehicle settings back from a snapshot.",
	Long: `Sends every setting in a vehicle settings snapshot to the running gateway,
which changes those that differ on the car, one at a time.

The gateway saves a snapshot in its --state_dir (default /var/lib/phev2mqtt)
before the first change of each run, named vehicle-settings-<date>-<time>.json.
It must run with VEHICLE_SETTINGS=write and be connected to the car. Follow the
progress in Home Assistant's "Vehicle Settings Last Change" sensor, or in the
gateway's log.
`,
	SilenceUsage: true,
	RunE:         runRestoreSettings,
}

func loadSnapshot(path string) (settingsSnapshot, error) {
	var snap settingsSnapshot
	data, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, fmt.Errorf("%s is not a vehicle settings snapshot: %v", path, err)
	}
	if len(snap.Settings) == 0 {
		return snap, fmt.Errorf("%s holds no settings", path)
	}
	sort.Slice(snap.Settings, func(i, j int) bool { return snap.Settings[i].ID < snap.Settings[j].ID })
	return snap, nil
}

// restoreCommand is the topic and payload that sets a snapshot setting.
func restoreCommand(prefix string, s snapshotSetting) (string, string) {
	return prefix + vehicleSettingCommandPrefix + strconv.Itoa(s.ID), strconv.Itoa(s.Value)
}

func runRestoreSettings(cmd *cobra.Command, args []string) error {
	file, _ := cmd.Flags().GetString("file")
	server, _ := cmd.Flags().GetString("mqtt_server")
	username, _ := cmd.Flags().GetString("mqtt_username")
	password, _ := cmd.Flags().GetString("mqtt_password")
	prefix, _ := cmd.Flags().GetString("mqtt_topic_prefix")

	snap, err := loadSnapshot(file)
	if err != nil {
		return err
	}
	c := mqtt.NewClient(mqtt.NewClientOptions().
		AddBroker(server).
		SetClientID("phev2mqtt-restore").
		SetUsername(username).
		SetPassword(password))
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	defer c.Disconnect(250)

	log.Infof("Restoring %d vehicle settings from %s (taken %s)", len(snap.Settings), file, snap.Taken)
	for _, s := range snap.Settings {
		topic, payload := restoreCommand(prefix, s)
		if token := c.Publish(topic, 1, false, payload); token.Wait() && token.Error() != nil {
			return token.Error()
		}
		log.Infof("Sent %s = %s", s.Name, s.Label)
		time.Sleep(200 * time.Millisecond)
	}
	log.Infof("Sent %d settings. The gateway changes those that differ, one at a time (up to about 10 s each); unchanged ones show as \"already set\".", len(snap.Settings))
	return nil
}

func init() {
	clientCmd.AddCommand(restoreSettingsCmd)
	restoreSettingsCmd.Flags().String("file", "", "Vehicle settings snapshot to restore")
	restoreSettingsCmd.Flags().String("mqtt_server", "tcp://127.0.0.1:1883", "Address of MQTT server")
	restoreSettingsCmd.Flags().String("mqtt_username", "", "Username to login to MQTT server")
	restoreSettingsCmd.Flags().String("mqtt_password", "", "Password to login to MQTT server")
	restoreSettingsCmd.Flags().String("mqtt_topic_prefix", "phev", "Prefix for MQTT topics")
	restoreSettingsCmd.MarkFlagRequired("file")
}
