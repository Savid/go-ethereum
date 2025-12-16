// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package logger

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

type dummyStatedb struct {
	state.StateDB
}

func (*dummyStatedb) GetRefund() uint64                                    { return 1337 }
func (*dummyStatedb) GetState(_ common.Address, _ common.Hash) common.Hash { return common.Hash{} }
func (*dummyStatedb) SetState(_ common.Address, _ common.Hash, _ common.Hash) common.Hash {
	return common.Hash{}
}

func (*dummyStatedb) GetStateAndCommittedState(common.Address, common.Hash) (common.Hash, common.Hash) {
	return common.Hash{}, common.Hash{}
}

// mockOpContext implements tracing.OpContext for manual OnOpcode testing.
type mockOpContext struct{}

func (m *mockOpContext) MemoryData() []byte       { return nil }
func (m *mockOpContext) StackData() []uint256.Int { return nil }
func (m *mockOpContext) Caller() common.Address   { return common.Address{} }
func (m *mockOpContext) Address() common.Address  { return common.Address{} }
func (m *mockOpContext) CallValue() *uint256.Int  { return new(uint256.Int) }
func (m *mockOpContext) CallInput() []byte        { return nil }
func (m *mockOpContext) ContractCode() []byte     { return nil }

// testStructLog is used to parse JSON structLogs in tests.
type testStructLog struct {
	Pc      uint64 `json:"pc"`
	Op      string `json:"op"`
	Gas     uint64 `json:"gas"`
	GasCost uint64 `json:"gasCost"`
	Depth   int    `json:"depth"`
}

