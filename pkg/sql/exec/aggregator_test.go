// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package exec

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/cockroachdb/apd"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlpb"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/types"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

var (
	defaultGroupCols = []uint32{0}
	defaultAggCols   = [][]uint32{{1}}
	defaultAggFns    = []distsqlpb.AggregatorSpec_Func{distsqlpb.AggregatorSpec_SUM}
	defaultColTyps   = []types.T{types.Int64, types.Int64}
)

type aggregatorTestCase struct {
	// colTypes, aggFns, groupCols, and aggCols will be set to their default
	// values before running a test if nil.
	colTypes  []types.T
	aggFns    []distsqlpb.AggregatorSpec_Func
	groupCols []uint32
	aggCols   [][]uint32
	input     tuples
	expected  tuples
	// {output}BatchSize if not 0 are passed in to NewOrderedAggregator to
	// divide input/output batches.
	batchSize       int
	outputBatchSize int
	name            string

	// convToDecimal will convert any float64s to apd.Decimals. If a string is
	// encountered, a best effort is made to convert that string to an
	// apd.Decimal.
	convToDecimal bool
}

// aggType is a helper struct that allows tests to test both the ordered and
// hash aggregators at the same time.
type aggType struct {
	new func(input Operator,
		colTypes []types.T,
		aggFns []distsqlpb.AggregatorSpec_Func,
		groupCols []uint32,
		aggCols [][]uint32,
	) (Operator, error)
	name string
}

var aggTypes = []aggType{
	{
		new:  NewHashAggregator,
		name: "hash",
	},
	{
		new:  NewOrderedAggregator,
		name: "ordered",
	},
}

func (tc *aggregatorTestCase) init() error {
	if tc.convToDecimal {
		for _, tuples := range []tuples{tc.input, tc.expected} {
			for _, tuple := range tuples {
				for i, e := range tuple {
					switch v := e.(type) {
					case float64:
						d := &apd.Decimal{}
						d, err := d.SetFloat64(v)
						if err != nil {
							return err
						}
						tuple[i] = *d
					case string:
						d := &apd.Decimal{}
						d, _, err := d.SetString(v)
						if err != nil {
							// If there was an error converting the string to decimal, just
							// leave the datum as is.
							continue
						}
						tuple[i] = *d
					}
				}
			}
		}
	}
	if tc.groupCols == nil {
		tc.groupCols = defaultGroupCols
	}
	if tc.aggFns == nil {
		tc.aggFns = defaultAggFns
	}
	if tc.aggCols == nil {
		tc.aggCols = defaultAggCols
	}
	if tc.colTypes == nil {
		tc.colTypes = defaultColTyps
	}
	if tc.batchSize == 0 {
		tc.batchSize = coldata.BatchSize
	}
	if tc.outputBatchSize == 0 {
		tc.outputBatchSize = coldata.BatchSize
	}
	return nil
}

