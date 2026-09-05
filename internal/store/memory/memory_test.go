package memory

import (
	"testing"

	"kanban/internal/store"
	"kanban/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return New() })
}