func TestStoreCapture(t *testing.T) {
	var (
		logger   = NewStructLogger(nil)
		evm      = vm.NewEVM(vm.BlockContext{}, &dummyStatedb{}, params.TestChainConfig, vm.Config{Tracer: logger.Hooks()})
		contract = vm.NewContract(common.Address{}, common.Address{}, new(uint256.Int), 100000, nil)
	)
	contract.Code = []byte{byte(vm.PUSH1), 0x1, byte(vm.PUSH1), 0x0, byte(vm.SSTORE)}
	var index common.Hash
	logger.OnTxStart(evm.GetVMContext(), nil, common.Address{})
	_, err := evm.Run(contract, []byte{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(logger.storage[contract.Address()]) == 0 {
		t.Fatalf("expected exactly 1 changed value on address %x, got %d", contract.Address(),
			len(logger.storage[contract.Address()]))
	}
	exp := common.BigToHash(big.NewInt(1))
	if logger.storage[contract.Address()][index] != exp {
		t.Errorf("expected %x, got %x", exp, logger.storage[contract.Address()][index])
	}
}

func TestComputeActualGasCost(t *testing.T) {
	t.Run("same_depth", func(t *testing.T) {
		// Create logger with ComputeActualGasCost enabled
		logger := NewStructLogger(&Config{ComputeActualGasCost: true})
		evm := vm.NewEVM(vm.BlockContext{}, &dummyStatedb{}, params.TestChainConfig, vm.Config{Tracer: logger.Hooks()})
		contract := vm.NewContract(common.Address{}, common.Address{}, new(uint256.Int), 100000, nil)

		// Simple bytecode: PUSH1 0x1, PUSH1 0x0, ADD
		contract.Code = []byte{byte(vm.PUSH1), 0x1, byte(vm.PUSH1), 0x0, byte(vm.ADD)}

		logger.OnTxStart(evm.GetVMContext(), nil, common.Address{})
		_, err := evm.Run(contract, []byte{}, false)
		if err != nil {
			t.Fatal(err)
		}

		result, err := logger.GetResult()
		if err != nil {
			t.Fatal(err)
		}

		var execResult ExecutionResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}

		// Verify we have logs
		if len(execResult.StructLogs) < 2 {
			t.Fatalf("expected at least 2 logs, got %d", len(execResult.StructLogs))
		}

		// Parse logs and verify gas costs are computed as diffs
		var logs []testStructLog
		for _, raw := range execResult.StructLogs {
			var log testStructLog
			if err := json.Unmarshal(raw, &log); err != nil {
				t.Fatal(err)
			}
			logs = append(logs, log)
		}

		// For all but the last log, gasCost should equal gas[n] - gas[n+1]
		for i := 0; i < len(logs)-1; i++ {
			expectedCost := logs[i].Gas - logs[i+1].Gas
			if logs[i].GasCost != expectedCost {
				t.Errorf("log[%d] (%s): expected gasCost %d, got %d",
					i, logs[i].Op, expectedCost, logs[i].GasCost)
			}
		}
	})

	t.Run("depth_change", func(t *testing.T) {
		// Manually simulate OnOpcode calls with depth changes
		logger := NewStructLogger(&Config{ComputeActualGasCost: true})
		logger.env = &tracing.VMContext{StateDB: &dummyStatedb{}}

		scope := &mockOpContext{}

		// Simulate: CALL at depth 1, child execution at depth 2, return to depth 1
		// depth=1, gas=100000: CALL opcode
		// depth=2, gas=63000:  child starts (PUSH)
		// depth=2, gas=60000:  child continues (STOP)
		// depth=1, gas=98000:  back to parent (PUSH)
		logger.OnOpcode(0, byte(vm.CALL), 100000, 100, scope, nil, 1, nil)
		logger.OnOpcode(0, byte(vm.PUSH1), 63000, 3, scope, nil, 2, nil)
		logger.OnOpcode(1, byte(vm.STOP), 60000, 0, scope, nil, 2, nil)
		logger.OnOpcode(1, byte(vm.PUSH1), 98000, 3, scope, nil, 1, nil)

		result, err := logger.GetResult()
		if err != nil {
			t.Fatal(err)
		}

		var execResult ExecutionResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}

		if len(execResult.StructLogs) != 4 {
			t.Fatalf("expected 4 logs, got %d", len(execResult.StructLogs))
		}

		// Parse all logs and find by opcode (order may vary due to streaming)
		var logs []testStructLog
		for _, raw := range execResult.StructLogs {
			var log testStructLog
			if err := json.Unmarshal(raw, &log); err != nil {
				t.Fatal(err)
			}
			logs = append(logs, log)
		}

		// Find the CALL log and verify its gasCost
		var callLog *testStructLog
		for i := range logs {
			if logs[i].Op == "CALL" && logs[i].Depth == 1 {
				callLog = &logs[i]
				break
			}
		}
		if callLog == nil {
			t.Fatal("CALL log not found")
		}

		// CALL at depth 1: gasCost should be 100000 - 98000 = 2000
		// (includes all child execution gas)
		if callLog.GasCost != 2000 {
			t.Errorf("CALL gasCost: expected 2000, got %d", callLog.GasCost)
		}

		// Find child opcodes at depth 2 and verify at least one has correct gas diff
		var childPushFound bool
		for _, log := range logs {
			if log.Depth == 2 && log.Op == "PUSH1" && log.GasCost == 3000 {
				childPushFound = true
				break
			}
		}
		if !childPushFound {
			t.Error("child PUSH with gasCost 3000 not found")
		}
	})

	t.Run("nested_calls", func(t *testing.T) {
		// Test 3 levels deep: depth 1 -> depth 2 -> depth 3 -> back
		logger := NewStructLogger(&Config{ComputeActualGasCost: true})
		logger.env = &tracing.VMContext{StateDB: &dummyStatedb{}}

		scope := &mockOpContext{}

		// Simulate nested calls:
		// depth=1, gas=100000: CALL (outer)
		// depth=2, gas=80000:  PUSH in first child
		// depth=2, gas=79000:  CALL (inner call)
		// depth=3, gas=50000:  PUSH in innermost child
		// depth=3, gas=49000:  STOP in innermost child
		// depth=2, gas=75000:  back to first child
		// depth=2, gas=74000:  STOP in first child
		// depth=1, gas=90000:  back to outer
		logger.OnOpcode(0, byte(vm.CALL), 100000, 100, scope, nil, 1, nil)
		logger.OnOpcode(0, byte(vm.PUSH1), 80000, 3, scope, nil, 2, nil)
		logger.OnOpcode(1, byte(vm.CALL), 79000, 100, scope, nil, 2, nil)
		logger.OnOpcode(0, byte(vm.PUSH1), 50000, 3, scope, nil, 3, nil)
		logger.OnOpcode(1, byte(vm.STOP), 49000, 0, scope, nil, 3, nil)
		logger.OnOpcode(2, byte(vm.PUSH1), 75000, 3, scope, nil, 2, nil)
		logger.OnOpcode(3, byte(vm.STOP), 74000, 0, scope, nil, 2, nil)
		logger.OnOpcode(1, byte(vm.PUSH1), 90000, 3, scope, nil, 1, nil)

		result, err := logger.GetResult()
		if err != nil {
			t.Fatal(err)
		}

		var execResult ExecutionResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}

		if len(execResult.StructLogs) != 8 {
			t.Fatalf("expected 8 logs, got %d", len(execResult.StructLogs))
		}

		// Parse all logs
		var logs []testStructLog
		for _, raw := range execResult.StructLogs {
			var log testStructLog
			if err := json.Unmarshal(raw, &log); err != nil {
				t.Fatal(err)
			}
			logs = append(logs, log)
		}

		// Find and verify outer CALL at depth 1
		// gasCost should be 100000 - 90000 = 10000 (includes all nested execution)
		var outerCall *testStructLog
		for i := range logs {
			if logs[i].Op == "CALL" && logs[i].Depth == 1 {
				outerCall = &logs[i]
				break
			}
		}
		if outerCall == nil {
			t.Fatal("outer CALL log not found")
		}
		if outerCall.GasCost != 10000 {
			t.Errorf("outer CALL gasCost: expected 10000, got %d", outerCall.GasCost)
		}

		// Find and verify inner CALL at depth 2
		// gasCost should be 79000 - 75000 = 4000 (includes innermost execution)
		var innerCall *testStructLog
		for i := range logs {
			if logs[i].Op == "CALL" && logs[i].Depth == 2 {
				innerCall = &logs[i]
				break
			}
		}
		if innerCall == nil {
			t.Fatal("inner CALL log not found")
		}
		if innerCall.GasCost != 4000 {
			t.Errorf("inner CALL gasCost: expected 4000, got %d", innerCall.GasCost)
		}

		// Verify innermost PUSH at depth 3 has correct gas diff
		// gasCost should be 50000 - 49000 = 1000
		var innermostPush *testStructLog
		for i := range logs {
			if logs[i].Op == "PUSH1" && logs[i].Depth == 3 {
				innermostPush = &logs[i]
				break
			}
		}
		if innermostPush == nil {
			t.Fatal("innermost PUSH log not found")
		}
		if innermostPush.GasCost != 1000 {
			t.Errorf("innermost PUSH gasCost: expected 1000, got %d", innermostPush.GasCost)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		// Default behavior - ComputeActualGasCost is false
		logger := NewStructLogger(nil)
		evm := vm.NewEVM(vm.BlockContext{}, &dummyStatedb{}, params.TestChainConfig, vm.Config{Tracer: logger.Hooks()})
		contract := vm.NewContract(common.Address{}, common.Address{}, new(uint256.Int), 100000, nil)

		contract.Code = []byte{byte(vm.PUSH1), 0x1, byte(vm.PUSH1), 0x0, byte(vm.ADD)}

		logger.OnTxStart(evm.GetVMContext(), nil, common.Address{})
		_, err := evm.Run(contract, []byte{}, false)
		if err != nil {
			t.Fatal(err)
		}

		result, err := logger.GetResult()
		if err != nil {
			t.Fatal(err)
		}

		var execResult ExecutionResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}

		// Parse logs
		var logs []testStructLog
		for _, raw := range execResult.StructLogs {
			var log testStructLog
			if err := json.Unmarshal(raw, &log); err != nil {
				t.Fatal(err)
			}
			logs = append(logs, log)
		}

		// With disabled flag, gasCost should be the pre-calculated cost, not the diff
		// For PUSH1, the pre-calculated cost is 3
		for _, log := range logs {
			if log.Op == "PUSH1" && log.GasCost != 3 {
				t.Errorf("PUSH1 with disabled flag: expected pre-calculated gasCost 3, got %d", log.GasCost)
			}
		}
	})

	t.Run("empty_logs", func(t *testing.T) {
		logger := NewStructLogger(&Config{ComputeActualGasCost: true})
		logger.env = &tracing.VMContext{StateDB: &dummyStatedb{}}

		// No opcodes executed, just get result
		result, err := logger.GetResult()
		if err != nil {
			t.Fatal(err)
		}

		var execResult ExecutionResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}

		if len(execResult.StructLogs) != 0 {
			t.Errorf("expected 0 logs, got %d", len(execResult.StructLogs))
		}
	})
}