func TestAggregatorOneFunc(t *testing.T) {
	testCases := []aggregatorTestCase{
		{
			input: tuples{
				{0, 1},
			},
			expected: tuples{
				{1},
			},
			name:            "OneTuple",
			outputBatchSize: 4,
		},
		{
			input: tuples{
				{0, 1},
				{0, 1},
			},
			expected: tuples{
				{2},
			},
			name: "OneGroup",
		},
		{
			input: tuples{
				{0, 1},
				{0, 0},
				{0, 1},
				{1, 4},
				{2, 5},
			},
			expected: tuples{
				{2},
				{4},
				{5},
			},
			batchSize: 2,
			name:      "MultiGroup",
		},
		{
			input: tuples{
				{0, 1},
				{0, 2},
				{0, 3},
				{1, 4},
				{1, 5},
			},
			expected: tuples{
				{6},
				{9},
			},
			batchSize: 1,
			name:      "CarryBetweenInputBatches",
		},
		{
			input: tuples{
				{0, 1},
				{0, 2},
				{0, 3},
				{0, 4},
				{1, 5},
				{2, 6},
			},
			expected: tuples{
				{10},
				{5},
				{6},
			},
			batchSize:       2,
			outputBatchSize: 1,
			name:            "CarryBetweenOutputBatches",
		},
		{
			input: tuples{
				{0, 1},
				{0, 1},
				{1, 2},
				{2, 3},
				{2, 3},
				{3, 4},
				{3, 4},
				{4, 5},
				{5, 6},
				{6, 7},
				{7, 8},
			},
			expected: tuples{
				{2},
				{2},
				{6},
				{8},
				{5},
				{6},
				{7},
				{8},
			},
			batchSize:       4,
			outputBatchSize: 1,
			name:            "CarryBetweenInputAndOutputBatches",
		},
		{
			input: tuples{
				{0, 1},
				{0, 2},
				{0, 3},
				{0, 4},
			},
			expected: tuples{
				{10},
			},
			batchSize:       1,
			outputBatchSize: 1,
			name:            "NoGroupingCols",
			groupCols:       []uint32{},
		},
		{
			input: tuples{
				{1, 0, 0},
				{2, 0, 0},
				{3, 0, 0},
				{4, 0, 0},
			},
			expected: tuples{
				{10},
			},
			batchSize:       1,
			outputBatchSize: 1,
			name:            "UnusedInputColumns",
			colTypes:        []types.T{types.Int64, types.Int64, types.Int64},
			groupCols:       []uint32{1, 2},
			aggCols:         [][]uint32{{0}},
		},
	}

	// Run tests with deliberate batch sizes and no selection vectors.
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.init(); err != nil {
				t.Fatal(err)
			}

			tupleSource := newOpTestInput(uint16(tc.batchSize), tc.input)
			a, err := NewOrderedAggregator(
				tupleSource,
				tc.colTypes,
				tc.aggFns,
				tc.groupCols,
				tc.aggCols,
			)
			if err != nil {
				t.Fatal(err)
			}

			out := newOpTestOutput(a, []int{0}, tc.expected)
			// Explicitly reinitialize the aggregator with the given output batch
			// size.
			a.(*orderedAggregator).initWithBatchSize(tc.batchSize, tc.outputBatchSize)
			if err := out.VerifyAnyOrder(); err != nil {
				t.Fatal(err)
			}

			// Run randomized tests on this test case.
			t.Run(fmt.Sprintf("Randomized"), func(t *testing.T) {
				for _, agg := range aggTypes {
					t.Run(agg.name, func(t *testing.T) {
						runTests(t, []tuples{tc.input}, tc.expected, unorderedVerifier, []int{0},
							func(input []Operator) (Operator, error) {
								return agg.new(
									input[0],
									tc.colTypes,
									tc.aggFns,
									tc.groupCols,
									tc.aggCols,
								)
							})
					})
				}
			})
		})
	}
}

func TestAggregatorMultiFunc(t *testing.T) {
	testCases := []aggregatorTestCase{
		{
			aggFns: []distsqlpb.AggregatorSpec_Func{distsqlpb.AggregatorSpec_SUM, distsqlpb.AggregatorSpec_SUM},
			aggCols: [][]uint32{
				{2}, {1},
			},
			input: tuples{
				{0, 1, 2},
				{0, 1, 2},
			},
			colTypes: []types.T{types.Int64, types.Int64, types.Int64},
			expected: tuples{
				{4, 2},
			},
			name: "OutputOrder",
		},
		{
			aggFns: []distsqlpb.AggregatorSpec_Func{distsqlpb.AggregatorSpec_SUM, distsqlpb.AggregatorSpec_SUM},
			aggCols: [][]uint32{
				{2}, {1},
			},
			input: tuples{
				{0, 1, 1.3},
				{0, 1, 1.6},
				{0, 1, 0.5},
				{1, 1, 1.2},
			},
			colTypes: []types.T{types.Int64, types.Int64, types.Decimal},
			expected: tuples{
				{3.4, 3},
				{1.2, 1},
			},
			name:          "SumMultiType",
			convToDecimal: true,
		},
		{
			aggFns: []distsqlpb.AggregatorSpec_Func{distsqlpb.AggregatorSpec_AVG, distsqlpb.AggregatorSpec_SUM},
			aggCols: [][]uint32{
				{1}, {1},
			},
			input: tuples{
				{0, 1.1},
				{0, 1.2},
				{0, 2.3},
				{1, 6.21},
				{1, 2.43},
			},
			colTypes: []types.T{types.Int64, types.Decimal},
			expected: tuples{
				{"1.5333333333333333333", 4.6},
				{4.32, 8.64},
			},
			name:          "AvgSumSingleInputBatch",
			convToDecimal: true,
		},
	}

	for _, agg := range aggTypes {
		for _, tc := range testCases {
			t.Run(fmt.Sprintf("%s/%s/Randomized", agg.name, tc.name), func(t *testing.T) {
				if err := tc.init(); err != nil {
					t.Fatal(err)
				}
				runTests(t, []tuples{tc.input}, tc.expected, unorderedVerifier, []int{0, 1},
					func(input []Operator) (Operator, error) {
						return agg.new(input[0], tc.colTypes, tc.aggFns, tc.groupCols, tc.aggCols)
					})
			})
		}
	}
}

