package serve

import "context"

// contentLine says what the content check found, for a report section; "" when there is nothing. Built in content.go.
func (s *Server) contentLine(ctx context.Context, lang string, in func(rule string) bool) (string, error) {
	return "", nil
}