// Tests that blank fields don't appear in logs when JSON marshalled, to reduce
// logs bloat and confusion. See https://github.com/ethereum/go-ethereum/issues/24487
func TestStructLogMarshalingOmitEmpty(t *testing.T) {
	tests := []struct {
		name string
		log  *StructLog
		want string
	}{
		{"empty err and no fields", &StructLog{},
			`{"pc":0,"op":0,"gas":"0x0","gasCost":"0x0","memSize":0,"stack":null,"depth":0,"refund":0,"opName":"STOP"}`},
		{"with err", &StructLog{Err: errors.New("this failed")},
			`{"pc":0,"op":0,"gas":"0x0","gasCost":"0x0","memSize":0,"stack":null,"depth":0,"refund":0,"opName":"STOP","error":"this failed"}`},
		{"with mem", &StructLog{Memory: make([]byte, 2), MemorySize: 2},
			`{"pc":0,"op":0,"gas":"0x0","gasCost":"0x0","memory":"0x0000","memSize":2,"stack":null,"depth":0,"refund":0,"opName":"STOP"}`},
		{"with 0-size mem", &StructLog{Memory: make([]byte, 0)},
			`{"pc":0,"op":0,"gas":"0x0","gasCost":"0x0","memSize":0,"stack":null,"depth":0,"refund":0,"opName":"STOP"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blob, err := json.Marshal(tt.log)
			if err != nil {
				t.Fatal(err)
			}
			if have, want := string(blob), tt.want; have != want {
				t.Fatalf("mismatched results\n\thave: %v\n\twant: %v", have, want)
			}
		})
	}
}
