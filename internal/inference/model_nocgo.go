//go:build !cgo

package inference

import (
	"fmt"
	"spot-rl-controller/pkg/types"
)

// Model is a no-cgo placeholder that keeps the package buildable when cgo is disabled.
type Model struct{}

// SetSharedLibraryPath is a no-op in no-cgo builds.
func SetSharedLibraryPath(path string) {
	_ = path
}

// New returns a clear runtime error when cgo is disabled.
func New(modelPath string) (*Model, error) {
	_ = modelPath
	return nil, fmt.Errorf("onnx inference requires cgo; rebuild with CGO_ENABLED=1")
}

// Predict is unavailable without cgo and ONNX Runtime.
func (m *Model) Predict(state types.State, actionMask *[types.ActionDim]bool) (types.Action, types.DecodedAction, [types.ActionDim]float32, error) {
	_ = m
	_ = state
	_ = actionMask
	return 0, types.DecodedAction{}, [types.ActionDim]float32{}, fmt.Errorf("onnx inference requires cgo; rebuild with CGO_ENABLED=1")
}

// Close is a no-op in no-cgo builds.
func (m *Model) Close() error {
	_ = m
	return nil
}
