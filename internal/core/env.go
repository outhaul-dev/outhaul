package core

// EnvVar is a single environment variable for an app. Value is plaintext at this
// layer; the store encrypts it at rest. Secret vars are injected into the running
// container only, never into the build (so they are not baked into an image layer).
type EnvVar struct {
	Key      string
	Value    string
	IsSecret bool
}
