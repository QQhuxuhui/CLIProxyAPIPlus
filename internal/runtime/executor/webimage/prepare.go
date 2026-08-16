package webimage

import "github.com/google/uuid"

func newTurnTraceID() string {
	return uuid.NewString()
}
