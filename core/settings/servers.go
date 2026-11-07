package settings

import (
	"fmt"
	"net/url"
)

// Servers configures remote coordination services. It stores no credentials and
// does not opt local sessions into uploading. Hosts choose credential storage and
// explicit account/tenant/application destinations through the remote client.
type Servers struct {
	Connections []ServerConnection `json:"connections"`
}

// SyncSettings applies on the next worker cycle, without restarting the sidecar.
// Registering a connection never opts sessions into publication.
type SyncSettings struct {
	Paused         bool `json:"paused"`
	PollIntervalMs int  `json:"pollIntervalMs"`
	BatchEvents    int  `json:"batchEvents"`
	WorkPerCycle   int  `json:"workPerCycle"`
}
type ServerConnection struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func validateSync(s SyncSettings) error {
	if s.PollIntervalMs < 100 || s.PollIntervalMs > 60000 || s.BatchEvents < 1 || s.BatchEvents > 100 || s.WorkPerCycle < 1 || s.WorkPerCycle > 100 {
		return fmt.Errorf("%w: sync interval must be 100..60000 ms, batch/work limits 1..100", ErrInvalid)
	}
	return nil
}

func validateServers(s Servers) error {
	seen := map[string]bool{}
	for _, c := range s.Connections {
		if len(c.ID) == 0 || len(c.ID) > 128 || seen[c.ID] {
			return fmt.Errorf("%w: server IDs must be nonempty and unique", ErrInvalid)
		}
		seen[c.ID] = true
		u, err := url.Parse(c.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
			return fmt.Errorf("%w: server URL must be an HTTPS origin or loopback HTTP", ErrInvalid)
		}
	}
	return nil
}
