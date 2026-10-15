package protocol

import (
	"fmt"
	"sort"
)

// Vehicle settings are the car's own customisable functions (lights, locks,
// wipers, A/C behaviour...), as in the official app's "Vehicle settings".
// The wire format was reverse-engineered by phev-remote-plus
// (https://github.com/Anwo91/phev-remote-plus, core/model/VehicleSettings.kt)
// from the official app:
//
// A register 0x16 notification is [status][A1][B1][A2][B2][A3][B3][00]. The
// status is normally 2; with 1 or 3 the car refuses changes. Each (A, B) pair
// is one setting:
//
//	id    = A & 63
//	mask  = (B >> 1) & 127   bit k set: option k is available on this car
//	value = ((B & 1) << 2) | ((A >> 6) & 3)   7: the car reports "unknown"
//
// The car spreads its settings over several notifications, so they are
// accumulated. Writes (not used yet): register 0x0f [id, value] changes one
// setting, then register 0x0e [0] saves.

// VehicleSetting is one setting as the car reports it.
type VehicleSetting struct {
	Mask  int
	Value int
}

// SettingDef is a catalogue entry: name and option labels as the official
// app shows them.
type SettingDef struct {
	ID      int
	Name    string
	Group   string
	Options []string
}

const (
	minSettingID = 1
	maxSettingID = 60

	// SetVehicleSettingRegister changes one setting: [id, value].
	SetVehicleSettingRegister = 0x0f
	// SaveVehicleSettingsRegister saves changed settings: [0].
	SaveVehicleSettingsRegister = 0x0e
)

// DecodeSettingPair decodes one (A, B) pair of a 0x16 notification. It
// returns false for padding (id 0), ids out of range and values the mask
// does not allow, which the official app ignores too.
func DecodeSettingPair(a, b byte) (int, VehicleSetting, bool) {
	id := int(a & 63)
	s := VehicleSetting{Mask: int(b>>1) & 127, Value: int(b&1)<<2 | int(a>>6)&3}
	if id < minSettingID || id > maxSettingID {
		return 0, s, false
	}
	if !(s.Mask == 0 || s.Value == 7 || (s.Mask>>s.Value)&1 == 1) {
		return 0, s, false
	}
	return id, s, true
}

// EncodeSettingPair is the inverse of DecodeSettingPair.
func EncodeSettingPair(id int, s VehicleSetting) (byte, byte) {
	a := byte(id&63) | byte(s.Value&3)<<6
	b := byte(s.Mask&127)<<1 | byte(s.Value>>2)&1
	return a, b
}

// SettingDefinition returns the catalogue entry for a named, changeable id.
func SettingDefinition(id int) (SettingDef, bool) {
	d, ok := settingDefs[id]
	return d, ok
}

