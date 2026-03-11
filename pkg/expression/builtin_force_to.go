// Copyright 2026 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"math"
	"strings"

	"github.com/pingcap/tidb/pkg/errno"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/dbterror"
)

type builtinForceToIntSig struct {
	baseBuiltinFunc

	rounder         Expression
	targetFieldType types.FieldType
}

func (b *builtinForceToIntSig) cloneFrom(from *builtinForceToIntSig) {
	b.baseBuiltinFunc.cloneFrom(&from.baseBuiltinFunc)
	b.targetFieldType = *from.targetFieldType.Clone()
	if from.rounder != nil {
		b.rounder = from.rounder.Clone().(*Constant)
	}
}

func (b *builtinForceToIntSig) Clone() builtinFunc {
	newSig := &builtinForceToIntSig{}
	newSig.cloneFrom(b)
	return newSig
}

var ErrInvalidType = dbterror.ClassServer.NewStd(errno.ErrInvalidType)

func toOverUnderErr(dt types.Datum) error {
	// Judge it is inf or -inf
	// For int:
	//			inf:  01111111 & 1 == 1
	//		   -inf:  10000000 & 1 == 0
	// For uint:
	//			inf:  11111111 & 1 == 1
	//		   -inf:  00000000 & 1 == 0
	if dt.GetInt64()&1 == 1 {
		return types.ErrDataOverflow
	} else {
		return types.ErrDataUnderflow
	}
}

func (b *builtinForceToIntSig) evalInt(evalCtx EvalContext, row chunk.Row) (int64, bool, error) {
	con := b.args[0]
	conET := con.GetType(evalCtx).EvalType()
	dt, err := con.Eval(evalCtx, row)
	if err != nil {
		return dt.GetInt64(), dt.IsNull(), err
	}
	if b.targetFieldType.GetType() == mysql.TypeBit {
		b.targetFieldType = *types.NewFieldType(mysql.TypeLonglong)
	}

	// hack it to work
	if conET == types.ETString && !dt.IsNull() && strings.TrimSpace(dt.GetString()) == "" {
		return 0, false, nil
	}

	oriTypeCtx := evalCtx.TypeCtx()
	// Disable AllowNegativeToUnsigned to make sure return 0 when underflow happens.
	newTypeCtx := oriTypeCtx.WithFlags(oriTypeCtx.Flags().WithAllowNegativeToUnsigned(false))
	intDatum, err := dt.ConvertTo(newTypeCtx, &b.targetFieldType)
	if err != nil {
		if terror.ErrorEqual(err, types.ErrDataOutOfRange) {
			return intDatum.GetInt64(), intDatum.IsNull(), toOverUnderErr(intDatum)
		}
		return dt.GetInt64(), dt.IsNull(), err
	}

	if conET == types.ETString && strings.TrimSpace(dt.GetString()) == "" {
		return intDatum.GetInt64(), intDatum.IsNull(), nil
	}

	c, err := intDatum.Compare(evalCtx.TypeCtx(), &dt, collate.GetBinaryCollator())
	if err != nil {
		return dt.GetInt64(), dt.IsNull(), err
	}
	if c == 0 {
		return intDatum.GetInt64(), intDatum.IsNull(), nil
	}

	if b.rounder != nil {
		intDatum2, err := b.rounder.Eval(evalCtx, row)
		if err != nil {
			return dt.GetInt64(), dt.IsNull(), nil // just go with original value?
		}

		if b.rounder.GetType(evalCtx).EvalType() == types.ETInt {
			return intDatum2.GetInt64(), intDatum2.IsNull(), nil
		}

		intDatum2, err = intDatum2.ConvertTo(evalCtx.TypeCtx(), &b.targetFieldType)
		if err != nil {
			if terror.ErrorEqual(err, types.ErrDataOutOfRange) {
				return intDatum2.GetInt64(), intDatum2.IsNull(), toOverUnderErr(intDatum2)
			}
			return dt.GetInt64(), dt.IsNull(), nil // just go with original value?
		}

		return intDatum2.GetInt64(), intDatum2.IsNull(), nil
	} else {
		// this is only for eq
		switch conET {
		// An integer value equal or NULL-safe equal to a float value which contains
		// non-zero decimal digits is definitely false.
		// e.g.,
		//   1. "integer  =  1.1" is definitely false.
		//   2. "integer <=> 1.1" is definitely false.
		case types.ETReal, types.ETDecimal:
			//return dt.GetInt64(), dt.IsNull(), nil // just go with original value?
			return dt.GetInt64(), dt.IsNull(), types.ErrDataOverflow.GenWithStack("Conversion to int failed.")
		case types.ETString:
			// We try to convert the string constant to double.
			// If the double result equals the int result, we can return the int result;
			// otherwise, the compare function will be false.
			// **note**
			// 1. We compare `doubleDatum` with the `integral part of doubleDatum` rather then intDatum to handle the
			//    case when `targetFieldType.GetType()` is `TypeYear`.
			// 2. When `targetFieldType.GetType()` is `TypeYear`, we can not compare `doubleDatum` with `intDatum` directly,
			//    because we'll convert values in the ranges '0' to '69' and '70' to '99' to YEAR values in the ranges
			//    2000 to 2069 and 1970 to 1999.
			// 3. Suppose the value of `con` is 2, when `targetFieldType.GetType()` is `TypeYear`, the value of `doubleDatum`
			//    will be 2.0 and the value of `intDatum` will be 2002 in this case.
			var doubleDatum types.Datum
			doubleDatum, err = dt.ConvertTo(evalCtx.TypeCtx(), types.NewFieldType(mysql.TypeDouble))
			if err != nil {
				return dt.GetInt64(), dt.IsNull(), nil // just go with original value?
			}
			if doubleDatum.GetFloat64() != math.Trunc(doubleDatum.GetFloat64()) {
				// This is not the correct error, but just hack it for now.
				return dt.GetInt64(), dt.IsNull(), types.ErrDataOverflow.GenWithStack("Conversion to int failed.")
			}
			return intDatum.GetInt64(), intDatum.IsNull(), nil
		}
	}

	return dt.GetInt64(), dt.IsNull(), nil // just go with original value?
}

