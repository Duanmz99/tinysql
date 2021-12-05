// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"strconv"

	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
)

// Vectorizable checks whether a list of expressions can employ vectorized execution.
func Vectorizable(exprs []Expression) bool {
	for _, expr := range exprs {
		if HasGetSetVarFunc(expr) {
			return false
		}
	}
	return true
}

// HasGetSetVarFunc checks whether an expression contains SetVar/GetVar function.
func HasGetSetVarFunc(expr Expression) bool {
	scalaFunc, ok := expr.(*ScalarFunction)
	if !ok {
		return false
	}
	if scalaFunc.FuncName.L == ast.SetVar {
		return true
	}
	if scalaFunc.FuncName.L == ast.GetVar {
		return true
	}
	for _, arg := range scalaFunc.GetArgs() {
		if HasGetSetVarFunc(arg) {
			return true
		}
	}
	return false
}

// HasAssignSetVarFunc checks whether an expression contains SetVar function and assign a value
// 避免将getvar和setvar的语句下推
func HasAssignSetVarFunc(expr Expression) bool {
	scalaFunc, ok := expr.(*ScalarFunction)
	if !ok {
		return false
	}
	if scalaFunc.FuncName.L == ast.SetVar {
		for _, arg := range scalaFunc.GetArgs() {
			if _, ok := arg.(*ScalarFunction); ok {
				return true
			}
		}
	}
	for _, arg := range scalaFunc.GetArgs() {
		if HasAssignSetVarFunc(arg) {
			return true
		}
	}
	return false
}

func evalOneVec(ctx sessionctx.Context, expr Expression, input *chunk.Chunk, output *chunk.Chunk, colIdx int) error {
	ft := expr.GetType()
	result := output.Column(colIdx)
	switch ft.EvalType() {
	case types.ETInt:
		if err := expr.VecEvalInt(ctx, input, result); err != nil {
			return err
		}
	case types.ETReal:
		if err := expr.VecEvalReal(ctx, input, result); err != nil {
			return err
		}
		if ft.Tp == mysql.TypeFloat {
			f64s := result.Float64s()
			n := input.NumRows()
			buf := chunk.NewColumn(ft, n)
			buf.ResizeFloat32(n, false)
			f32s := buf.Float32s()
			for i := range f64s {
				if result.IsNull(i) {
					buf.SetNull(i, true)
				} else {
					f32s[i] = float32(f64s[i])
				}
			}
			output.SetCol(colIdx, buf)
		}
	case types.ETString:
		if err := expr.VecEvalString(ctx, input, result); err != nil {
			return err
		}
	}
	return nil
}

func evalOneColumn(ctx sessionctx.Context, expr Expression, iterator *chunk.Iterator4Chunk, output *chunk.Chunk, colID int) (err error) {
	switch fieldType, evalType := expr.GetType(), expr.GetType().EvalType(); evalType {
	case types.ETInt:
		for row := iterator.Begin(); err == nil && row != iterator.End(); row = iterator.Next() {
			err = executeToInt(ctx, expr, fieldType, row, output, colID)
		}
	case types.ETReal:
		for row := iterator.Begin(); err == nil && row != iterator.End(); row = iterator.Next() {
			err = executeToReal(ctx, expr, fieldType, row, output, colID)
		}
	case types.ETString:
		for row := iterator.Begin(); err == nil && row != iterator.End(); row = iterator.Next() {
			err = executeToString(ctx, expr, fieldType, row, output, colID)
		}
	}
	return err
}

func evalOneCell(ctx sessionctx.Context, expr Expression, row chunk.Row, output *chunk.Chunk, colID int) (err error) {
	switch fieldType, evalType := expr.GetType(), expr.GetType().EvalType(); evalType {
	case types.ETInt:
		err = executeToInt(ctx, expr, fieldType, row, output, colID)
	case types.ETReal:
		err = executeToReal(ctx, expr, fieldType, row, output, colID)
	case types.ETString:
		err = executeToString(ctx, expr, fieldType, row, output, colID)
	}
	return err
}

func executeToInt(ctx sessionctx.Context, expr Expression, fieldType *types.FieldType, row chunk.Row, output *chunk.Chunk, colID int) error {
	res, isNull, err := expr.EvalInt(ctx, row)
	if err != nil {
		return err
	}
	if isNull {
		output.AppendNull(colID)
		return nil
	}
	if fieldType.Tp == mysql.TypeBit {
		output.AppendBytes(colID, strconv.AppendUint(make([]byte, 0, 8), uint64(res), 10))
		return nil
	}
	if mysql.HasUnsignedFlag(fieldType.Flag) {
		output.AppendUint64(colID, uint64(res))
		return nil
	}
	output.AppendInt64(colID, res)
	return nil
}

func executeToReal(ctx sessionctx.Context, expr Expression, fieldType *types.FieldType, row chunk.Row, output *chunk.Chunk, colID int) error {
	res, isNull, err := expr.EvalReal(ctx, row)
	if err != nil {
		return err
	}
	if isNull {
		output.AppendNull(colID)
		return nil
	}
	if fieldType.Tp == mysql.TypeFloat {
		output.AppendFloat32(colID, float32(res))
		return nil
	}
	output.AppendFloat64(colID, res)
	return nil
}

