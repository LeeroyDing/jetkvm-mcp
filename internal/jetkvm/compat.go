package jetkvm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Protocol/firmware compatibility evidence.
//
// This client was built against github.com/jetkvm/kvm, dev branch, commit
// b3c29a4 ("Set video codec before native stream start (#1509)"), cloned
// and read on 2026-08-02. Nothing here is a published, versioned API:
// jetkvm/kvm ships no protocol spec, only a Go implementation of both
// server and reference (browser) client. These constants exist so a
// handshake-shape drift produces an actionable error instead of a silent
// hang or confusing low-level failure. The reported version is informational;
// there is no trustworthy published firmware-version compatibility range.
const (
	// EvidenceCommit is the jetkvm/kvm commit this client's protocol
	// assumptions were read from.
	EvidenceCommit = "b3c29a4"

	// EvidenceBranch is the branch EvidenceCommit was read from.
	EvidenceBranch = "dev"
)

// CompatibilityError is returned when the device's handshake behavior
// doesn't match what this client was built to understand. It always
// includes what we expected and what we saw so a human (or agent) can
// decide whether to update this client or investigate the device.
type CompatibilityError struct {
	// Stage names the handshake step that failed, e.g.
	// "signaling-metadata" or "data-channel-open".
	Stage string
	// Detail explains the specific mismatch.
	Detail string
}

func (e *CompatibilityError) Error() string {
	return fmt.Sprintf(
		"jetkvm: firmware protocol compatibility check failed at %s: %s "+
			"(this client's protocol assumptions were read from jetkvm/kvm %s branch commit %s; "+
			"the device's behavior no longer matches - see README.md#firmware-compatibility)",
		e.Stage, e.Detail, EvidenceBranch, EvidenceCommit,
	)
}

// DeviceMetadata is the first message the signaling websocket sends,
// carrying the firmware's self-reported version string. Evidence:
// web.go's handleLocalWebRTCSignal, which writes
// {"type":"device-metadata","data":{"deviceVersion":builtAppVersion}}
// immediately after accepting the websocket, before any offer is sent.
type DeviceMetadata struct {
	DeviceVersion string `json:"deviceVersion"`
}

// checkDeviceMetadata validates the very first signaling message actually
// looks like the device-metadata message we expect, rather than assuming
// firmware compatibility and failing confusingly three steps later.
func checkDeviceMetadata(msgType string, raw []byte) (DeviceMetadata, error) {
	if msgType != "device-metadata" {
		return DeviceMetadata{}, &CompatibilityError{
			Stage:  "signaling-metadata",
			Detail: "the first signaling message had an unexpected type",
		}
	}
	var meta DeviceMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return DeviceMetadata{}, &CompatibilityError{
			Stage:  "signaling-metadata",
			Detail: "device-metadata payload did not decode as expected",
		}
	}
	if meta.DeviceVersion == "" {
		return DeviceMetadata{}, &CompatibilityError{
			Stage:  "signaling-metadata",
			Detail: "device-metadata payload had an empty deviceVersion field",
		}
	}
	if !validFirmwareVersion(meta.DeviceVersion) {
		return DeviceMetadata{}, &CompatibilityError{
			Stage:  "signaling-metadata",
			Detail: "device-metadata payload had an invalid deviceVersion field",
		}
	}
	return meta, nil
}

func validFirmwareVersion(version string) bool {
	if len(version) == 0 || len(version) > 96 || strings.ContainsAny(version, "\r\n\t") {
		return false
	}
	digitsAt := 0
	if version[0] == 'v' || version[0] == 'V' {
		digitsAt = 1
	}
	if digitsAt >= len(version) || version[digitsAt] < '0' || version[digitsAt] > '9' {
		return false
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
			continue
		}
		return false
	}
	return redactSensitive(version) == version
}

func safeDeviceIdentifier(deviceID string) string {
	if len(deviceID) == 0 || len(deviceID) > 128 || redactSensitive(deviceID) != deviceID {
		return redactionPlaceholder
	}
	for _, r := range deviceID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._:+-", r) {
			continue
		}
		return redactionPlaceholder
	}
	return deviceID
}
