// Copyright 2026 The go-ethereum Authors
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

package vm

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// run7979 executes code in a fresh state with the given extra EIPs enabled and
// returns the return data, the gas used and the error. Placeholder opcode
// values follow the EIP: 0xB0=CALLSUB, 0xB1=CALLDEST, 0xB2=RETURNSUB.
func run7979(t *testing.T, code []byte, gasLimit uint64, extraEips []int, setup func(db *state.StateDB)) ([]byte, uint64, error) {
	t.Helper()
	address := common.BytesToAddress([]byte("contract"))
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb.CreateAccount(address)
	statedb.SetCode(address, code, tracing.CodeChangeUnspecified)
	if setup != nil {
		setup(statedb)
	}
	statedb.Finalise(params.Rules{IsEIP158: true})

	vmctx := BlockContext{
		CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int, *params.Rules) {},
	}
	evm := NewEVM(vmctx, statedb, params.AllEthashProtocolChanges, Config{ExtraEips: extraEips})
	initialGas := NewGasBudget(gasLimit, 0)
	ret, result, err := evm.Call(common.Address{}, address, nil, initialGas, new(uint256.Int))
	return ret, result.Used(initialGas), err
}

// TestEIP7979 runs the test cases of the EIP, plus edge cases around
// destinations, the return stack, and call elimination via JUMP.
func TestEIP7979(t *testing.T) {
	const gasLimit = 1_000_000
	tests := []struct {
		name    string
		code    string
		failure error  // expected error (errors.Is), nil for success
		used    uint64 // expected gas used; ignored when failure != nil
	}{
		// The five test cases of EIP-7979.
		{"simple routine", "0x6004B000B1B2", nil, 17},
		{"two levels of subroutines", "0x6004B000B16009B0B2B1B2", nil, 34},
		{"invalid destination", "0x60FFB000B1B2", ErrInvalidCallDest, 0},
		{"empty return stack", "0xB2", ErrReturnStackUnderflow, 0},
		{"subroutine at end of code", "0x600556B1B25B6003B0", nil, 29},

		// Destinations.
		{"CALLSUB to a JUMPDEST", "0x6004B0005B", ErrInvalidCallDest, 0},
		{"CALLSUB into PUSH data", "0x6005B00060B100", ErrInvalidCallDest, 0},
		{"CALLSUB out of range", "0x6008B000B1B2", ErrInvalidCallDest, 0},
		{"CALLSUB destination is also an invalid jump", "0x60FFB000B1B2", ErrInvalidJump, 0},

		// CALLDEST is a valid JUMP/JUMPI destination (call elimination).
		{"JUMP to a CALLDEST", "0x60045600B100", nil, 12},
		{"JUMPI to a CALLDEST", "0x600160065700B100", nil, 17},
		{"JUMP to a CALLDEST, then RETURNSUB with no caller", "0x60045600B1B2", ErrReturnStackUnderflow, 0},
		{"tail call: subroutine jumps to another, one return", "0x6004B000B1600856B1B2", nil, 29},

		// CALLDEST as a plain label.
		{"CALLDEST reached by fall-through", "0xB100", nil, 1},
		{"fall into subroutine, then RETURNSUB", "0xB1B2", ErrReturnStackUnderflow, 0},

		// Return stack depth: recursion with no base case halts at 1024.
		{"recursion overflows the return stack", "0x6004B000B16004B0B2", ErrReturnStackOverflow, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := hexutil.MustDecode(tt.code)
			ret, used, err := run7979(t, code, gasLimit, []int{7979}, nil)
			if tt.failure != nil {
				if !errors.Is(err, tt.failure) {
					t.Fatalf("error mismatch: have %v, want %v", err, tt.failure)
				}
				if used != gasLimit {
					t.Fatalf("exceptional halt should consume all gas: used %d of %d", used, gasLimit)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if used != tt.used {
				t.Fatalf("gas used mismatch: have %d, want %d", used, tt.used)
			}
			if len(ret) != 0 {
				t.Fatalf("unexpected return data: %x", ret)
			}
		})
	}
}

// TestEIP7979OutOfGas checks that running out of gas inside a subroutine is
// an ordinary out-of-gas halt.
func TestEIP7979OutOfGas(t *testing.T) {
	// PUSH1 (3) + CALLSUB (8) + CALLDEST (1) = 12; RETURNSUB (5) needs 17.
	code := hexutil.MustDecode("0x6004B000B1B2")
	_, used, err := run7979(t, code, 15, []int{7979}, nil)
	if !errors.Is(err, ErrOutOfGas) {
		t.Fatalf("error mismatch: have %v, want %v", err, ErrOutOfGas)
	}
	if used != 15 {
		t.Fatalf("out of gas should consume all gas: used %d", used)
	}
}

// TestEIP7979ReturnStackIsPerFrame checks that the return stack belongs to
// the message frame: a nested call starts with an empty return stack, and the
// caller's return stack survives the nested call.
func TestEIP7979ReturnStackIsPerFrame(t *testing.T) {
	// Caller:
	//   00: PUSH1 0x0b       subroutine address
	//   02: CALLSUB
	//   03: PUSH1 0x00
	//   05: MSTORE           store the STATICCALL success flag at 0
	//   06: PUSH1 0x20
	//   08: PUSH1 0x00
	//   0a: RETURN           return it
	//   0b: CALLDEST
	//   0c: PUSH1 0x00       retSize
	//   0e: PUSH1 0x00       retOffset
	//   10: PUSH1 0x00       argsSize
	//   12: PUSH1 0x00       argsOffset
	//   14: PUSH1 0xbb       callee address
	//   16: GAS
	//   17: STATICCALL
	//   18: RETURNSUB
	caller := hexutil.MustDecode("0x600bB060005260206000F3B1600060006000600060bb5AFAB2")
	callee := common.BytesToAddress([]byte{0xbb})

	tests := []struct {
		name       string
		calleeCode string
		success    byte // expected STATICCALL success flag
	}{
		// A bare RETURNSUB in the callee underflows the callee's (empty)
		// return stack even though the caller's holds a return address.
		{"callee underflows its own return stack", "0xB2", 0},
		// A well-formed callee subroutine succeeds; the caller's return
		// address is intact afterwards either way.
		{"callee uses its own return stack", "0x6004B000B1B2", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := func(db *state.StateDB) {
				db.CreateAccount(callee)
				db.SetCode(callee, hexutil.MustDecode(tt.calleeCode), tracing.CodeChangeUnspecified)
			}
			ret, _, err := run7979(t, caller, 1_000_000, []int{7979}, setup)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := make([]byte, 32)
			want[31] = tt.success
			if !bytes.Equal(ret, want) {
				t.Fatalf("return data mismatch: have %x, want %x", ret, want)
			}
		})
	}
}

// TestEIP7979Inactive checks that without the EIP the new opcodes are
// undefined and a CALLDEST byte is not a valid jump destination, i.e. that
// legacy semantics are untouched.
func TestEIP7979Inactive(t *testing.T) {
	// CALLSUB is an undefined opcode.
	_, _, err := run7979(t, hexutil.MustDecode("0x6004B000B1B2"), 1_000_000, nil, nil)
	var invalidOp *ErrInvalidOpCode
	if !errors.As(err, &invalidOp) {
		t.Fatalf("error mismatch: have %v, want invalid opcode", err)
	}
	// A JUMP to a 0xB1 byte is an invalid jump.
	_, _, err = run7979(t, hexutil.MustDecode("0x60045600B100"), 1_000_000, nil, nil)
	if !errors.Is(err, ErrInvalidJump) {
		t.Fatalf("error mismatch: have %v, want %v", err, ErrInvalidJump)
	}
}
