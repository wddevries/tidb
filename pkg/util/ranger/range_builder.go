package ranger

import (
	"math"

	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/collate"
)

type orExpr struct {
	exprs   []expression.Expression // all completely covered expressions
	subAnds []andExpr
	cols    map[int64]struct{} // all columns in subAnds
}

func makeOrExpr() orExpr {
	var oe orExpr
	oe.cols = make(map[int64]struct{})
	return oe
}

type andRangeCol struct {
	exprs       []expression.Expression // all completely covered expressions
	col         *expression.Column
	points      []*point
	collator    collate.Collator
	columnValue *valueInfo
}

type andExpr struct {
	subOrs []orExpr
	//orCols map[int64]orExpr
	cols   map[int64]struct{} // all columns in subOrs and ranges
	ranges map[int64]andRangeCol
}

func makeAndExpr() andExpr {
	var ae andExpr
	ae.cols = make(map[int64]struct{})
	ae.ranges = make(map[int64]andRangeCol)
	return ae
}
func (ae *andExpr) finalize(d *rangeDetacher) (success bool) {
	for _, ae := range ae.subOrs {
		if !ae.finalize(d) {
			return false
		}
	}

	for _, rc := range ae.ranges {
		newTp := newFieldType(rc.col.RetType)
		if d.convertToSortKey {
			newTp = convertStringFTToBinaryCollate(newTp)
		}
		rc.collator = collate.GetCollator(newTp.GetCollate())
		cpts, err := convertPoints(d.sctx, rc.points, newTp, mysql.HasNotNullFlag(newTp.GetFlag()), false)
		if err != nil {
			return false
		}
		rc.points = cpts

		if len(rc.exprs) == 1 && allEqOrIn(rc.exprs[0]) {
			rc.columnValue = extractValueInfo(rc.exprs[0])
		} else {
			rc.columnValue = nil
		}
	}

	return true
}

func (oe *orExpr) finalize(d *rangeDetacher) (success bool) {
	for _, ae := range oe.subAnds {
		if !ae.finalize(d) {
			return false
		}
	}

	return true
}

func (rc *andRangeCol) rangeColInsertPoint(
	points []*point,
	rb builder,
	colType *types.FieldType,
	col *expression.Column,
) (success bool) {
	rc.col = col
	if rc.points != nil {
		// I am not sure why we can't use newTp for this.
		collator := collate.GetCollator(colType.GetCollate())
		points = rb.intersection(rc.points, points, collator)
		if rb.err != nil {
			return false
		}
		if len(points) == 0 {
			// if points is empty then the conditions can not be satisifed.
			return false
		}
	}

	rc.points = points
	return true
}

func (ae *andExpr) addOr(oe orExpr) {
	for c := range oe.cols {
		ae.cols[c] = struct{}{}
	}
	ae.subOrs = append(ae.subOrs, oe)
}

func (oe *orExpr) addAnd(ae andExpr) {
	for c := range ae.cols {
		oe.cols[c] = struct{}{}
	}
	oe.subAnds = append(oe.subAnds, ae)
}

func buildOrFromPoints(
	points []*point,
	rb builder,
	colType *types.FieldType,
	col *expression.Column,
	cond expression.Expression,
) (
	oe orExpr,
) {
	oe = makeOrExpr()

	// make an or
	for len(points) >= 2 {
		sae := makeAndExpr()
		// just pass no condition
		rc := sae.ranges[col.ID]
		success := rc.rangeColInsertPoint(points[:2], rb, colType, col)
		if success {
			sae.cols[col.ID] = struct{}{}
			sae.ranges[col.ID] = rc
		} else {
			panic("Success should have been true")
		}
		oe.addAnd(sae)
		points = points[2:]
	}

	oe.cols[col.ID] = struct{}{}
	oe.exprs = append(oe.exprs, cond)

	return
}

func buildOr(
	d *rangeDetacher,
	conds []expression.Expression,
	f *expression.ScalarFunction,
	cols []*expression.Column,
	lengths []int,
) (
	oe orExpr,
	filterExprs []expression.Expression,
	success bool,
) {
	// f is a cast of the cond
	dnfItems := expression.FlattenDNFConditions(f)
	oe = makeOrExpr()
	fullyCovered := true
	for _, subcond := range dnfItems {
		ae, fe, success := buildAnd(d, []expression.Expression{subcond}, cols, lengths)
		if len(fe) > 0 {
			fullyCovered = false
		}
		// If one branch of an OR fails, then we are scanning a range covering the others. We don't need the or.
		// But if the branch is impossible, we should drop it and keep the rest.
		if !success {
			// branches of an or must have some applicable condition
			return orExpr{}, filterExprs, false
		}

		oe.addAnd(ae)
	}

	if fullyCovered {
		oe.exprs = append(oe.exprs, conds...)
	} else {
		filterExprs = append(filterExprs, conds...)
	}

	return oe, filterExprs, true
}

