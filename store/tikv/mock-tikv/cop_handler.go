// Copyright 2016 PingCAP, Inc.
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

package mocktikv

import (
	"bytes"
	"encoding/binary"

	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
	"github.com/pingcap/tidb/xapi"
	"github.com/pingcap/tidb/xapi/xeval"
	"github.com/pingcap/tipb/go-tipb"
)

type selectContext struct {
	sel          *tipb.SelectRequest
	eval         *xeval.Evaluator
	whereColumns map[int64]*tipb.ColumnInfo
	groups       map[string]bool
	groupKeys    [][]byte
	aggregates   []*aggregateFuncExpr
	aggregate    bool

	// Use for DecodeRow.
	colTps map[int64]*types.FieldType
}

func (h *rpcHandler) handleCopRequest(req *coprocessor.Request) (*coprocessor.Response, error) {
	resp := &coprocessor.Response{}
	if err := h.checkContext(req.GetContext()); err != nil {
		resp.RegionError = err
		return resp, nil
	}
	if len(req.Ranges) == 0 {
		return resp, nil
	}
	if req.GetTp() == kv.ReqTypeSelect || req.GetTp() == kv.ReqTypeIndex {
		sel := new(tipb.SelectRequest)
		err := proto.Unmarshal(req.Data, sel)
		if err != nil {
			return nil, errors.Trace(err)
		}
		ctx := &selectContext{
			sel: sel,
		}
		ctx.eval = &xeval.Evaluator{Row: make(map[int64]types.Datum)}
		if sel.Where != nil {
			ctx.whereColumns = make(map[int64]*tipb.ColumnInfo)
			collectColumnsInWhere(sel.Where, ctx)
		}
		ctx.aggregate = len(sel.Aggregates) > 0 || len(sel.GetGroupBy()) > 0
		if ctx.aggregate {
			// compose aggregateFuncExpr
			ctx.aggregates = make([]*aggregateFuncExpr, 0, len(sel.Aggregates))
			for _, agg := range sel.Aggregates {
				aggExpr := &aggregateFuncExpr{expr: agg}
				ctx.aggregates = append(ctx.aggregates, aggExpr)
			}
			ctx.groups = make(map[string]bool)
			ctx.groupKeys = make([][]byte, 0)
		}

		var rows []*tipb.Row
		if req.GetTp() == kv.ReqTypeSelect {
			rows, err = h.getRowsFromSelectReq(ctx)
		} else {
			rows, err = h.getRowsFromIndexReq(ctx)
		}
		selResp := new(tipb.SelectResponse)
		selResp.Error = toPBError(err)
		selResp.Rows = rows
		if err != nil {
			resp.OtherError = proto.String(err.Error())
		}
		data, err := proto.Marshal(selResp)
		if err != nil {
			return nil, errors.Trace(err)
		}
		resp.Data = data
	}
	return resp, nil
}

