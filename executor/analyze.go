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

package executor

import (
	"strconv"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tipb/go-tipb"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

var _ Executor = &AnalyzeExec{}

// AnalyzeExec represents Analyze executor.
type AnalyzeExec struct {
	baseExecutor
	tasks []*analyzeTask
}

const (
	maxSampleSize        = 10000
	maxRegionSampleSize  = 1000
	maxSketchSize        = 10000
	maxBucketSize        = 256
	defaultCMSketchDepth = 5
	defaultCMSketchWidth = 2048
)

// Next implements the Executor Next interface.
func (e *AnalyzeExec) Next(ctx context.Context) (Row, error) {
	return nil, errors.Trace(e.run(ctx))
}

// NextChunk implements the Executor NextChunk interface.
func (e *AnalyzeExec) NextChunk(ctx context.Context, chk *chunk.Chunk) error {
	return errors.Trace(e.run(ctx))
}

func (e *AnalyzeExec) run(ctx context.Context) error {
	concurrency, err := getBuildStatsConcurrency(e.ctx)
	if err != nil {
		return errors.Trace(err)
	}
	taskCh := make(chan *analyzeTask, len(e.tasks))
	resultCh := make(chan statistics.AnalyzeResult, len(e.tasks))
	for i := 0; i < concurrency; i++ {
		go e.analyzeWorker(taskCh, resultCh)
	}
	for _, task := range e.tasks {
		taskCh <- task
	}
	close(taskCh)
	dom := domain.GetDomain(e.ctx)
	lease := dom.StatsHandle().Lease
	if lease > 0 {
		var err1 error
		for i := 0; i < len(e.tasks); i++ {
			result := <-resultCh
			if result.Err != nil {
				err1 = result.Err
				log.Error(errors.ErrorStack(err1))
				continue
			}
			dom.StatsHandle().AnalyzeResultCh() <- &result
		}
		// We sleep two lease to make sure other tidb node has updated this node.
		time.Sleep(lease * 2)
		return errors.Trace(err1)
	}
	results := make([]statistics.AnalyzeResult, 0, len(e.tasks))
	var err1 error
	for i := 0; i < len(e.tasks); i++ {
		result := <-resultCh
		if result.Err != nil {
			err1 = result.Err
			log.Error(errors.ErrorStack(err1))
			continue
		}
		results = append(results, result)
	}
	if err1 != nil {
		return errors.Trace(err1)
	}
	for _, result := range results {
		for i, hg := range result.Hist {
			err = statistics.SaveStatsToStorage(e.ctx, result.TableID, result.Count, result.IsIndex, hg, result.Cms[i])
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	err = dom.StatsHandle().Update(GetInfoSchema(e.ctx))
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func getBuildStatsConcurrency(ctx sessionctx.Context) (int, error) {
	sessionVars := ctx.GetSessionVars()
	concurrency, err := variable.GetSessionSystemVar(sessionVars, variable.TiDBBuildStatsConcurrency)
	if err != nil {
		return 0, errors.Trace(err)
	}
	c, err := strconv.ParseInt(concurrency, 10, 64)
	return int(c), errors.Trace(err)
}

type taskType int

const (
	colTask taskType = iota
	idxTask
)

type analyzeTask struct {
	taskType taskType
	idxExec  *AnalyzeIndexExec
	colExec  *AnalyzeColumnsExec
}

func (e *AnalyzeExec) analyzeWorker(taskCh <-chan *analyzeTask, resultCh chan<- statistics.AnalyzeResult) {
	for task := range taskCh {
		switch task.taskType {
		case colTask:
			resultCh <- analyzeColumnsPushdown(task.colExec)
		case idxTask:
			resultCh <- analyzeIndexPushdown(task.idxExec)
		}
	}
}

func analyzeIndexPushdown(idxExec *AnalyzeIndexExec) statistics.AnalyzeResult {
	hist, cms, err := idxExec.buildStats()
	if err != nil {
		return statistics.AnalyzeResult{Err: err}
	}
	result := statistics.AnalyzeResult{
		TableID: idxExec.tblInfo.ID,
		Hist:    []*statistics.Histogram{hist},
		Cms:     []*statistics.CMSketch{cms},
		IsIndex: 1,
	}
	if hist.Len() > 0 {
		result.Count = hist.Buckets[hist.Len()-1].Count
	}
	return result
}

// AnalyzeIndexExec represents analyze index push down executor.
type AnalyzeIndexExec struct {
	ctx         sessionctx.Context
	tblInfo     *model.TableInfo
	idxInfo     *model.IndexInfo
	concurrency int
	priority    int
	analyzePB   *tipb.AnalyzeReq
	result      distsql.SelectResult
}

func (e *AnalyzeIndexExec) open() error {
	idxRange := &ranger.NewRange{LowVal: []types.Datum{types.MinNotNullDatum()}, HighVal: []types.Datum{types.MaxValueDatum()}}
	var builder distsql.RequestBuilder
	kvReq, err := builder.SetIndexRanges(e.ctx.GetSessionVars().StmtCtx, e.tblInfo.ID, e.idxInfo.ID, []*ranger.NewRange{idxRange}).
		SetAnalyzeRequest(e.analyzePB).
		SetKeepOrder(true).
		SetPriority(e.priority).
		Build()
	kvReq.Concurrency = e.concurrency
	kvReq.IsolationLevel = kv.RC
	ctx := context.TODO()
	e.result, err = distsql.Analyze(ctx, e.ctx.GetClient(), kvReq)
	if err != nil {
		return errors.Trace(err)
	}
	e.result.Fetch(ctx)
	return nil
}

func (e *AnalyzeIndexExec) buildStats() (hist *statistics.Histogram, cms *statistics.CMSketch, err error) {
	if err = e.open(); err != nil {
		return nil, nil, errors.Trace(err)
	}
	defer func() {
		if err1 := e.result.Close(); err1 != nil {
			hist = nil
			cms = nil
			err = errors.Trace(err1)
		}
	}()
	hist = &statistics.Histogram{}
	cms = statistics.NewCMSketch(defaultCMSketchDepth, defaultCMSketchWidth)
	for {
		data, err := e.result.NextRaw(context.TODO())
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		if data == nil {
			break
		}
		resp := &tipb.AnalyzeIndexResp{}
		err = resp.Unmarshal(data)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		hist, err = statistics.MergeHistograms(e.ctx.GetSessionVars().StmtCtx, hist, statistics.HistogramFromProto(resp.Hist), maxBucketSize)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		if resp.Cms != nil {
			err := cms.MergeCMSketch(statistics.CMSketchFromProto(resp.Cms))
			if err != nil {
				return nil, nil, errors.Trace(err)
			}
		}
	}
	hist.ID = e.idxInfo.ID
	return hist, cms, nil
}

func analyzeColumnsPushdown(colExec *AnalyzeColumnsExec) statistics.AnalyzeResult {
	hists, cms, err := colExec.buildStats()
	if err != nil {
		return statistics.AnalyzeResult{Err: err}
	}
	result := statistics.AnalyzeResult{
		TableID: colExec.tblInfo.ID,
		Hist:    hists,
		Cms:     cms,
	}
	hist := hists[0]
	result.Count = hist.NullCount
	if hist.Len() > 0 {
		result.Count += hist.Buckets[hist.Len()-1].Count
	}
	return result
}

// AnalyzeColumnsExec represents Analyze columns push down executor.
type AnalyzeColumnsExec struct {
	ctx           sessionctx.Context
	tblInfo       *model.TableInfo
	colsInfo      []*model.ColumnInfo
	pkInfo        *model.ColumnInfo
	concurrency   int
	priority      int
	keepOrder     bool
	analyzePB     *tipb.AnalyzeReq
	resultHandler *tableResultHandler
}

func (e *AnalyzeColumnsExec) open() error {
	var ranges []*ranger.NewRange
	if e.pkInfo != nil {
		ranges = ranger.FullIntNewRange(mysql.HasUnsignedFlag(e.pkInfo.Flag))
	} else {
		ranges = ranger.FullIntNewRange(false)
	}
	e.resultHandler = &tableResultHandler{}
	firstPartRanges, secondPartRanges := splitRanges(ranges, e.keepOrder)
	firstResult, err := e.buildResp(firstPartRanges)
	if err != nil {
		return errors.Trace(err)
	}
	if len(secondPartRanges) == 0 {
		e.resultHandler.open(nil, firstResult)
		return nil
	}
	var secondResult distsql.SelectResult
	secondResult, err = e.buildResp(secondPartRanges)
	if err != nil {
		return errors.Trace(err)
	}
	e.resultHandler.open(firstResult, secondResult)

	return nil
}

func (e *AnalyzeColumnsExec) buildResp(ranges []*ranger.NewRange) (distsql.SelectResult, error) {
	var builder distsql.RequestBuilder
	kvReq, err := builder.SetTableRanges(e.tblInfo.ID, ranges).
		SetAnalyzeRequest(e.analyzePB).
		SetKeepOrder(e.keepOrder).
		SetPriority(e.priority).
		Build()
	kvReq.IsolationLevel = kv.RC
	kvReq.Concurrency = e.concurrency
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx := context.TODO()
	result, err := distsql.Analyze(ctx, e.ctx.GetClient(), kvReq)
	if err != nil {
		return nil, errors.Trace(err)
	}
	result.Fetch(ctx)
	return result, nil
}

func (e *AnalyzeColumnsExec) buildStats() (hists []*statistics.Histogram, cms []*statistics.CMSketch, err error) {
	if err = e.open(); err != nil {
		return nil, nil, errors.Trace(err)
	}
	defer func() {
		if err1 := e.resultHandler.Close(); err1 != nil {
			hists = nil
			cms = nil
			err = errors.Trace(err1)
		}
	}()
	pkHist := &statistics.Histogram{}
	collectors := make([]*statistics.SampleCollector, len(e.colsInfo))
	for i := range collectors {
		collectors[i] = &statistics.SampleCollector{
			IsMerger:      true,
			FMSketch:      statistics.NewFMSketch(maxSketchSize),
			MaxSampleSize: maxSampleSize,
			CMSketch:      statistics.NewCMSketch(defaultCMSketchDepth, defaultCMSketchWidth),
		}
	}
	for {
		data, err1 := e.resultHandler.nextRaw(context.TODO())
		if err1 != nil {
			return nil, nil, errors.Trace(err1)
		}
		if data == nil {
			break
		}
		resp := &tipb.AnalyzeColumnsResp{}
		err = resp.Unmarshal(data)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		sc := e.ctx.GetSessionVars().StmtCtx
		if e.pkInfo != nil {
			pkHist, err = statistics.MergeHistograms(sc, pkHist, statistics.HistogramFromProto(resp.PkHist), maxBucketSize)
			if err != nil {
				return nil, nil, errors.Trace(err)
			}
		}
		for i, rc := range resp.Collectors {
			collectors[i].MergeSampleCollector(sc, statistics.SampleCollectorFromProto(rc))
		}
	}
	timeZone := e.ctx.GetSessionVars().GetTimeZone()
	if e.pkInfo != nil {
		pkHist.ID = e.pkInfo.ID
		err = pkHist.DecodeTo(&e.pkInfo.FieldType, timeZone)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		hists = append(hists, pkHist)
		cms = append(cms, nil)
	}
	for i, col := range e.colsInfo {
		for j, s := range collectors[i].Samples {
			collectors[i].Samples[j], err = tablecodec.DecodeColumnValue(s.GetBytes(), &col.FieldType, timeZone)
			if err != nil {
				return nil, nil, errors.Trace(err)
			}
		}
		hg, err := statistics.BuildColumn(e.ctx, maxBucketSize, col.ID, collectors[i], &col.FieldType)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		hists = append(hists, hg)
		cms = append(cms, collectors[i].CMSketch)
	}
	return hists, cms, nil
}
