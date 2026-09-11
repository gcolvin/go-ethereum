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

import "github.com/ethereum/go-ethereum/params"

// This file implements EIP-7979: Call and Return Opcodes for the EVM.
//
//   - CALLSUB   pops a destination from the data stack, pushes PC+1 to the
//     return stack, and transfers control to the destination, which must be
//     a CALLDEST.
//   - CALLDEST  marks a subroutine entry. Like JUMPDEST it is otherwise a
//     no-op, and it is additionally a valid JUMP/JUMPI destination, so a
//     jump may enter a subroutine without pushing a return address.
//   - RETURNSUB pops the return stack into the PC.
//
// The return stack is per call frame, like the data stack, and is not
// observable by EVM code. Its representation is not consensus-critical: we
// store PC+1 so that RETURNSUB can set the PC and let the interpreter loop's
// increment land on the instruction after the CALLSUB, exactly as opJump does.

// returnStackLimit is the maximum number of return addresses a frame may
// hold. A CALLSUB that would exceed it is an exceptional halt.
const returnStackLimit = 1024

// ReturnStack is the EIP-7979 return stack: return addresses pushed only by
// CALLSUB and popped only by RETURNSUB.
type ReturnStack struct {
	data []uint64
}

func newReturnStack() *ReturnStack {
	return &ReturnStack{data: make([]uint64, 0, 16)}
}

// push appends a return address, reporting false if the stack is full.
func (rs *ReturnStack) push(pc uint64) bool {
	if len(rs.data) >= returnStackLimit {
		return false
	}
	rs.data = append(rs.data, pc)
	return true
}

// pop removes and returns the most recent return address, reporting false if
// the stack is empty.
func (rs *ReturnStack) pop() (uint64, bool) {
	n := len(rs.data)
	if n == 0 {
		return 0, false
	}
	pc := rs.data[n-1]
	rs.data = rs.data[:n-1]
	return pc, true
}

// Data returns the return addresses, innermost last. Callers must not modify
// the contents of the returned slice.
func (rs *ReturnStack) Data() []uint64 {
	return rs.data
}

// opCallsub implements CALLSUB.
func opCallsub(pc *uint64, evm *EVM, scope *ScopeContext) ([]byte, error) {
	if evm.abort.Load() {
		return nil, errStopToken
	}
	pos := scope.Stack.pop1()
	if !scope.Contract.validCallDest(pos) {
		return nil, ErrInvalidCallDest
	}
	if scope.ReturnStack == nil {
		scope.ReturnStack = newReturnStack()
	}
	if !scope.ReturnStack.push(*pc + 1) {
		return nil, ErrReturnStackOverflow
	}
	*pc = pos.Uint64() - 1 // pc will be increased by the interpreter loop
	return nil, nil
}

// opCalldest implements CALLDEST. Like JUMPDEST, it is a no-op label.
func opCalldest(pc *uint64, evm *EVM, scope *ScopeContext) ([]byte, error) {
	return nil, nil
}

// opReturnsub implements RETURNSUB.
func opReturnsub(pc *uint64, evm *EVM, scope *ScopeContext) ([]byte, error) {
	if scope.ReturnStack == nil {
		return nil, ErrReturnStackUnderflow
	}
	ret, ok := scope.ReturnStack.pop()
	if !ok {
		return nil, ErrReturnStackUnderflow
	}
	*pc = ret - 1 // pc will be increased by the interpreter loop
	return nil, nil
}

// opJumpEIP7979 is opJump with CALLDEST accepted as a destination.
func opJumpEIP7979(pc *uint64, evm *EVM, scope *ScopeContext) ([]byte, error) {
	if evm.abort.Load() {
		return nil, errStopToken
	}
	pos := scope.Stack.pop1()
	if !scope.Contract.validJumpdestEIP7979(pos) {
		return nil, ErrInvalidJump
	}
	*pc = pos.Uint64() - 1 // pc will be increased by the interpreter loop
	return nil, nil
}

// opJumpiEIP7979 is opJumpi with CALLDEST accepted as a destination.
func opJumpiEIP7979(pc *uint64, evm *EVM, scope *ScopeContext) ([]byte, error) {
	if evm.abort.Load() {
		return nil, errStopToken
	}
	pos, cond := scope.Stack.pop2()
	if !cond.IsZero() {
		if !scope.Contract.validJumpdestEIP7979(pos) {
			return nil, ErrInvalidJump
		}
		*pc = pos.Uint64() - 1 // pc will be increased by the interpreter loop
	}
	return nil, nil
}

// enable7979 applies EIP-7979 (Call and Return Opcodes for the EVM) to the
// given jump table:
//   - adds CALLSUB (mid, 8 gas), CALLDEST (jumpdest, 1 gas) and RETURNSUB
//     (low, 5 gas);
//   - lets JUMP and JUMPI land on a CALLDEST.
//
// Existing opcodes keep their semantics; the change is purely additive.
func enable7979(jt *JumpTable) {
	jt[CALLSUB] = &operation{
		execute:     opCallsub,
		constantGas: GasMidStep,
		minStack:    minStack(1, 0),
		maxStack:    maxStack(1, 0),
	}
	jt[CALLDEST] = &operation{
		execute:     opCalldest,
		constantGas: params.JumpdestGas,
		minStack:    minStack(0, 0),
		maxStack:    maxStack(0, 0),
	}
	jt[RETURNSUB] = &operation{
		execute:     opReturnsub,
		constantGas: GasFastStep,
		minStack:    minStack(0, 0),
		maxStack:    maxStack(0, 0),
	}
	jt[JUMP].execute = opJumpEIP7979
	jt[JUMPI].execute = opJumpiEIP7979
}
