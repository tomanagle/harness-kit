package session

import (
	"log/slog"

	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/settings"
)

// Logs returns the structured diagnostic stream. Subscribers own their
// subscriptions; diagnostics are separate from durable conversation events.
func (m *Manager) Logs() *logging.Stream { return m.opts.Logs }

// Logger routes host diagnostics through the same configurable slog stream.
// With/WithGroup can attach application, session, and turn attributes.
func (m *Manager) Logger() *slog.Logger { return m.log }

func (m *Manager) configureLogs(v settings.Logging) {
	m.opts.Logs.Configure(logging.Config{MaxQueueRecords: v.MaxQueueRecords, MaxQueueBytes: v.MaxQueueBytes, MaxRecordBytes: v.MaxRecordBytes})
}