func TestAggregatorAllFunctions(t *testing.T) {
	testCases := []aggregatorTestCase{
		{
			aggFns: []distsqlpb.AggregatorSpec_Func{
				distsqlpb.AggregatorSpec_ANY_NOT_NULL,
				distsqlpb.AggregatorSpec_AVG,
				distsqlpb.AggregatorSpec_COUNT_ROWS,
				distsqlpb.AggregatorSpec_COUNT,
				distsqlpb.AggregatorSpec_SUM,
				distsqlpb.AggregatorSpec_MIN,
				distsqlpb.AggregatorSpec_MAX,
			},
			aggCols:  [][]uint32{{0}, {1}, {}, {1}, {2}, {2}, {2}},
			colTypes: []types.T{types.Int64, types.Decimal, types.Int64},
			input: tuples{
				{0, 3.1, 2},
				{0, 1.1, 3},
				{1, 1.1, 1},
				{1, 4.1, 0},
				{2, 1.1, 1},
				{3, 4.1, 0},
				{3, 5.1, 0},
			},
			expected: tuples{
				{0, 2.1, 2, 2, 5, 2, 3},
				{1, 2.6, 2, 2, 1, 0, 1},
				{2, 1.1, 1, 1, 1, 1, 1},
				{3, 4.6, 2, 2, 0, 0, 0},
			},
			convToDecimal: true,
		},

		// Test case for null handling.
		{
			aggFns: []distsqlpb.AggregatorSpec_Func{
				distsqlpb.AggregatorSpec_ANY_NOT_NULL,
				distsqlpb.AggregatorSpec_ANY_NOT_NULL,
				distsqlpb.AggregatorSpec_COUNT_ROWS,
				distsqlpb.AggregatorSpec_COUNT,
				distsqlpb.AggregatorSpec_SUM,
				distsqlpb.AggregatorSpec_SUM_INT,
				distsqlpb.AggregatorSpec_MIN,
				distsqlpb.AggregatorSpec_MAX,
				distsqlpb.AggregatorSpec_AVG,
			},
			aggCols:  [][]uint32{{0}, {1}, {}, {1}, {1}, {2}, {2}, {2}, {1}},
			colTypes: []types.T{types.Int64, types.Decimal, types.Int64},
			input: tuples{
				{0, nil, nil},
				{0, 3.1, 5},
				{1, nil, nil},
				{1, nil, nil},
			},
			expected: tuples{
				{0, 3.1, 2, 1, 3.1, 5, 5, 5, 3.1},
				{1, nil, 2, 0, nil, nil, nil, nil, nil},
			},
			convToDecimal: true,
		},
	}

	for _, agg := range aggTypes {
		for i, tc := range testCases {
			t.Run(fmt.Sprintf("%s/%d", agg.name, i), func(t *testing.T) {
				if err := tc.init(); err != nil {
					t.Fatal(err)
				}
				runTests(
					t,
					[]tuples{tc.input},
					tc.expected,
					orderedVerifier,
					[]int{0, 1, 2, 3, 4, 5, 6, 7, 8}[:len(tc.expected[0])],
					func(input []Operator) (Operator, error) {
						return agg.new(input[0], tc.colTypes, tc.aggFns, tc.groupCols, tc.aggCols)
					})
			})
		}
	}
}