func buildAnd(
	d *rangeDetacher,
	conds []expression.Expression,
	cols []*expression.Column,
	lengths []int,
) (
	ae andExpr,
	filterExprs []expression.Expression,
	success bool,
) {
	rb := builder{sctx: d.sctx}
	ae = makeAndExpr()
	ae.cols = make(map[int64]struct{})
	conlen := len(conds)
	appliedCond := false
	for i := 0; i < conlen; i++ {
		cond := conds[i]
		f, ok := cond.(*expression.ScalarFunction)
		if ok {
			switch f.FuncName.L {
			case ast.LogicAnd:
				// It should be fine to collapse this condition into the main list.
				conds = append(conds, f.GetArgs()...)
				conlen = len(conds)
				continue
			case ast.LogicOr:
				oe, oeFilterExprs, oeSuccess := buildOr(d, []expression.Expression{cond}, f, cols, lengths)
				if oeSuccess {
					ae.addOr(oe)
					appliedCond = true
					if len(oeFilterExprs) == 0 {
						continue
					}
				}
			case ast.EQ, ast.NullEQ, ast.LE, ast.GE, ast.LT, ast.GT, ast.In, ast.IsNull:
				offset := getPotentialEqOrInColOffset(d.sctx, cond, cols)
				if offset != -1 {
					col := cols[offset]
					colType := col.GetType(d.sctx.ExprCtx.GetEvalCtx())
					newTp := newFieldType(colType)
					points := rb.build(cond, newTp, types.UnspecifiedLength, false)
					if rb.err == nil {
						if len(points) > 2 {
							oe := buildOrFromPoints(points, rb, colType, col, cond)
							ae.addOr(oe)
							appliedCond = true
						} else if len(points) == 2 {
							rc := ae.ranges[col.ID]
							aeSuccess := rc.rangeColInsertPoint(points[:2], rb, colType, col)
							// failed means impossible, but we just skip it for now
							if aeSuccess {
								ae.cols[col.ID] = struct{}{}
								ae.ranges[col.ID] = rc
								appliedCond = true
								isFullLength := lengths[offset] == types.UnspecifiedLength || lengths[offset] == col.GetType(d.sctx.ExprCtx.GetEvalCtx()).GetFlen()
								if isFullLength {
									rc.exprs = append(rc.exprs, cond)
									continue
								}
							}
						}
					}
				}
			}
		}

		filterExprs = append(filterExprs, cond)
	}

	return ae, filterExprs, appliedCond
}

type RangeBuilder struct {
	filterExprs []expression.Expression // all completely covered expressions
	and         andExpr
	colMap      map[int64]struct{} // all columns in subAnds
}

func MakeRangeBuilder(
	d *rangeDetacher,
	conds []expression.Expression,
	cols []*expression.Column,
	lengths []int,
) (
	rb RangeBuilder,
	success bool,
) {
	and, filters, success := buildAnd(d, conds, cols, lengths)
	if success {
		// This should probably be merged with the code above.
		success = and.finalize(d)
	}
	return RangeBuilder{filters, and, and.cols}, true
}

/*
// Range represents a range generated in physical plan building phase.
type Range struct {
	LowVal      []types.Datum // Low value is exclusive.
	HighVal     []types.Datum // High value is exclusive.
	Collators   []collate.Collator
	LowExclude  bool
	HighExclude bool
}

type point struct {
	value types.Datum
	excl  bool // exclude
	start bool
}

type Range struct {
	LowVal      []types.Datum // Low value is exclusive.
	HighVal     []types.Datum // High value is exclusive.
	Collators   []collate.Collator
	LowExclude  bool
	HighExclude bool
}
*/

type rangeCollector struct {
	points       [][]*point // should only be two points per column
	collators    []collate.Collator
	columnValues []*valueInfo
	depth        int
}

func makeRangeCollector(l int) rangeCollector {
	rng := rangeCollector{
		make([][]*point, l),
		make([]collate.Collator, l),
		nil,
		0,
	}
	return rng
}

func copyRangeCollector(rng rangeCollector) rangeCollector {
	newRng := makeRangeCollector(cap(rng.points))
	copy(newRng.points, rng.points)
	copy(newRng.collators, rng.collators)
	newRng.columnValues = rng.columnValues
	newRng.depth = rng.depth + 1
	return newRng
}

