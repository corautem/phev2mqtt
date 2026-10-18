package protocol

import (
	"reflect"
	"testing"
)

// Vectors and expectations follow PHEV Link (phev-remote-plus,
// https://github.com/Anwo91/phev-remote-plus) VehicleSettingsTest.kt,
// Copyright (C) 2026 Anwo91 and contributors, from its source as published on
// 14 October 2026 under the GNU General Public License version 3 or later.
// See README.md, Credits.

func TestSettingCatalogueConsistent(t *testing.T) {
	grouped := map[int]bool{}
	for _, g := range settingGroups {
		for _, id := range g.IDs {
			if grouped[id] {
				t.Errorf("id %d in more than one group", id)
			}
			grouped[id] = true
		}
	}
	for id, d := range settingDefs {
		if d.ID != id || d.Name == "" {
			t.Errorf("bad definition for %d: %+v", id, d)
		}
		if got := settingOptionCount(id); got != len(d.Options) {
			t.Errorf("id %d: option count %d, %d labels", id, got, len(d.Options))
		}
		if !grouped[id] {
			t.Errorf("id %d is in no group", id)
		}
	}
	for id := range hiddenSettingIDs {
		if _, ok := settingDefs[id]; ok {
			t.Errorf("hidden id %d has a definition", id)
		}
	}
	if len(settingDefs) != 41 {
		t.Errorf("%d named settings, want 41", len(settingDefs))
	}
	if ids := SettingIDs(); len(ids) != 41 || ids[0] != 3 {
		t.Errorf("SettingIDs = %v, want 41 ids starting with 3 (Keyless group)", ids)
	}
}

func TestVehicleSettingsHandComputed(t *testing.T) {
	// 02 | 07 3e | 8b 0b | 00 00 | 00
	// pair 1: id 7, mask 31, value 0: accepted.
	// pair 2: id 11, value 6, but option 6 is not in mask 0b101: rejected.
	var v VehicleSettings
	if got := v.Update(mustHex(t, "02073e8b0b000000")); !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("changed = %v, want [7]", got)
	}
	if s, _ := v.Get(7); s != (VehicleSetting{31, 0}) {
		t.Errorf("id 7 = %+v", s)
	}
	if _, ok := v.Get(11); ok {
		t.Error("id 11 accepted with a value its mask lacks")
	}
	if v.Status != 2 || !v.Known() {
		t.Errorf("status %d, known %v", v.Status, v.Known())
	}
	s7, _ := v.Get(7)
	if !SettingAvailable(7, s7) || SettingOptionLabel(7, 0, &s7) != "Off" {
		t.Errorf("id 7 not shown as Off")
	}

	// id 11 with every option offered: mask 127, value 6 ("3 minutes").
	var v2 VehicleSettings
	v2.Update(mustHex(t, "028bff"))
	s11, _ := v2.Get(11)
	if s11 != (VehicleSetting{127, 6}) || SettingOptionLabel(11, 6, &s11) != "3 minutes" {
		t.Errorf("id 11 = %+v %q", s11, SettingOptionLabel(11, 6, &s11))
	}
}

func TestVehicleSettingsAccumulate(t *testing.T) {
	var v VehicleSettings
	v.Update(mustHex(t, "02073e8bff000000"))
	// id 7 value 1; id 13 mask 15 value 0
	if got := v.Update(mustHex(t, "02473e0d1e000000")); !reflect.DeepEqual(got, []int{7, 13}) {
		t.Errorf("changed = %v, want [7 13]", got)
	}
	want := map[int]VehicleSetting{7: {31, 1}, 11: {127, 6}, 13: {15, 0}}
	if !reflect.DeepEqual(v.settings, want) {
		t.Errorf("settings = %v, want %v", v.settings, want)
	}
	if got := v.Update(mustHex(t, "02473e0d1e000000")); len(got) != 0 {
		t.Errorf("repeat reported changes %v", got)
	}
}