func TestAggregatorRandom(t *testing.T) {
	// This test aggregates random inputs, keeping track of the expected results
	// to make sure the aggregations are correct.
	rng, _ := randutil.NewPseudoRand()
	ctx := context.Background()
	for _, groupSize := range []int{1, 2, coldata.BatchSize / 4, coldata.BatchSize / 2} {
		for _, numInputBatches := range []int{1, 2, 64} {
			for _, hasNulls := range []bool{true, false} {
				for _, agg := range aggTypes {
					t.Run(fmt.Sprintf("%s/groupSize=%d/numInputBatches=%d/hasNulls=%t", agg.name, groupSize, numInputBatches, hasNulls),
						func(t *testing.T) {
							nTuples := coldata.BatchSize * numInputBatches
							typs := []types.T{types.Int64, types.Float64}
							cols := []coldata.Vec{
								coldata.NewMemColumn(typs[0], nTuples),
								coldata.NewMemColumn(typs[1], nTuples)}
							groups, aggCol, aggColNulls := cols[0].Int64(), cols[1].Float64(), cols[1].Nulls()
							var expRowCounts, expCounts []int64
							var expSums, expMins, expMaxs []float64
							// SUM, MIN, MAX, and AVG aggregators can output null.
							var expNulls []bool
							curGroup := -1
							for i := range groups {
								if i%groupSize == 0 {
									expRowCounts = append(expRowCounts, int64(groupSize))
									expCounts = append(expCounts, 0)
									expSums = append(expSums, 0)
									expMins = append(expMins, 2048)
									expMaxs = append(expMaxs, -2048)
									expNulls = append(expNulls, true)
									curGroup++
								}
								if hasNulls && rng.Float64() < nullProbability {
									aggColNulls.SetNull(uint16(i))
								} else {
									// Keep the inputs small so they are a realistic size. Using a
									// large range is not realistic and makes decimal operations
									// slower.
									aggCol[i] = 2048 * (rng.Float64() - 0.5)
									expNulls[curGroup] = false
									expCounts[curGroup]++
									expSums[curGroup] += aggCol[i]
									expMins[curGroup] = min64(aggCol[i], expMins[curGroup])
									expMaxs[curGroup] = max64(aggCol[i], expMaxs[curGroup])
								}
								groups[i] = int64(curGroup)
							}

							source := newChunkingBatchSource(typs, cols, uint64(nTuples))
							a, err := agg.new(
								source,
								typs,
								[]distsqlpb.AggregatorSpec_Func{
									distsqlpb.AggregatorSpec_COUNT_ROWS,
									distsqlpb.AggregatorSpec_COUNT,
									distsqlpb.AggregatorSpec_SUM_INT,
									distsqlpb.AggregatorSpec_MIN,
									distsqlpb.AggregatorSpec_MAX,
									distsqlpb.AggregatorSpec_AVG},
								[]uint32{0},
								[][]uint32{{}, {1}, {1}, {1}, {1}, {1}},
							)
							if err != nil {
								t.Fatal(err)
							}
							a.Init()

							// Exhaust aggregator until all batches have been read.
							i := 0
							tupleIdx := 0
							for b := a.Next(ctx); b.Length() != 0; b = a.Next(ctx) {
								rowCountCol := b.ColVec(0)
								countCol := b.ColVec(1)
								sumCol := b.ColVec(2)
								minCol := b.ColVec(3)
								maxCol := b.ColVec(4)
								avgCol := b.ColVec(5)
								for j := uint16(0); j < b.Length(); j++ {
									rowCount := rowCountCol.Int64()[j]
									count := countCol.Int64()[j]
									sum := sumCol.Float64()[j]
									min := minCol.Float64()[j]
									max := maxCol.Float64()[j]
									avg := avgCol.Float64()[j]
									expRowCount := expRowCounts[tupleIdx]
									if rowCount != expRowCount {
										t.Fatalf("Found rowCount %d, expected %d, idx %d of batch %d", rowCount, expRowCount, j, i)
									}
									expCount := expCounts[tupleIdx]
									if count != expCount {
										t.Fatalf("Found count %d, expected %d, idx %d of batch %d", count, expCount, j, i)
									}

									expNull := expNulls[tupleIdx]
									if expNull {
										if !sumCol.Nulls().NullAt(uint16(j)) {
											t.Fatalf("Found non-null sum %f, expected null, idx %d of batch %d", sum, j, i)
										}
										if !minCol.Nulls().NullAt(uint16(j)) {
											t.Fatalf("Found non-null min %f, expected null, idx %d of batch %d", sum, j, i)
										}
										if !maxCol.Nulls().NullAt(uint16(j)) {
											t.Fatalf("Found non-null max %f, expected null, idx %d of batch %d", sum, j, i)
										}
										if !avgCol.Nulls().NullAt(uint16(j)) {
											t.Fatalf("Found non-null avg %f, expected null, idx %d of batch %d", sum, j, i)
										}
									} else {
										expSum := expSums[tupleIdx]
										if math.Abs(sum-expSum) > 1e-6 {
											t.Fatalf("Found sum %f, expected %f, idx %d of batch %d", sum, expSum, j, i)
										}
										expMin := expMins[tupleIdx]
										if min != expMin {
											t.Fatalf("Found min %f, expected %f, idx %d of batch %d", min, expMin, j, i)
										}
										expMax := expMaxs[tupleIdx]
										if max != expMax {
											t.Fatalf("Found max %f, expected %f, idx %d of batch %d", max, expMax, j, i)
										}
										expAvg := expSum / float64(expCount)
										if math.Abs(avg-expAvg) > 1e-6 {
											t.Fatalf("Found avg %f, expected %f, idx %d of batch %d", avg, expAvg, j, i)
										}
									}
									tupleIdx++
								}
								i++
							}
							totalInputRows := numInputBatches * coldata.BatchSize
							nOutputRows := totalInputRows / groupSize
							expBatches := (nOutputRows / coldata.BatchSize)
							if nOutputRows%coldata.BatchSize != 0 {
								expBatches++
							}
							if i != expBatches {
								t.Fatalf("expected %d batches, found %d", expBatches, i)
							}
						})
				}
			}
		}
	}
}