func (h *rpcHandler) getRowsFromAgg(ctx *selectContext) ([]*tipb.Row, error) {
	rows := make([]*tipb.Row, 0, len(ctx.groupKeys))
	for _, gk := range ctx.groupKeys {
		row := new(tipb.Row)
		// Each aggregate partial result will be converted to one or two datums.
		rowData := make([]types.Datum, 0, 1+2*len(ctx.aggregates))
		// The first column is group key.
		rowData = append(rowData, types.NewBytesDatum(gk))
		for _, agg := range ctx.aggregates {
			agg.currentGroup = gk
			ds, err := agg.toDatums()
			if err != nil {
				return nil, errors.Trace(err)
			}
			rowData = append(rowData, ds...)
		}
		var err error
		row.Data, err = codec.EncodeValue(nil, rowData...)
		if err != nil {
			return nil, errors.Trace(err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func collectColumnsInWhere(expr *tipb.Expr, ctx *selectContext) error {
	if expr == nil {
		return nil
	}
	if expr.GetTp() == tipb.ExprType_ColumnRef {
		_, i, err := codec.DecodeInt(expr.Val)
		if err != nil {
			return errors.Trace(err)
		}
		var columns []*tipb.ColumnInfo
		if ctx.sel.TableInfo != nil {
			columns = ctx.sel.TableInfo.Columns
		} else {
			columns = ctx.sel.IndexInfo.Columns
		}
		for _, c := range columns {
			if c.GetColumnId() == i {
				ctx.whereColumns[i] = c
				return nil
			}
		}
		return xeval.ErrInvalid.Gen("column %d not found", i)
	}
	for _, child := range expr.Children {
		err := collectColumnsInWhere(child, ctx)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func toPBError(err error) *tipb.Error {
	if err == nil {
		return nil
	}
	perr := new(tipb.Error)
	code := int32(1)
	perr.Code = &code
	errStr := err.Error()
	perr.Msg = &errStr
	return perr
}

func (h *rpcHandler) getRowsFromSelectReq(ctx *selectContext) ([]*tipb.Row, error) {
	// Init ctx.colTps and use it to decode all the rows.
	columns := ctx.sel.TableInfo.Columns
	ctx.colTps = make(map[int64]*types.FieldType, len(columns))
	for _, col := range columns {
		if *col.PkHandle {
			continue
		}
		ctx.colTps[col.GetColumnId()] = xapi.FieldTypeFromPBColumn(col)
	}

	kvRanges, desc := h.extractKVRanges(ctx.sel)
	var rows []*tipb.Row
	limit := int64(-1)
	if ctx.sel.Limit != nil {
		limit = ctx.sel.GetLimit()
	}
	for _, ran := range kvRanges {
		if limit == 0 {
			break
		}
		ranRows, err := h.getRowsFromRange(ctx, ran, limit, desc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		rows = append(rows, ranRows...)
		limit -= int64(len(ranRows))
	}
	if ctx.aggregate {
		return h.getRowsFromAgg(ctx)
	}
	return rows, nil
}

// extractKVRanges extracts kv.KeyRanges slice from a SelectRequest, and also returns if it is in descending order.
func (h *rpcHandler) extractKVRanges(sel *tipb.SelectRequest) (kvRanges []kv.KeyRange, desc bool) {
	var (
		tid   int64
		idxID int64
	)
	if sel.IndexInfo != nil {
		tid = sel.IndexInfo.GetTableId()
		idxID = sel.IndexInfo.GetIndexId()
	} else {
		tid = sel.TableInfo.GetTableId()
	}
	for _, kran := range sel.Ranges {
		var upperKey, lowerKey kv.Key
		if idxID == 0 {
			upperKey = tablecodec.EncodeRowKey(tid, kran.GetHigh())
			if bytes.Compare(upperKey, h.startKey) <= 0 {
				continue
			}
			lowerKey = tablecodec.EncodeRowKey(tid, kran.GetLow())
		} else {
			upperKey = tablecodec.EncodeIndexSeekKey(tid, idxID, kran.GetHigh())
			if bytes.Compare(upperKey, h.startKey) <= 0 {
				continue
			}
			lowerKey = tablecodec.EncodeIndexSeekKey(tid, idxID, kran.GetLow())
		}
		if len(h.endKey) != 0 && bytes.Compare([]byte(lowerKey), h.endKey) >= 0 {
			break
		}
		var kvr kv.KeyRange
		kvr.StartKey = kv.Key(maxStartKey(lowerKey, h.startKey))
		kvr.EndKey = kv.Key(minEndKey(upperKey, h.endKey))
		kvRanges = append(kvRanges, kvr)
	}
	if sel.OrderBy != nil {
		desc = *sel.OrderBy[0].Desc
	}
	if desc {
		reverseKVRanges(kvRanges)
	}
	return
}

func reverseKVRanges(kvRanges []kv.KeyRange) {
	for i := 0; i < len(kvRanges)/2; i++ {
		j := len(kvRanges) - i - 1
		kvRanges[i], kvRanges[j] = kvRanges[j], kvRanges[i]
	}
}

func (h *rpcHandler) getRowsFromRange(ctx *selectContext, ran kv.KeyRange, limit int64, desc bool) ([]*tipb.Row, error) {
	startKey := maxStartKey(ran.StartKey, h.startKey)
	endKey := minEndKey(ran.EndKey, h.endKey)
	if limit == 0 || bytes.Compare(startKey, endKey) >= 0 {
		return nil, nil
	}
	var rows []*tipb.Row
	if ran.IsPoint() {
		val, err := h.mvccStore.Get(startKey, ctx.sel.GetStartTs())
		if len(val) == 0 {
			return nil, nil
		} else if err != nil {
			return nil, errors.Trace(err)
		}
		handle, err := tablecodec.DecodeRowKey(kv.Key(startKey))
		if err != nil {
			return nil, errors.Trace(err)
		}
		row, err := h.handleRowData(ctx, handle, val)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if row != nil {
			rows = append(rows, row)
		}
		return rows, nil
	}
	var seekKey []byte
	if desc {
		seekKey = endKey
	} else {
		seekKey = startKey
	}
	for {
		if limit == 0 {
			break
		}
		var (
			pairs []Pair
			pair  Pair
			err   error
		)
		if desc {
			pairs = h.mvccStore.ReverseScan(startKey, seekKey, 1, ctx.sel.GetStartTs())
		} else {
			pairs = h.mvccStore.Scan(seekKey, endKey, 1, ctx.sel.GetStartTs())
		}
		if len(pairs) > 0 {
			pair = pairs[0]
		}
		if pair.Err != nil {
			// TODO: handle lock error.
			return nil, errors.Trace(pair.Err)
		}
		if pair.Key == nil {
			break
		}
		if desc {
			if bytes.Compare(pair.Key, startKey) < 0 {
				break
			}
			seekKey = []byte(tablecodec.TruncateToRowKeyLen(kv.Key(pair.Key)))
		} else {
			if bytes.Compare(pair.Key, endKey) >= 0 {
				break
			}
			seekKey = []byte(kv.Key(pair.Key).PrefixNext())
		}
		handle, err := tablecodec.DecodeRowKey(pair.Key)
		if err != nil {
			return nil, errors.Trace(err)
		}
		row, err := h.handleRowData(ctx, handle, pair.Value)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if row != nil {
			rows = append(rows, row)
			limit--
		}
	}
	return rows, nil
}

// handleRowData deals with raw row data:
//	1. Decodes row from raw byte slice.
//	2. Checks if it fit where condition.
//	3. Update aggregate functions.
func (h *rpcHandler) handleRowData(ctx *selectContext, handle int64, value []byte) (*tipb.Row, error) {
	columns := ctx.sel.TableInfo.Columns
	row := new(tipb.Row)
	var d types.Datum
	d.SetInt64(handle)
	var err error
	row.Handle, err = codec.EncodeValue(nil, d)
	if err != nil {
		return nil, errors.Trace(err)
	}
	rowData := make([][]byte, len(columns))
	values, err := h.getRowData(value, ctx.colTps)
	if err != nil {
		return nil, errors.Trace(err)
	}
	for i, col := range columns {
		if *col.PkHandle {
			var colVal []byte
			if mysql.HasUnsignedFlag(uint(*col.Flag)) {
				// PK column is Unsigned
				var ud types.Datum
				ud.SetUint64(uint64(handle))
				var err1 error
				colVal, err1 = codec.EncodeValue(nil, ud)
				if err1 != nil {
					return nil, errors.Trace(err1)
				}
			} else {
				colVal = row.Handle
			}
			rowData[i] = colVal
			continue
		}
		v, ok := values[col.GetColumnId()]
		if !ok {
			if mysql.HasNotNullFlag(uint(col.GetFlag())) {
				return nil, errors.New("Miss column")
			}
			v = []byte{codec.NilFlag}
			values[col.GetColumnId()] = v
		}
		rowData[i] = v
	}
	// Evalue where
	match, err := h.evalWhereForRow(ctx, handle, values)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !match {
		return nil, nil
	}

	if ctx.aggregate {
		// Update aggregate functions.
		err = h.aggregate(ctx, rowData)
		if err != nil {
			return nil, errors.Trace(err)
		}
	} else {
		// If without aggregate functions, just return raw row data.
		for _, d := range rowData {
			row.Data = append(row.Data, d...)
		}
	}
	return row, nil
}

func (h *rpcHandler) getRowData(value []byte, colTps map[int64]*types.FieldType) (map[int64][]byte, error) {
	res, err := tablecodec.CutRow(value, colTps)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if res == nil {
		res = make(map[int64][]byte, len(colTps))
	}
	return res, nil
}

func (h *rpcHandler) evalWhereForRow(ctx *selectContext, handle int64, row map[int64][]byte) (bool, error) {
	if ctx.sel.Where == nil {
		return true, nil
	}
	for colID, col := range ctx.whereColumns {
		if col.GetPkHandle() {
			ctx.eval.Row[colID] = types.NewIntDatum(handle)
		} else {
			data := row[colID]
			ft := xapi.FieldTypeFromPBColumn(col)
			datum, err := tablecodec.DecodeColumnValue(data, ft)
			if err != nil {
				return false, errors.Trace(err)
			}
			ctx.eval.Row[colID] = datum
		}
	}
	result, err := ctx.eval.Eval(ctx.sel.Where)
	if err != nil {
		return false, errors.Trace(err)
	}
	if result.IsNull() {
		return false, nil
	}
	boolResult, err := result.ToBool()
	if err != nil {
		return false, errors.Trace(err)
	}
	return boolResult == 1, nil
}

func (h *rpcHandler) getRowsFromIndexReq(ctx *selectContext) ([]*tipb.Row, error) {
	kvRanges, desc := h.extractKVRanges(ctx.sel)
	var rows []*tipb.Row
	limit := int64(-1)
	if ctx.sel.Limit != nil {
		limit = ctx.sel.GetLimit()
	}
	for _, ran := range kvRanges {
		if limit == 0 {
			break
		}
		ranRows, err := h.getIndexRowFromRange(ctx.sel, ran, desc, limit)
		if err != nil {
			return nil, errors.Trace(err)
		}
		rows = append(rows, ranRows...)
		limit -= int64(len(ranRows))
	}
	return rows, nil
}

func (h *rpcHandler) getIndexRowFromRange(sel *tipb.SelectRequest, ran kv.KeyRange, desc bool, limit int64) ([]*tipb.Row, error) {
	startKey := maxStartKey(ran.StartKey, h.startKey)
	endKey := minEndKey(ran.EndKey, h.endKey)
	if limit == 0 || bytes.Compare(startKey, endKey) >= 0 {
		return nil, nil
	}
	var rows []*tipb.Row
	var seekKey kv.Key
	if desc {
		seekKey = endKey
	} else {
		seekKey = startKey
	}
	for {
		if limit == 0 {
			break
		}
		var (
			pairs []Pair
			pair  Pair
			err   error
		)
		if desc {
			pairs = h.mvccStore.ReverseScan(startKey, seekKey, 1, sel.GetStartTs())
		} else {
			pairs = h.mvccStore.Scan(seekKey, endKey, 1, sel.GetStartTs())
		}
		if len(pairs) > 0 {
			pair = pairs[0]
		}
		if pair.Err != nil {
			// TODO: handle lock error.
			return nil, errors.Trace(pair.Err)
		}
		if pair.Key == nil {
			break
		}
		if desc {
			if bytes.Compare(pair.Key, startKey) < 0 {
				break
			}
			seekKey = pair.Key
		} else {
			if bytes.Compare(pair.Key, endKey) >= 0 {
				break
			}
			seekKey = []byte(kv.Key(pair.Key).PrefixNext())
		}

		datums, err := tablecodec.DecodeIndexKey(pair.Key)
		if err != nil {
			return nil, errors.Trace(err)
		}
		var handle types.Datum
		columns := sel.IndexInfo.Columns
		if len(datums) > len(columns) {
			handle = datums[len(columns)]
			datums = datums[:len(columns)]
		} else {
			var intHandle int64
			intHandle, err = decodeHandle(pair.Value)
			if err != nil {
				return nil, errors.Trace(err)
			}
			handle.SetInt64(intHandle)
		}
		data, err := codec.EncodeValue(nil, datums...)
		if err != nil {
			return nil, errors.Trace(err)
		}
		handleData, err := codec.EncodeValue(nil, handle)
		if err != nil {
			return nil, errors.Trace(err)
		}
		row := &tipb.Row{Handle: handleData, Data: data}
		rows = append(rows, row)
		limit--
	}
	return rows, nil
}

func maxStartKey(rangeStartKey kv.Key, regionStartKey []byte) []byte {
	if bytes.Compare([]byte(rangeStartKey), regionStartKey) > 0 {
		return []byte(rangeStartKey)
	}
	return regionStartKey
}

func minEndKey(rangeEndKey kv.Key, regionEndKey []byte) []byte {
	if len(regionEndKey) == 0 || bytes.Compare([]byte(rangeEndKey), regionEndKey) < 0 {
		return []byte(rangeEndKey)
	}
	return regionEndKey
}

func decodeHandle(data []byte) (int64, error) {
	var h int64
	buf := bytes.NewBuffer(data)
	err := binary.Read(buf, binary.BigEndian, &h)
	return h, errors.Trace(err)
}

func isDefaultNull(err error, col *tipb.ColumnInfo) bool {
	return terror.ErrorEqual(err, kv.ErrNotExist) && !mysql.HasNotNullFlag(uint(col.GetFlag()))
}