func getForceToInt(ctx BuildContext, targetFieldType types.FieldType, expr Expression, rounder Expression) (Expression, error) {
	//fmt.Println("fudge2: ", expr)
	//tp := types.NewFieldType(mysql.TypeLonglong)
	b, err := newBaseBuiltinFunc(ctx, "ForceToInt", []Expression{expr}, &targetFieldType)
	if err != nil {
		return nil, err
	}

	sig := &builtinForceToIntSig{b, rounder, targetFieldType}
	//sig.setPbCode(tipb.ScalarFuncSig_Unspecified)

	sf := &ScalarFunction{
		FuncName: ast.NewCIStr("ForceToInt"),
		RetType:  &targetFieldType,
		Function: sig,
	}

	return FoldConstant(ctx, sf), nil
}

func GetForceToIntCeil(ctx BuildContext, targetFieldType types.FieldType, expr Expression, op opcode.Op) (Expression, error) {
	evalCtx := ctx.GetEvalCtx()
	ceil := NewFunctionInternal(ctx, ast.Ceil, types.NewFieldType(mysql.TypeUnspecified), expr)
	switch expr.GetType(evalCtx).EvalType() {
	case types.ETInt:
	case types.ETReal, types.ETDecimal:
		ceil = NewFunctionInternal(ctx, ast.Ceil, types.NewFieldType(mysql.TypeUnspecified), expr)
	case types.ETDatetime:
	case types.ETString:
	case types.ETTimestamp:
	case types.ETDuration:
		// round?
	case types.ETJson:
	case types.ETVectorFloat32:
	}

	return getForceToInt(ctx, targetFieldType, expr, ceil)
}

func GetForceToIntFloor(ctx BuildContext, targetFieldType types.FieldType, expr Expression, op opcode.Op) (Expression, error) {
	evalCtx := ctx.GetEvalCtx()
	floor := NewFunctionInternal(ctx, ast.Floor, types.NewFieldType(mysql.TypeUnspecified), expr)
	switch expr.GetType(evalCtx).EvalType() {
	case types.ETInt:
	case types.ETReal, types.ETDecimal:
		floor = NewFunctionInternal(ctx, ast.Floor, types.NewFieldType(mysql.TypeUnspecified), expr)
	case types.ETDatetime:
	case types.ETString:
	case types.ETTimestamp:
	case types.ETDuration:
		// round?
	case types.ETJson:
	case types.ETVectorFloat32:
	}
	return getForceToInt(ctx, targetFieldType, expr, floor)
}

func GetForceToIntEQ(ctx BuildContext, targetFieldType types.FieldType, expr *Constant, op opcode.Op) (Expression, error) {
	return getForceToInt(ctx, targetFieldType, expr, nil)
}

type builtinForceIntToTimeSig struct {
	baseBuiltinFunc
	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
	targetFieldType *types.FieldType
	mode            ForceIntToTimeMode
}

func (b *builtinForceIntToTimeSig) cloneFrom(from *builtinForceIntToTimeSig) {
	b.baseBuiltinFunc.cloneFrom(&from.baseBuiltinFunc)
	b.targetFieldType = from.targetFieldType.Clone()
	b.mode = from.mode
}

func (b *builtinForceIntToTimeSig) Clone() builtinFunc {
	newSig := &builtinForceIntToTimeSig{}
	newSig.cloneFrom(b)
	return newSig
}

// ForceIntToTimeMode is used to specify rounding mode or equality for force int to time conversion.
type ForceIntToTimeMode int

const (
	ForceIntToTimeModeFloor ForceIntToTimeMode = iota
	ForceIntToTimeModeCeil
	ForceIntToTimeModeEqual
)

func (b *builtinForceIntToTimeSig) evalTime(ctx EvalContext, row chunk.Row) (res types.Time, isNull bool, err error) {
	val, isNull, err := b.args[0].EvalInt(ctx, row)
	if isNull || err != nil {
		return res, isNull, err
	}

	if b.args[0].GetType(ctx).GetType() == mysql.TypeYear {
		res, err = types.ParseTimeFromYear(val)
	} else {
		res, err = types.ParseTimeFromNum(typeCtx(ctx), val, b.tp.GetType(), b.tp.GetDecimal())
	}
	if err != nil {
		res.SetToBadInt(uint64(val))
		return res, false, handleInvalidTimeError(ctx, err)
	}
	if b.tp.GetType() == mysql.TypeDate {
		// Truncate hh:mm:ss part if the type is Date.
		res.SetCoreTime(types.FromDate(res.Year(), res.Month(), res.Day(), 0, 0, 0, 0))
	}
	return res, false, nil
}

func WrapWithForceIntToTime(ctx BuildContext, targetFieldType *types.FieldType, expr Expression, mode ForceIntToTimeMode) (Expression, error) {
	//fmt.Println("fudge2: ", expr)
	//tp := types.NewFieldType(mysql.TypeLonglong)
	b, err := newBaseBuiltinFunc(ctx, "ForceIntToTime", []Expression{expr}, targetFieldType)
	if err != nil {
		return nil, err
	}

	sig := &builtinForceIntToTimeSig{b, targetFieldType, mode}

	sf := &ScalarFunction{
		FuncName: ast.NewCIStr("ForceToInt"),
		RetType:  targetFieldType,
		Function: sig,
	}

	return FoldConstant(ctx, sf), nil
}
