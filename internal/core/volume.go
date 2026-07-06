package core

import "time"

// Outhaul volume labels. Every volume Outhaul creates for a single-container
// app carries all three, so the inventory can enumerate and attribute them.
const (
	VolumeLabelManaged = "outhaul.managed" // "true"
	VolumeLabelApp     = "outhaul.app"     // owning app name
	VolumeLabelRole    = "outhaul.role"    // VolumeRoleData
	VolumeRoleData     = "data"
)

// VolumeLabels is the label set stamped on an app's persistent volume at
// create time. Passing it to a label-matched ListVolumes returns exactly that
// app's data volumes.
func VolumeLabels(appName string) map[string]string {
	return map[string]string{
		VolumeLabelManaged: "true",
		VolumeLabelApp:     appName,
		VolumeLabelRole:    VolumeRoleData,
	}
}

// AppContainerPrefix is the fixed prefix of a single-container app's canonical
// Docker container name. Shared so deploy, restore, and the server agree on it.
const AppContainerPrefix = "outhaul-app-"

// AppContainerName is the canonical container name for a single-container app.
func AppContainerName(name string) string { return AppContainerPrefix + name }

// Volume is a persistent Docker named volume mounted into a single-container
// app at MountPath. Name is the Docker volume name, derived once at creation
// and immutable thereafter; MountPath is editable (it remounts the same
// volume elsewhere).
type Volume struct {
	ID        int64
	AppID     int64
	Name      string
	MountPath string
	CreatedAt time.Time
}

// VolumeListing is a Volume plus its owning app's identity, for the global tab.
type VolumeListing struct {
	Volume
	AppName string
	AppKind string
}
