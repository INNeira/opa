// Copyright 2020 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/bytecodealliance/wasmtime-go"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/metrics"
	"github.com/open-policy-agent/opa/topdown"
	"github.com/open-policy-agent/opa/topdown/builtins"
)

func opaFunctions(dispatcher *builtinDispatcher, store *wasmtime.Store) []*wasmtime.Extern {

	i32 := wasmtime.NewValType(wasmtime.KindI32)

	externs := []*wasmtime.Extern{
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32}, nil), opaAbort).AsExtern(),
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32}, []*wasmtime.ValType{i32}), dispatcher.Call).AsExtern(),
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32, i32}, []*wasmtime.ValType{i32}), dispatcher.Call).AsExtern(),
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32, i32, i32}, []*wasmtime.ValType{i32}), dispatcher.Call).AsExtern(),
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32, i32, i32, i32}, []*wasmtime.ValType{i32}), dispatcher.Call).AsExtern(),
		wasmtime.NewFunc(store, wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32, i32, i32, i32, i32}, []*wasmtime.ValType{i32}), dispatcher.Call).AsExtern(),
	}

	return externs
}

func opaAbort(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {

	data := caller.GetExport("memory").Memory().UnsafeData()[args[0].I32():]

	n := bytes.IndexByte(data, 0)
	if n == -1 {
		panic("invalid abort argument")
	}

	panic(abortError{message: string(data[:n])})
}

type builtinDispatcher struct {
	ctx      *topdown.BuiltinContext
	builtins map[int32]topdown.BuiltinFunc
	result   *ast.Term
}

func newBuiltinDispatcher() *builtinDispatcher {
	return &builtinDispatcher{}
}

func (d *builtinDispatcher) SetMap(m map[int32]topdown.BuiltinFunc) {
	d.builtins = m
}

func (d *builtinDispatcher) Reset(ctx context.Context) {
	d.ctx = &topdown.BuiltinContext{
		Context:  ctx,
		Cancel:   nil,
		Runtime:  nil,
		Time:     ast.NumberTerm(json.Number(strconv.FormatInt(time.Now().UnixNano(), 10))),
		Metrics:  metrics.New(),
		Cache:    make(builtins.Cache),
		Location: nil,
		Tracers:  nil,
		QueryID:  0,
		ParentID: 0,
	}

}

func (d *builtinDispatcher) Call(caller *wasmtime.Caller, args []wasmtime.Val) (result []wasmtime.Val, trap *wasmtime.Trap) {

	if d.ctx == nil {
		panic("unreachable: uninitialized built-in dispatcher context")
	}

	if d.builtins == nil {
		panic("unreachable: uninitialized built-in dispatcher index")
	}

	exports := getExports(caller)

	var convertedArgs []*ast.Term

	// first two args are the built-in identifier and context structure
	for i := 2; i < len(args); i++ {

		x, err := fromWasmValue(exports, args[i].I32())
		if err != nil {
			panic(builtinError{err: err})
		}

		convertedArgs = append(convertedArgs, x)
	}

	var output *ast.Term

	if err := d.builtins[args[0].I32()](*d.ctx, convertedArgs, func(t *ast.Term) error {
		output = t
		return nil
	}); err != nil {
		panic(builtinError{err: err})
	}

	// if output is undefined, return NULL
	if output == nil {
		return []wasmtime.Val{wasmtime.ValI32(0)}, nil
	}

	addr, err := toWasmValue(exports, output)
	if err != nil {
		panic(builtinError{err: err})
	}

	return []wasmtime.Val{wasmtime.ValI32(addr)}, nil
}

type exports struct {
	Memory       *wasmtime.Memory
	mallocFn     *wasmtime.Func
	valueDumpFn  *wasmtime.Func
	valueParseFn *wasmtime.Func
}

func getExports(c *wasmtime.Caller) exports {
	var e exports
	e.Memory = c.GetExport("memory").Memory()
	e.mallocFn = c.GetExport("opa_malloc").Func()
	e.valueDumpFn = c.GetExport("opa_value_dump").Func()
	e.valueParseFn = c.GetExport("opa_value_parse").Func()
	return e
}

func (e exports) Malloc(len int32) (int32, error) {
	ptr, err := e.mallocFn.Call(len)
	if err != nil {
		return 0, err
	}
	return ptr.(int32), nil
}

func (e exports) ValueDump(addr int32) (int32, error) {
	result, err := e.valueDumpFn.Call(addr)
	if err != nil {
		return 0, err
	}
	return result.(int32), nil
}

func (e exports) ValueParse(addr int32, len int32) (int32, error) {
	result, err := e.valueParseFn.Call(addr, len)
	if err != nil {
		return 0, err
	}
	return result.(int32), nil
}

func fromWasmValue(e exports, addr int32) (*ast.Term, error) {

	serialized, err := e.ValueDump(addr)
	if err != nil {
		return nil, err
	}

	data := e.Memory.UnsafeData()[serialized:]
	n := bytes.IndexByte(data, 0)
	if n < 0 {
		return nil, errors.New("invalid serialized value address")
	}

	return ast.ParseTerm(string(data[0:n]))
}

func toWasmValue(e exports, term *ast.Term) (int32, error) {

	raw := []byte(term.String())
	n := int32(len(raw))
	p, err := e.Malloc(n)
	if err != nil {
		return 0, err
	}

	copy(e.Memory.UnsafeData()[p:p+n], raw)
	addr, err := e.ValueParse(p, n)
	if err != nil {
		return 0, err
	}

	return addr, nil
}
