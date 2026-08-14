package server

import "time"

// Metrics is the telemetry surface the server emits through. It is implemented
// by internal/telemetry and kept minimal so handlers stay testable. `route` is
// the template ("index", "kvp"); `path` carries the raw URL path so real key
// names appear in the labels.
type Metrics interface {
	Request(method, route, path, status string)
	RequestDuration(method, route, path string, d time.Duration)
	RequestInFlight(method, route, path string, delta int)
	DBQuery(operation string, d time.Duration)
	KeyStored()
	KeyExpired()
	Error(route, path, status string)
}

type noopMetrics struct{}

func (noopMetrics) Request(string, string, string, string)        {}
func (noopMetrics) RequestDuration(string, string, string, time.Duration) {}
func (noopMetrics) RequestInFlight(string, string, string, int)   {}
func (noopMetrics) DBQuery(string, time.Duration)                 {}
func (noopMetrics) KeyStored()                                    {}
func (noopMetrics) KeyExpired()                                   {}
func (noopMetrics) Error(string, string, string)                  {}
