//go:build cgo

package inference

import (
	"fmt"
	"spot-rl-controller/pkg/types"

	ort "github.com/yalue/onnxruntime_go"
)

// Model wraps ONNX Runtime session đề chạy DQN inference
type Model struct {
	session *ort.DynamicAdvancedSession
}

// SetSharedLibraryPath configures the ONNX Runtime shared library path.
func SetSharedLibraryPath(path string) {
	ort.SetSharedLibraryPath(path)
}

// New load file .onnx từ modelPath và khởi tạo ONNX Runtime
func New(modelPath string) (*Model, error) {
	// Khởi tạo ONNX Runtime environment (chỉ gọi 1 lần)
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("init onnxruntime: %w", err)
	}

	// Tên input/output phải khớp với export_model.py:
	inputNames := []string{"state"}
	outputNames := []string{"q_values"}

	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		inputNames,
		outputNames,
		nil,
	)

	if err != nil {
		return nil, fmt.Errorf("load model %s: %w", modelPath, err)
	}

	return &Model{session: session}, nil
}

// Predict nhận state, trả về action tốt nhất (argmax Q-values) và decoded action.
// Caller có thể truyền actionMask để chỉ argmax trên actions hợp lệ.
// Truyền nil để bỏ qua mask (chọn argmax raw).
func (m *Model) Predict(state types.State, actionMask *[types.ActionDim]bool) (types.Action, types.DecodedAction, [types.ActionDim]float32, error) {
	// 1. Chuyển state thành []float32 (flat vector [90])
	inputData := state.ToSlice()

	// 2. Tạo input tensor shape [1, 33] - batch_size=1
	inputShape := ort.NewShape(1, int64(types.StateDim))
	inputTensor, err := ort.NewTensor(inputShape, inputData)
	if err != nil {
		return 0, types.DecodedAction{}, [types.ActionDim]float32{}, fmt.Errorf("create input tensor: %w", err)
	}
	defer inputTensor.Destroy()

	// 3. Tạo output tensor shape [1, 105]
	outputShape := ort.NewShape(1, int64(types.ActionDim))
	outputData := make([]float32, types.ActionDim)
	outputTensor, err := ort.NewTensor(outputShape, outputData)
	if err != nil {
		return 0, types.DecodedAction{}, [types.ActionDim]float32{}, fmt.Errorf("create output tensor: %w", err)
	}
	defer outputTensor.Destroy()

	// 4. Chạy inference
	err = m.session.Run(
		[]ort.ArbitraryTensor{inputTensor},
		[]ort.ArbitraryTensor{outputTensor},
	)
	if err != nil {
		return 0, types.DecodedAction{}, [types.ActionDim]float32{}, fmt.Errorf("run inference: %w", err)
	}

	// 5. Đọc Q-values từ output tensor
	qValues := outputTensor.GetData()

	// 6. Argmax với action mask — bỏ qua actions không hợp lệ
	bestAction := -1
	for i := 0; i < types.ActionDim; i++ {
		if actionMask != nil && !actionMask[i] {
			continue
		}
		if bestAction == -1 || qValues[i] > qValues[bestAction] {
			bestAction = i
		}
	}
	// Fallback: nếu mask block hết (không nên xảy ra vì HOLD luôn valid)
	if bestAction == -1 {
		bestAction = types.HoldAction
	}

	// 7. Decode composite action → (op, typeIdx, azIdx)
	action := types.Action(bestAction)
	decoded := action.Decode()

	// Copy sang fixed-size array để trả về
	var qArr [types.ActionDim]float32
	copy(qArr[:], qValues)

	return action, decoded, qArr, nil
}

// Close releases ONNX Runtime resources.
func (m *Model) Close() error {
	if m == nil {
		return nil
	}
	if m.session != nil {
		m.session.Destroy()
		m.session = nil
	}
	ort.DestroyEnvironment()
	return nil
}