func BenchmarkAggregator(b *testing.B) {
	rng, _ := randutil.NewPseudoRand()
	ctx := context.Background()

	for _, aggFn := range []distsqlpb.AggregatorSpec_Func{
		distsqlpb.AggregatorSpec_ANY_NOT_NULL,
		distsqlpb.AggregatorSpec_AVG,
		distsqlpb.AggregatorSpec_COUNT_ROWS,
		distsqlpb.AggregatorSpec_COUNT,
		distsqlpb.AggregatorSpec_SUM,
		distsqlpb.AggregatorSpec_MIN,
		distsqlpb.AggregatorSpec_MAX,
	} {
		fName := distsqlpb.AggregatorSpec_Func_name[int32(aggFn)]
		b.Run(fName, func(b *testing.B) {
			for _, agg := range aggTypes {
				for _, typ := range []types.T{types.Int64, types.Decimal} {
					for _, groupSize := range []int{1, 2, coldata.BatchSize / 2, coldata.BatchSize} {
						for _, hasNulls := range []bool{false, true} {
							for _, numInputBatches := range []int{64} {
								b.Run(fmt.Sprintf("%s/%s/groupSize=%d/hasNulls=%t/numInputBatches=%d", agg.name, typ.String(),
									groupSize, hasNulls, numInputBatches),
									func(b *testing.B) {
										colTypes := []types.T{types.Int64, typ}
										nTuples := numInputBatches * coldata.BatchSize
										cols := []coldata.Vec{coldata.NewMemColumn(types.Int64, nTuples), coldata.NewMemColumn(typ, nTuples)}
										groups := cols[0].Int64()
										curGroup := -1
										for i := 0; i < nTuples; i++ {
											if groupSize == 1 || i%groupSize == 0 {
												curGroup++
											}
											groups[i] = int64(curGroup)
										}
										if hasNulls {
											nulls := cols[1].Nulls()
											for i := 0; i < nTuples; i++ {
												if rng.Float64() < nullProbability {
													nulls.SetNull(uint16(i))
												}
											}
										}
										switch typ {
										case types.Int64:
											vals := cols[1].Int64()
											for i := range vals {
												vals[i] = rng.Int63() % 1024
											}
										case types.Decimal:
											vals := cols[1].Decimal()
											for i := range vals {
												vals[i].SetInt64(rng.Int63() % 1024)
											}
										}
										source := newChunkingBatchSource(colTypes, cols, uint64(nTuples))

										nCols := 1
										if aggFn == distsqlpb.AggregatorSpec_COUNT_ROWS {
											nCols = 0
										}
										a, err := agg.new(
											source,
											colTypes,
											[]distsqlpb.AggregatorSpec_Func{aggFn},
											[]uint32{0},
											[][]uint32{[]uint32{1}[:nCols]},
										)
										if err != nil {
											b.Skip()
										}
										a.Init()

										b.ResetTimer()

										// Only count the int64 column.
										b.SetBytes(int64(8 * nTuples))
										for i := 0; i < b.N; i++ {
											a.(resetter).reset()
											source.reset()
											// Exhaust aggregator until all batches have been read.
											foundTuples := 0
											for b := a.Next(ctx); b.Length() != 0; b = a.Next(ctx) {
												foundTuples += int(b.Length())
											}
											if foundTuples != nTuples/groupSize {
												b.Fatalf("Found %d tuples, expected %d", foundTuples, nTuples/groupSize)
											}
										}
									},
								)
							}
						}
					}
				}
			}
		})
	}
}