// SettingIDs returns every named setting id, in the official app's display
// order.
func SettingIDs() []int {
	var ids []int
	seen := map[int]bool{}
	for _, g := range settingGroups {
		for _, id := range g.IDs {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	var rest []int
	for id := range settingDefs {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Ints(rest)
	return append(ids, rest...)
}

func settingOptionCount(id int) int {
	if id < 0 || id >= len(settingOptionCounts) {
		return 0
	}
	return settingOptionCounts[id]
}

// SettingAvailable reports whether the official app would show the setting:
// the id is named and not hidden, the car offers at least one option, the
// current value is a known option and the mask fits the option count.
func SettingAvailable(id int, s VehicleSetting) bool {
	if hiddenSettingIDs[id] {
		return false
	}
	d, ok := settingDefs[id]
	if !ok || d.Name == "" {
		return false
	}
	n := settingOptionCount(id)
	if n <= 0 {
		return false
	}
	return s.Mask != 0 && s.Value >= 0 && s.Value < n && s.Mask <= 1<<n-1
}

// SettingOptions returns the option indices the car offers for the setting,
// or nil when it is not available.
func SettingOptions(id int, s VehicleSetting) []int {
	if !SettingAvailable(id, s) {
		return nil
	}
	var opts []int
	for o := 0; o < settingOptionCount(id); o++ {
		if (s.Mask>>o)&1 == 1 {
			opts = append(opts, o)
		}
	}
	return opts
}

// SettingOptionLabel returns the label of an option. When options in play
// share the official label (id 12 repeats "On"), a stable suffix tells them
// apart: "On (type 2)" is always option 3 of id 12. With s, only the options
// the car offers count as in play; with nil, all of them do.
func SettingOptionLabel(id, option int, s *VehicleSetting) string {
	d, ok := settingDefs[id]
	if !ok || option < 0 || option >= len(d.Options) || d.Options[option] == "" {
		return fmt.Sprintf("Option %d", option+1)
	}
	base := d.Options[option]
	var inPlay []int
	if s != nil && SettingAvailable(id, *s) {
		inPlay = SettingOptions(id, *s)
	} else {
		for o := range d.Options {
			inPlay = append(inPlay, o)
		}
	}
	collides := false
	for _, o := range inPlay {
		if o != option && o < len(d.Options) && d.Options[o] == base {
			collides = true
		}
	}
	if !collides {
		return base
	}
	ordinal := 0
	for o := 0; o <= option; o++ {
		if d.Options[o] == base {
			ordinal++
		}
	}
	return fmt.Sprintf("%s (type %d)", base, ordinal)
}

// SettingsStatusAllowsChanges reports whether the car accepts setting
// changes; the official app refuses to change settings with status 1 or 3.
func SettingsStatusAllowsChanges(status int) bool {
	return status != 1 && status != 3
}

// VehicleSettings accumulates the settings the car reports in register 0x16.
type VehicleSettings struct {
	// Status is the status byte of the last notification, -1 before any.
	Status   int
	settings map[int]VehicleSetting
}

// Update folds a 0x16 notification in and returns the ids whose setting is
// new or changed, in ascending order. Notifications shorter than one pair
// are ignored.
func (v *VehicleSettings) Update(data []byte) []int {
	if v.settings == nil {
		v.settings = map[int]VehicleSetting{}
		v.Status = -1
	}
	if len(data) < 3 {
		return nil
	}
	v.Status = int(data[0])
	var changed []int
	for i := 0; i < 3; i++ {
		ai, bi := 1+2*i, 2+2*i
		if bi >= len(data) {
			break
		}
		id, s, ok := DecodeSettingPair(data[ai], data[bi])
		if !ok {
			continue
		}
		if old, known := v.settings[id]; !known || old != s {
			v.settings[id] = s
			changed = append(changed, id)
		}
	}
	sort.Ints(changed)
	return changed
}

// Get returns the setting the car last reported for id.
func (v *VehicleSettings) Get(id int) (VehicleSetting, bool) {
	s, ok := v.settings[id]
	return s, ok
}

// Known reports whether the car has sent any settings notification.
func (v *VehicleSettings) Known() bool {
	return v.settings != nil && v.Status >= 0
}

// Seed records a setting remembered from an earlier run, unless the car has
// already reported that id. It reports whether the setting was added.
func (v *VehicleSettings) Seed(id int, s VehicleSetting) bool {
	if v.settings == nil {
		v.settings = map[int]VehicleSetting{}
		v.Status = -1
	}
	if _, known := v.settings[id]; known || id < minSettingID || id > maxSettingID {
		return false
	}
	v.settings[id] = s
	return true
}

// All returns a copy of every setting known so far.
func (v *VehicleSettings) All() map[int]VehicleSetting {
	out := make(map[int]VehicleSetting, len(v.settings))
	for id, s := range v.settings {
		out[id] = s
	}
	return out
}

// OptionByLabel returns the option of the setting whose label (as
// SettingOptionLabel gives it for s) is label, among the options s offers.
func OptionByLabel(id int, s VehicleSetting, label string) (int, bool) {
	for _, o := range SettingOptions(id, s) {
		if SettingOptionLabel(id, o, &s) == label {
			return o, true
		}
	}
	return 0, false
}
