package session

// matrix/hardened: settings for the World Store task board integration
// (internal/worldstore). Nothing here is enabled by default and nothing in it
// talks to the network: the bridge is personal-panel's local node entry
// (scripts/worldstore-ai.cjs), which talks to the headless World Store data
// service on this machine.

// WorldStoreSettings is the [worldstore] section of config.toml.
type WorldStoreSettings struct {
	// Bridge is the absolute path to personal-panel's
	// scripts/worldstore-ai.cjs. Empty disables the task board.
	Bridge string `toml:"bridge,omitempty"`

	// Node is the node binary used to run the bridge (default: "node" on PATH).
	Node string `toml:"node,omitempty"`

	// DeviceID names this machine in task ↔ session links (default: hostname
	// without ".local"). Changing it orphans links recorded under the old name.
	DeviceID string `toml:"device_id,omitempty"`
}