func executeToString(ctx sessionctx.Context, expr Expression, fieldType *types.FieldType, row chunk.Row, output *chunk.Chunk, colID int) error {
	res, isNull, err := expr.EvalString(ctx, row)
	if err != nil {
		return err
	}
	if isNull {
		output.AppendNull(colID)
	} else {
		output.AppendString(colID, res)
	}
	return nil
}

// VectorizedFilter applies a list of filters to a Chunk and
// returns a bool slice, which indicates whether a row is passed the filters.
// Filters is executed vectorized.
func VectorizedFilter(ctx sessionctx.Context, filters []Expression, iterator *chunk.Iterator4Chunk, selected []bool) (_ []bool, err error) {
	selected, _, err = VectorizedFilterConsiderNull(ctx, filters, iterator, selected, nil)
	return selected, err
}

// VectorizedFilterConsiderNull applies a list of filters to a Chunk and
// returns two bool slices, `selected` indicates whether a row passed the
// filters, `isNull` indicates whether the result of the filter is null.
// Filters is executed vectorized.
func VectorizedFilterConsiderNull(ctx sessionctx.Context, filters []Expression, iterator *chunk.Iterator4Chunk, selected []bool, isNull []bool) ([]bool, []bool, error) {
	// canVectorized used to check whether all of the filters can be vectorized evaluated
	canVectorized := true
	for _, filter := range filters {
		if !filter.Vectorized() {
			canVectorized = false
			break
		}
	}

	input := iterator.GetChunk()
	sel := input.Sel()
	var err error
	if canVectorized && ctx.GetSessionVars().EnableVectorizedExpression {
		selected, isNull, err = vectorizedFilter(ctx, filters, iterator, selected, isNull)
	} else {
		selected, isNull, err = rowBasedFilter(ctx, filters, iterator, selected, isNull)
	}
	if err != nil || sel == nil {
		return selected, isNull, err
	}

	// When the input.Sel() != nil, we need to handle the selected slice and input.Sel()
	// Get the index which is not appeared in input.Sel() and set the selected[index] = false
	selectedLength := len(selected)
	unselected := allocZeroSlice(selectedLength)
	defer deallocateZeroSlice(unselected)
	// unselected[i] == 1 means that the i-th row is not selected
	for i := 0; i < selectedLength; i++ {
		unselected[i] = 1
	}
	for _, ind := range sel {
		unselected[ind] = 0
	}
	for i := 0; i < selectedLength; i++ {
		if selected[i] && unselected[i] == 1 {
			selected[i] = false
		}
	}
	return selected, isNull, err
}

// rowBasedFilter filters by row.
func rowBasedFilter(ctx sessionctx.Context, filters []Expression, iterator *chunk.Iterator4Chunk, selected []bool, isNull []bool) ([]bool, []bool, error) {
	// If input.Sel() != nil, we will call input.SetSel(nil) to clear the sel slice in input chunk.
	// After the function finished, then we reset the sel in input chunk.
	// Then the caller will handle the input.sel and selected slices.
	input := iterator.GetChunk()
	if input.Sel() != nil {
		defer input.SetSel(input.Sel())
		input.SetSel(nil)
		iterator = chunk.NewIterator4Chunk(input)
	}

	selected = selected[:0]
	for i, numRows := 0, iterator.Len(); i < numRows; i++ {
		selected = append(selected, true)
	}
	if isNull != nil {
		isNull = isNull[:0]
		for i, numRows := 0, iterator.Len(); i < numRows; i++ {
			isNull = append(isNull, false)
		}
	}
	var (
		filterResult       int64
		bVal, isNullResult bool
		err                error
	)
	for _, filter := range filters {
		isIntType := true
		if filter.GetType().EvalType() != types.ETInt {
			isIntType = false
		}
		for row := iterator.Begin(); row != iterator.End(); row = iterator.Next() {
			if !selected[row.Idx()] {
				continue
			}
			if isIntType {
				filterResult, isNullResult, err = filter.EvalInt(ctx, row)
				if err != nil {
					return nil, nil, err
				}
				selected[row.Idx()] = selected[row.Idx()] && !isNullResult && (filterResult != 0)
			} else {
				// TODO: should rewrite the filter to `cast(expr as SIGNED) != 0` and always use `EvalInt`.
				bVal, isNullResult, err = EvalBool(ctx, []Expression{filter}, row)
				if err != nil {
					return nil, nil, err
				}
				selected[row.Idx()] = selected[row.Idx()] && bVal
			}
			if isNull != nil {
				isNull[row.Idx()] = isNull[row.Idx()] || isNullResult
			}
		}
	}
	return selected, isNull, nil
}

// vectorizedFilter filters by vector.
func vectorizedFilter(ctx sessionctx.Context, filters []Expression, iterator *chunk.Iterator4Chunk, selected []bool, isNull []bool) ([]bool, []bool, error) {
	selected, isNull, err := VecEvalBool(ctx, filters, iterator.GetChunk(), selected, isNull)
	if err != nil {
		return nil, nil, err
	}

	return selected, isNull, nil
}