func (rb *rangeCollector) finalize(d *rangeDetacher) *Range {
	tc := d.sctx.TypeCtx
	r := &Range{}
	l := 0 // number of cols in output range
	// find number of cols
	for l < len(rb.points) && len(rb.points[l]) > 0 {
		a := rb.points[l][0]
		b := rb.points[l][1]
		coll := rb.collators[l]

		r.LowVal = append(r.LowVal, a.value)
		r.HighVal = append(r.HighVal, b.value)
		r.Collators = append(r.Collators, coll)
		r.LowExclude = a.excl
		r.HighExclude = b.excl

		l++
		if !isPointImpl(tc, a.value, b.value, coll, d.sctx.RegardNULLAsPoint) || a.excl || b.excl {
			// This is not a point, so we can add no more columns
			break
		}
	}

	return r
}

func (oe orExpr) getRangeCols(
	d *rangeDetacher,
	rng rangeCollector,
	colmap map[int64]int,
) (
	ranges Ranges,
	accessCond []expression.Expression,
	filterConds []expression.Expression,
	success bool,
) {
	rngs := Ranges{}
	var matchExprs []expression.Expression
	matchedAll := true
	for _, ae := range oe.subAnds {
		outRngs, _, aeRemainedConds, aeSuccess := ae.getRangeCols(d, copyRangeCollector(rng), colmap)
		if len(aeRemainedConds) > 0 {
			matchedAll = false
		}
		if !aeSuccess {
			return Ranges{}, []expression.Expression{}, []expression.Expression{}, false
		}
		rngs = append(rngs, outRngs...)
	}

	if matchedAll {
		matchExprs = append(matchExprs, oe.exprs...)
	} else {
		filterConds = append(filterConds, oe.exprs...)
	}
	return rngs, matchExprs, filterConds, true
}

func (ae andExpr) getRangeCols(
	d *rangeDetacher,
	rng rangeCollector,
	colmap map[int64]int,
) (
	ranges Ranges,
	accessConds []expression.Expression,
	filterConds []expression.Expression,
	success bool,
) {
	firstcol := math.MaxInt64 // this is the first column modified
	for _, rc := range ae.ranges {
		if i, found := colmap[rc.col.ID]; found {
			points := rc.points
			if len(points) > 0 {
				rng.columnValues[i] = nil
				rb := builder{sctx: d.sctx}
				points = rb.intersection(points, rng.points[i], rc.collator)
				if rb.err != nil {
					// Something when wrong?  I am not sure this can happen.
					filterConds = append(filterConds, rc.exprs...)
					continue
					//return Ranges{}, []expression.Expression{}, []expression.Expression{}, false
				} else if len(points) == 0 {
					/* If points is empty, then the conditions can not be satisifed. */
					return Ranges{}, []expression.Expression{}, []expression.Expression{}, true
				}
			} else {
				if rng.depth == 0 {
					rng.columnValues[i] = rc.columnValue
				}
			}

			if i < firstcol {
				firstcol = i
			}
			rng.points[i] = points
			rng.collators[i] = rc.collator
			accessConds = append(accessConds, rc.exprs...)
		} else {
			filterConds = append(filterConds, rc.exprs...)
		}
	}

	rngs := Ranges{}
	for _, o := range ae.subOrs {
		orngs, oAccessConds, oRemainedConds, oSuccess := o.getRangeCols(d, rng, colmap)
		if oSuccess {
			filterConds = append(filterConds, oRemainedConds...)
			accessConds = append(accessConds, oAccessConds...)
			rngs = append(rngs, orngs...)
		}
	}

	if len(rngs) == 0 {
		r := rng.finalize(d)
		// last column in rng must not be less than firstcol
		if len(r.LowVal) > firstcol {
			rngs = append(rngs, r)
		} else {
			success = false
		}
	}

	return rngs, accessConds, filterConds, success
}

func (rb RangeBuilder) buildRange(
	d *rangeDetacher,
	cols []*expression.Column,
) (
	ranges Ranges,
	accessCond []expression.Expression,
	filterConds []expression.Expression,
	columnValues []*valueInfo,
	success bool,
) {
	rng := makeRangeCollector(len(cols))
	rng.columnValues = make([]*valueInfo, len(cols))
	colmap := make(map[int64]int)
	for i, col := range cols {
		colmap[col.ID] = i
	}

	rngs, exprs, filters, success := rb.and.getRangeCols(d, rng, colmap)
	filters = append(filters, rb.filterExprs...)
	return rngs, exprs, filters, rng.columnValues, success
}