func TestVehicleSettingsInvalid(t *testing.T) {
	if _, _, ok := DecodeSettingPair(0x3d, 0x02); ok {
		t.Error("id 61 accepted")
	}
	if _, _, ok := DecodeSettingPair(0x00, 0x02); ok {
		t.Error("padding accepted")
	}
	// value 7 is accepted even when not in the mask, but not shown.
	if id, s, ok := DecodeSettingPair(0xc5, 0x0f); !ok || id != 5 || s != (VehicleSetting{7, 7}) || SettingAvailable(5, s) {
		t.Errorf("value 7: %d %+v %v", id, s, ok)
	}
	// mask 0 (not equipped) is accepted but not shown.
	if id, s, ok := DecodeSettingPair(0x17, 0x00); !ok || id != 23 || s != (VehicleSetting{0, 0}) {
		t.Errorf("mask 0: %d %+v %v", id, s, ok)
	}
	var short VehicleSettings
	short.Update(mustHex(t, "0207"))
	if short.Known() {
		t.Error("too-short notification counted")
	}
	var partial VehicleSettings
	partial.Update(mustHex(t, "01073e8b"))
	if s, ok := partial.Get(7); !ok || s != (VehicleSetting{31, 0}) || partial.Status != 1 {
		t.Errorf("partial: %+v status %d", s, partial.Status)
	}
	if SettingsStatusAllowsChanges(1) || SettingsStatusAllowsChanges(3) || !SettingsStatusAllowsChanges(2) {
		t.Error("status rules")
	}
}

func TestSettingAvailability(t *testing.T) {
	tests := []struct {
		id   int
		s    VehicleSetting
		want bool
	}{
		{7, VehicleSetting{31, 0}, true},
		{7, VehicleSetting{63, 0}, false}, // mask wider than 5 options
		{7, VehicleSetting{0, 0}, false},  // not equipped
		{7, VehicleSetting{31, 5}, false}, // value out of range
		{23, VehicleSetting{3, 0}, false}, // hidden (theft alarm)
		{18, VehicleSetting{1, 0}, false}, // unused id
	}
	for _, test := range tests {
		if got := SettingAvailable(test.id, test.s); got != test.want {
			t.Errorf("SettingAvailable(%d, %+v) = %v", test.id, test.s, got)
		}
	}
	if got := SettingOptions(4, VehicleSetting{0b0111, 2}); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("options = %v", got)
	}
	if got := SettingOptions(4, VehicleSetting{0b1010, 1}); !reflect.DeepEqual(got, []int{1, 3}) {
		t.Errorf("options = %v", got)
	}
	if got := SettingOptions(4, VehicleSetting{0b10000, 1}); got != nil {
		t.Errorf("options = %v, want none", got)
	}
}

func TestSettingLabelsDisambiguate(t *testing.T) {
	all := VehicleSetting{63, 2}
	var got []string
	for o := 0; o < 6; o++ {
		got = append(got, SettingOptionLabel(12, o, &all))
	}
	want := []string{"Off", "Tail Lamp/Head Lamp: Cutout always", "On (type 1)", "On (type 2)",
		"Tail Lamp: Cutout once / Head Lamp: Cutout always", "On (type 3)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %q", got)
	}
	one := VehicleSetting{0b000101, 2}
	if l := SettingOptionLabel(12, 2, &one); l != "On" {
		t.Errorf("single On = %q", l)
	}
	some := VehicleSetting{0b101001, 3}
	got = nil
	for _, o := range SettingOptions(12, some) {
		got = append(got, SettingOptionLabel(12, o, &some))
	}
	if want := []string{"Off", "On (type 2)", "On (type 3)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
	if l := SettingOptionLabel(12, 3, nil); l != "On (type 2)" {
		t.Errorf("label without setting = %q", l)
	}
	if l := SettingOptionLabel(7, 9, nil); l != "Option 10" {
		t.Errorf("unknown option = %q", l)
	}
}

