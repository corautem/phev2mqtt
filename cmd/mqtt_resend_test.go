package cmd

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeMQTT records the last payload published to each topic.
type fakeMQTT struct {
	mqtt.Client
	mu  sync.Mutex
	pub map[string]string
}

func (f *fakeMQTT) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pub[topic] = fmt.Sprint(payload)
	return &mqtt.DummyToken{}
}

func (f *fakeMQTT) reset() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.pub
	f.pub = map[string]string{}
	return old
}

func newTestClient() (*mqttClient, *fakeMQTT) {
	f := &fakeMQTT{pub: map[string]string{}}
	return &mqttClient{client: f, prefix: "phev", mqttData: map[string]string{}, haDiscovery: true, haDiscoveryPrefix: "homeassistant"}, f
}

func TestRepublishState(t *testing.T) {
	m, f := newTestClient()
	m.publish("/car/clock", "2026-10-14T11:12:51+01:00")
	m.publish("/power/switch", "off")
	m.publish("/power/switch", "acc")
	f.reset()

	m.republishState()
	want := map[string]string{"phev/car/clock": "2026-10-14T11:12:51+01:00", "phev/power/switch": "acc"}
	if got := f.reset(); !reflect.DeepEqual(got, want) {
		t.Errorf("re-sent %v, want %v", got, want)
	}
}

func TestDiscoverySchedulesStateResend(t *testing.T) {
	defer func(d time.Duration) { stateResendDelay = d }(stateResendDelay)
	stateResendDelay = 10 * time.Millisecond

	m, f := newTestClient()
	m.publish("/battery/level", "94")
	f.reset()

	m.publishHomeAssistantDiscovery(testVIN, "phev", "Phev")
	time.Sleep(200 * time.Millisecond)
	if got := f.reset()["phev/battery/level"]; got != "94" {
		t.Errorf("battery level after discovery = %q, want it re-sent as 94", got)
	}
}

// The car loop publishes while a timer re-sends; unguarded, the map access
// would crash the process.
func TestRepublishStateConcurrent(t *testing.T) {
	m, _ := newTestClient()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				m.republishState()
			}
		}
	}()
	for i := 0; i < 200000; i++ {
		m.publish(fmt.Sprintf("/register/%d", i%5000), "00")
	}
	close(stop)
	<-done
}
