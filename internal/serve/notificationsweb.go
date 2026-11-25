package serve

import (
	"net/http"
)

// The Notifications page, /notifications, under Monitoring, where lapilli
// serve works out the notifications (docs/design/routing.md, "In lapilli
// serve: worked out beside what was sent"): for each receiver, how many
// notifications the alert manager's configuration was worked out to send
// and how many were really recorded, over the hours that have both, how far
// the two agree, and, for the receivers none was recorded for, what was
// worked out.

func (s *Server) notificationsRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /notifications", page("notifications", s.notificationsPage))
}

func (s *Server) notificationsPage(r *http.Request) (any, error) {
	return s.notificationsNow(r.Context())
}