func TestSettingPairRoundTrip(t *testing.T) {
	if a, b := EncodeSettingPair(12, VehicleSetting{63, 5}); a != 0x4c || b != 0x7f {
		t.Errorf("encode = %02x %02x, want 4c 7f", a, b)
	}
	if _, _, ok := DecodeSettingPair(0x4c, 0x1f); ok {
		t.Error("value 5 accepted with a mask lacking option 5")
	}
	for _, id := range SettingIDs() {
		n := settingOptionCount(id)
		for value := 0; value < n; value++ {
			s := VehicleSetting{Mask: 1<<n - 1, Value: value}
			a, b := EncodeSettingPair(id, s)
			gotID, got, ok := DecodeSettingPair(a, b)
			if !ok || gotID != id || got != s {
				t.Errorf("id %d %+v round trip: %d %+v %v", id, s, gotID, got, ok)
			}
		}
	}
}

// Settings dumps from the car behind phev2mqtt's emulator (emulator/car.go).
func TestVehicleSettingsEmulatorDump(t *testing.T) {
	dump := []string{
		"023a003b003c0000", "02e201e301640e00", "02e501a61ea71e00", "026b0e2c002d0000",
		"022e006f00300000", "0231007206730600", "02b4003506360e00", "02370e3806390000",
		"023a003b003c0000", "024106420603fe00",
	}
	var v VehicleSettings
	for _, d := range dump {
		v.Update(mustHex(t, d))
	}
	available := 0
	for _, id := range SettingIDs() {
		if s, ok := v.Get(id); ok && SettingAvailable(id, s) {
			available++
			if l := SettingOptionLabel(id, s.Value, &s); l == "" {
				t.Errorf("id %d has an empty label", id)
			}
		}
	}
	if available == 0 {
		t.Error("no setting available from a real dump")
	}
	t.Logf("%d settings available from the emulator dump", available)
}

func TestVehicleSettingsArbitraryInput(t *testing.T) {
	var v VehicleSettings
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b += 3 {
			v.Update([]byte{2, byte(a), byte(b)})
		}
	}
	for _, id := range SettingIDs() {
		if s, ok := v.Get(id); ok {
			SettingOptionLabel(id, s.Value, &s)
			SettingOptions(id, s)
		}
	}
}

func TestVehicleSettingsSeed(t *testing.T) {
	var v VehicleSettings
	if !v.Seed(7, VehicleSetting{31, 1}) || v.Seed(7, VehicleSetting{31, 2}) {
		t.Error("seeding the same id twice")
	}
	if v.Known() {
		t.Error("seeded settings count as reported by the car")
	}
	// The car's own report wins over a seeded value.
	v.Update(mustHex(t, "02073e000000000000"))
	if s, _ := v.Get(7); s.Value != 0 {
		t.Errorf("id 7 = %+v after the car reported value 0", s)
	}
	if v.Seed(7, VehicleSetting{31, 3}) {
		t.Error("seed overwrote a reported setting")
	}
	if v.Seed(0, VehicleSetting{1, 0}) || v.Seed(61, VehicleSetting{1, 0}) {
		t.Error("seeded an id out of range")
	}
	all := v.All()
	all[7] = VehicleSetting{}
	if s, _ := v.Get(7); s.Mask != 31 {
		t.Error("All returned the internal map")
	}
}

func TestOptionByLabel(t *testing.T) {
	s := VehicleSetting{0b101001, 3}
	tests := []struct {
		label string
		want  int
		ok    bool
	}{
		{"Off", 0, true},
		{"On (type 2)", 3, true},
		{"On (type 3)", 5, true},
		{"On", 0, false},          // ambiguous for this car, so never offered
		{"On (type 1)", 0, false}, // option 2 is not in this car's mask
		{"Tail Lamp/Head Lamp: Cutout always", 0, false},
	}
	for _, test := range tests {
		if got, ok := OptionByLabel(12, s, test.label); got != test.want || ok != test.ok {
			t.Errorf("OptionByLabel(%q) = %d %v, want %d %v", test.label, got, ok, test.want, test.ok)
		}
	}
}