func TestHashAggregator(t *testing.T) {
	tcs := []aggregatorTestCase{
		{
			// Test carry between output batches.
			input: tuples{
				{0, 1},
				{1, 5},
				{0, 4},
				{0, 2},
				{2, 6},
				{0, 3},
				{0, 7},
			},
			colTypes:  []types.T{types.Int64, types.Int64},
			groupCols: []uint32{0},
			aggCols:   [][]uint32{{1}},

			expected: tuples{
				{5},
				{6},
				{17},
			},

			name: "carryBetweenBatches",
		},
		{
			// Test a single row input source.
			input: tuples{
				{5},
			},
			colTypes:  []types.T{types.Int64},
			groupCols: []uint32{0},
			aggCols:   [][]uint32{{0}},

			expected: tuples{
				{5},
			},

			name: "singleRowInput",
		},
		{
			// Test bucket collisions.
			input: tuples{
				{0, 3},
				{0, 4},
				{hashTableBucketSize, 6},
				{0, 5},
				{hashTableBucketSize, 7},
			},
			colTypes:  []types.T{types.Int64, types.Int64},
			groupCols: []uint32{0},
			aggCols:   [][]uint32{{1}},

			expected: tuples{
				{12},
				{13},
			},

			name: "bucketCollision",
		},
		{
			input: tuples{
				{0, 1, 1.3},
				{0, 1, 1.6},
				{0, 1, 0.5},
				{1, 1, 1.2},
			},
			colTypes:      []types.T{types.Int64, types.Int64, types.Decimal},
			convToDecimal: true,

			aggFns:    []distsqlpb.AggregatorSpec_Func{distsqlpb.AggregatorSpec_SUM, distsqlpb.AggregatorSpec_SUM},
			groupCols: []uint32{0, 1},
			aggCols: [][]uint32{
				{2}, {1},
			},

			expected: tuples{
				{3.4, 3},
				{1.2, 1},
			},

			name: "decimalSums",
		},
		{
			// Test unused input columns.
			input: tuples{
				{0, 1, 2, 3},
				{0, 1, 4, 5},
				{1, 1, 3, 7},
				{1, 2, 4, 9},
				{0, 1, 6, 11},
				{1, 2, 6, 13},
			},
			colTypes:  []types.T{types.Int64, types.Int64, types.Int64, types.Int64},
			groupCols: []uint32{0, 1},
			aggCols:   [][]uint32{{3}},

			expected: tuples{
				{7},
				{19},
				{22},
			},

			name: "unusedInputCol",
		},
	}

	for _, tc := range tcs {
		if err := tc.init(); err != nil {
			t.Fatal(err)
		}
		nOutput := len(tc.aggCols)
		cols := make([]int, nOutput)
		for i := 0; i < nOutput; i++ {
			cols[i] = i
		}
		runTests(t, []tuples{tc.input}, tc.expected, unorderedVerifier, cols, func(sources []Operator) (Operator, error) {
			return NewHashAggregator(sources[0], tc.colTypes, tc.aggFns, tc.groupCols, tc.aggCols)
		})
	}
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
