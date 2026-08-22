package jetkvm

import (
	"encoding/json"
	"errors"
	"testing"
)

// FuzzCheckDeviceMetadata drives the pure compatibility parser with arbitrary
// signaling message types and JSON payloads. Acceptance is derived directly
// from the documented envelope contract rather than from the helper under
// test; every rejection must return no metadata and a typed compatibility
// error at the signaling-metadata stage.
func FuzzCheckDeviceMetadata(f *testing.F) {
	for _, seed := range []struct {
		messageType string
		payload     []byte
	}{
		{messageType: "device-metadata", payload: []byte(`{"deviceVersion":"0.5.8"}`)},
		{messageType: "device-metadata", payload: []byte(`{"deviceVersion":"dev","extra":true}`)},
		{messageType: "answer", payload: []byte(`{"deviceVersion":"0.5.8"}`)},
		{messageType: "device-metadata", payload: []byte(`{"deviceVersion":""}`)},
		{messageType: "device-metadata", payload: []byte(`{"deviceVersion":42}`)},
		{messageType: "device-metadata", payload: []byte(`{"deviceVersion":`)},
		{messageType: "device-metadata", payload: []byte(`null`)},
		{messageType: "", payload: nil},
		{messageType: "device-metadata", payload: []byte{0xff, 0xfe}},
	} {
		f.Add(seed.messageType, seed.payload)
	}

	f.Fuzz(func(t *testing.T, messageType string, payload []byte) {
		var want struct {
			DeviceVersion string `json:"deviceVersion"`
		}
		decodeErr := json.Unmarshal(payload, &want)
		wantValid := messageType == "device-metadata" && decodeErr == nil && want.DeviceVersion != ""

		got, err := checkDeviceMetadata(messageType, payload)
		if wantValid {
			if err != nil {
				t.Fatalf("checkDeviceMetadata rejected valid metadata: %v", err)
			}
			if got.DeviceVersion != want.DeviceVersion {
				t.Fatalf("DeviceVersion = %q, want %q", got.DeviceVersion, want.DeviceVersion)
			}
			return
		}

		if err == nil {
			t.Fatalf("checkDeviceMetadata accepted invalid type/payload: type=%q payload=%q", messageType, payload)
		}
		if got != (DeviceMetadata{}) {
			t.Fatalf("invalid metadata returned partial result: %+v", got)
		}
		var compatibilityErr *CompatibilityError
		if !errors.As(err, &compatibilityErr) {
			t.Fatalf("metadata error = %T %v, want *CompatibilityError", err, err)
		}
		if compatibilityErr.Stage != "signaling-metadata" {
			t.Fatalf("compatibility stage = %q, want signaling-metadata", compatibilityErr.Stage)
		}
	})
}
