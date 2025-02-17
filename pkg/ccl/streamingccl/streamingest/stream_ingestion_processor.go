// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package streamingest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl/streamclient"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv/bulk"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

var minimumFlushInterval = settings.RegisterPublicDurationSettingWithExplicitUnit(
	settings.TenantWritable,
	"bulkio.stream_ingestion.minimum_flush_interval",
	"the minimum timestamp between flushes; flushes may still occur if internal buffers fill up",
	5*time.Second,
	nil, /* validateFn */
)

// checkForCutoverSignalFrequency is the frequency at which the resumer polls
// the system.jobs table to check whether the stream ingestion job has been
// signaled to cutover.
var cutoverSignalPollInterval = settings.RegisterDurationSetting(
	settings.TenantWritable,
	"bulkio.stream_ingestion.cutover_signal_poll_interval",
	"the interval at which the stream ingestion job checks if it has been signaled to cutover",
	30*time.Second,
	settings.NonNegativeDuration,
)

var streamIngestionResultTypes = []*types.T{
	types.Bytes, // jobspb.ResolvedSpans
}

type mvccKeyValues []storage.MVCCKeyValue

func (s mvccKeyValues) Len() int           { return len(s) }
func (s mvccKeyValues) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s mvccKeyValues) Less(i, j int) bool { return s[i].Key.Less(s[j].Key) }

type streamIngestionProcessor struct {
	execinfra.ProcessorBase

	flowCtx *execinfra.FlowCtx
	spec    execinfrapb.StreamIngestionDataSpec
	output  execinfra.RowReceiver

	// curBatch temporarily batches MVCC Keys so they can be
	// sorted before ingestion.
	// TODO: This doesn't yet use a buffering adder since the current
	// implementation is specific to ingesting KV pairs without timestamps rather
	// than MVCCKeys.
	curBatch mvccKeyValues
	// batcher is used to flush SSTs to the storage layer.
	batcher           *bulk.SSTBatcher
	maxFlushRateTimer *timeutil.Timer

	// client is a streaming client which provides a stream of events from a given
	// address.
	forceClientForTests streamclient.Client

	// Checkpoint events may need to be buffered if they arrive within the same
	// minimumFlushInterval.
	bufferedCheckpoints map[string]hlc.Timestamp
	// lastFlushTime keeps track of the last time that we flushed due to a
	// checkpoint timestamp event.
	lastFlushTime time.Time
	// When the event channel closes, we should flush any events that remains to
	// be buffered. The processor keeps track of if we're done seeing new events,
	// and have attempted to flush them with `internalDrained`.
	internalDrained bool

	// pollingWaitGroup registers the polling goroutine and waits for it to return
	// when the processor is being drained.
	pollingWaitGroup sync.WaitGroup

	// eventCh is the merged event channel of all of the partition event streams.
	eventCh chan partitionEvent

	// cutoverCh is used to convey that the ingestion job has been signaled to
	// cutover.
	cutoverCh chan struct{}

	// closePoller is used to shutdown the poller that checks the job for a
	// cutover signal.
	closePoller chan struct{}
	// cancelMergeAndWait cancels the merging goroutines and waits for them to
	// finish. It cannot be called concurrently with Next(), as it consumes from
	// the merged channel.
	cancelMergeAndWait func()

	// mu is used to provide thread-safe read-write operations to ingestionErr
	// and pollingErr.
	mu struct {
		syncutil.Mutex

		// ingestionErr stores any error that is returned from the worker goroutine so
		// that it can be forwarded through the DistSQL flow.
		ingestionErr error

		// pollingErr stores any error that is returned from the poller checking for a
		// cutover signal so that it can be forwarded through the DistSQL flow.
		pollingErr error
	}

	// metrics are monitoring counters shared between all ingestion jobs.
	metrics *Metrics
}

// partitionEvent augments a normal event with the partition it came from.
type partitionEvent struct {
	streamingccl.Event
	partition string
}

var _ execinfra.Processor = &streamIngestionProcessor{}
var _ execinfra.RowSource = &streamIngestionProcessor{}

const streamIngestionProcessorName = "stream-ingestion-processor"

func newStreamIngestionDataProcessor(
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec execinfrapb.StreamIngestionDataSpec,
	post *execinfrapb.PostProcessSpec,
	output execinfra.RowReceiver,
) (execinfra.Processor, error) {

	sip := &streamIngestionProcessor{
		flowCtx:             flowCtx,
		spec:                spec,
		output:              output,
		curBatch:            make([]storage.MVCCKeyValue, 0),
		bufferedCheckpoints: make(map[string]hlc.Timestamp),
		maxFlushRateTimer:   timeutil.NewTimer(),
		cutoverCh:           make(chan struct{}),
		closePoller:         make(chan struct{}),
	}

	if err := sip.Init(sip, post, streamIngestionResultTypes, flowCtx, processorID, output, nil, /* memMonitor */
		execinfra.ProcStateOpts{
			InputsToDrain: []execinfra.RowSource{},
			TrailingMetaCallback: func() []execinfrapb.ProducerMetadata {
				sip.close()
				return nil
			},
		},
	); err != nil {
		return nil, err
	}

	return sip, nil
}

// Start is part of the RowSource interface.
func (sip *streamIngestionProcessor) Start(ctx context.Context) {
	log.Infof(ctx, "starting ingest proc")
	ctx = sip.StartInternal(ctx, streamIngestionProcessorName)

	sip.metrics = sip.flowCtx.Cfg.JobRegistry.MetricsStruct().StreamIngest.(*Metrics)

	evalCtx := sip.FlowCtx.EvalCtx
	db := sip.FlowCtx.Cfg.DB
	var err error
	sip.batcher, err = bulk.MakeStreamSSTBatcher(ctx, db, evalCtx.Settings,
		func() int64 { return storageccl.MaxIngestBatchSize(evalCtx.Settings) })
	if err != nil {
		sip.MoveToDraining(errors.Wrap(err, "creating stream sst batcher"))
		return
	}

	// Start a poller that checks if the stream ingestion job has been signaled to
	// cutover.
	sip.pollingWaitGroup.Add(1)
	go func() {
		defer sip.pollingWaitGroup.Done()
		err := sip.checkForCutoverSignal(ctx, sip.closePoller)
		if err != nil {
			sip.mu.Lock()
			sip.mu.pollingErr = errors.Wrap(err, "error while polling job for cutover signal")
			sip.mu.Unlock()
		}
	}()

	log.Infof(ctx, "starting %d stream partitions", len(sip.spec.PartitionIds))

	// Initialize the event streams.
	eventChs := make(map[string]chan streamingccl.Event)
	errChs := make(map[string]chan error)
	for i := range sip.spec.PartitionIds {
		id := sip.spec.PartitionIds[i]
		spec := streamclient.SubscriptionToken(sip.spec.PartitionSpecs[i])
		addr := sip.spec.PartitionAddresses[i]
		var streamClient streamclient.Client
		if sip.forceClientForTests != nil {
			streamClient = sip.forceClientForTests
			log.Infof(ctx, "using testing client")
		} else {
			streamClient, err = streamclient.NewStreamClient(streamingccl.StreamAddress(addr))
			if err != nil {
				sip.MoveToDraining(errors.Wrapf(err, "creating client for parition spec %q from %q", spec, addr))
				return
			}
		}

		eventCh, errCh, err := streamClient.Subscribe(ctx, streamclient.StreamID(sip.spec.StreamID), spec, sip.spec.StartTime)
		if err != nil {
			sip.MoveToDraining(errors.Wrapf(err, "consuming partition %v", addr))
			return
		}
		eventChs[id] = eventCh
		errChs[id] = errCh
	}
	sip.eventCh = sip.merge(ctx, eventChs, errChs)
}

// Next is part of the RowSource interface.
func (sip *streamIngestionProcessor) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	if sip.State != execinfra.StateRunning {
		return nil, sip.DrainHelper()
	}

	sip.mu.Lock()
	err := sip.mu.pollingErr
	sip.mu.Unlock()
	if err != nil {
		sip.MoveToDraining(err)
		return nil, sip.DrainHelper()
	}

	progressUpdate, err := sip.consumeEvents()
	if err != nil {
		sip.MoveToDraining(err)
		return nil, sip.DrainHelper()
	}

	if progressUpdate != nil {
		progressBytes, err := protoutil.Marshal(progressUpdate)
		if err != nil {
			sip.MoveToDraining(err)
			return nil, sip.DrainHelper()
		}
		row := rowenc.EncDatumRow{
			rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(progressBytes))),
		}
		return row, nil
	}

	sip.mu.Lock()
	err = sip.mu.ingestionErr
	sip.mu.Unlock()
	if err != nil {
		sip.MoveToDraining(err)
		return nil, sip.DrainHelper()
	}

	sip.MoveToDraining(nil /* error */)
	return nil, sip.DrainHelper()
}

// ConsumerClosed is part of the RowSource interface.
func (sip *streamIngestionProcessor) ConsumerClosed() {
	sip.close()
}

func (sip *streamIngestionProcessor) close() {
	if sip.Closed {
		return
	}

	if sip.batcher != nil {
		sip.batcher.Close()
	}
	if sip.maxFlushRateTimer != nil {
		sip.maxFlushRateTimer.Stop()
	}
	close(sip.closePoller)
	// Wait for the processor goroutine to return so that we do not access
	// processor state once it has shutdown.
	sip.pollingWaitGroup.Wait()
	// Wait for the merge goroutine.
	if sip.cancelMergeAndWait != nil {
		sip.cancelMergeAndWait()
	}

	sip.InternalClose()
}

// checkForCutoverSignal periodically loads the job progress to check for the
// sentinel value that signals the ingestion job to complete.
func (sip *streamIngestionProcessor) checkForCutoverSignal(
	ctx context.Context, stopPoller chan struct{},
) error {
	sv := &sip.flowCtx.Cfg.Settings.SV
	registry := sip.flowCtx.Cfg.JobRegistry
	tick := time.NewTicker(cutoverSignalPollInterval.Get(sv))
	jobID := sip.spec.JobID
	defer tick.Stop()
	for {
		select {
		case <-stopPoller:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			j, err := registry.LoadJob(ctx, jobspb.JobID(jobID))
			if err != nil {
				return err
			}
			progress := j.Progress()
			var sp *jobspb.Progress_StreamIngest
			var ok bool
			if sp, ok = progress.GetDetails().(*jobspb.Progress_StreamIngest); !ok {
				return errors.Newf("unknown progress type %T in stream ingestion job %d",
					j.Progress().Progress, jobID)
			}
			// Job has been signaled to complete.
			if !sp.StreamIngest.CutoverTime.IsEmpty() {
				// Sanity check that the requested cutover time is less than equal to
				// the resolved ts recorded in the job progress. This should already
				// have been enforced when the cutover was signaled via the builtin.
				// TODO(adityamaru): Remove this when we allow users to specify a
				// cutover time in the future.
				resolvedTimestamp := progress.GetHighWater()
				if resolvedTimestamp == nil {
					return errors.AssertionFailedf("cutover has been requested before job %d has had a chance to"+
						" record a resolved ts", jobID)
				}
				if resolvedTimestamp.Less(sp.StreamIngest.CutoverTime) {
					return errors.AssertionFailedf("requested cutover time %s is before the resolved time %s recorded"+
						" in job %d", sp.StreamIngest.CutoverTime.String(), resolvedTimestamp.String(),
						jobID)
				}
				sip.cutoverCh <- struct{}{}
				return nil
			}
		}
	}
}

// merge takes events from all the streams and merges them into a single
// channel.
func (sip *streamIngestionProcessor) merge(
	ctx context.Context,
	partitionStreams map[string]chan streamingccl.Event,
	errorStreams map[string]chan error,
) chan partitionEvent {
	merged := make(chan partitionEvent)

	ctx, cancel := context.WithCancel(ctx)
	g := ctxgroup.WithContext(ctx)

	sip.cancelMergeAndWait = func() {
		cancel()
		// Wait until the merged channel is closed by the goroutine above.
		for range merged {
		}
	}

	for partition, eventCh := range partitionStreams {
		partition := partition
		eventCh := eventCh
		errCh, ok := errorStreams[partition]
		if !ok {
			log.Fatalf(ctx, "could not find error channel for partition %q", partition)
		}
		g.GoCtx(func(ctx context.Context) error {
			ctxDone := ctx.Done()
			for {
				select {
				case event, ok := <-eventCh:
					if !ok {
						return nil
					}

					pe := partitionEvent{
						Event:     event,
						partition: partition,
					}

					select {
					case merged <- pe:
					case <-ctxDone:
						return ctx.Err()
					}
				case err := <-errCh:
					return err
				case <-ctxDone:
					return ctx.Err()
				}
			}
		})
	}
	go func() {
		err := g.Wait()
		sip.mu.Lock()
		defer sip.mu.Unlock()
		sip.mu.ingestionErr = err
		close(merged)
	}()

	return merged
}

// consumeEvents handles processing events on the merged event queue and returns
// once a checkpoint event has been emitted so that it can inform the downstream
// frontier processor to consider updating the frontier.
//
// It should only make a claim that about the resolved timestamp of a partition
// increasing after it has flushed all KV events previously received by that
// partition.
func (sip *streamIngestionProcessor) consumeEvents() (*jobspb.ResolvedSpans, error) {
	// This timer is used to batch up resolved timestamp events that occur within
	// a given time interval, as to not flush too often and allow the buffer to
	// accumulate data.
	// A flush may still occur if the in memory buffer becomes full.
	sv := &sip.FlowCtx.Cfg.Settings.SV

	if sip.internalDrained {
		return nil, nil
	}

	for sip.State == execinfra.StateRunning {
		select {
		case event, ok := <-sip.eventCh:
			if !ok {
				sip.internalDrained = true
				return sip.flush()
			}

			if streamingKnobs, ok := sip.FlowCtx.TestingKnobs().StreamingTestingKnobs.(*sql.StreamingTestingKnobs); ok {
				if streamingKnobs != nil {
					if streamingKnobs.RunAfterReceivingEvent != nil {
						streamingKnobs.RunAfterReceivingEvent(sip.Ctx)
					}
				}
			}

			switch event.Type() {
			case streamingccl.KVEvent:
				if err := sip.bufferKV(event); err != nil {
					return nil, err
				}
			case streamingccl.CheckpointEvent:
				if err := sip.bufferCheckpoint(event); err != nil {
					return nil, err
				}

				minFlushInterval := minimumFlushInterval.Get(sv)
				if timeutil.Since(sip.lastFlushTime) < minFlushInterval {
					// Not enough time has passed since the last flush. Let's set a timer
					// that will trigger a flush eventually.
					// TODO: This resets the timer every checkpoint event, but we only
					// need to reset it once.
					sip.maxFlushRateTimer.Reset(time.Until(sip.lastFlushTime.Add(minFlushInterval)))
					continue
				}

				return sip.flush()
			case streamingccl.GenerationEvent:
				log.Info(sip.Ctx, "GenerationEvent received")
				select {
				case <-sip.cutoverCh:
					sip.internalDrained = true
					return nil, nil
				case <-sip.Ctx.Done():
					return nil, sip.Ctx.Err()
				}
			default:
				return nil, errors.Newf("unknown streaming event type %v", event.Type())
			}
		case <-sip.cutoverCh:
			// TODO(adityamaru): Currently, the cutover time can only be <= resolved
			// ts written to the job progress and so there is no point flushing
			// buffered KVs only to be reverted. When we allow users to specify a
			// cutover ts in the future, this will need to change.
			//
			// On receiving a cutover signal, the processor must shutdown gracefully.
			sip.internalDrained = true
			return nil, nil

		case <-sip.maxFlushRateTimer.C:
			sip.maxFlushRateTimer.Read = true
			return sip.flush()
		}
	}

	// No longer running, we've closed our batcher.
	return nil, nil
}

func (sip *streamIngestionProcessor) bufferKV(event partitionEvent) error {
	// TODO: In addition to flushing when receiving a checkpoint event, we
	// should also flush when we've buffered sufficient KVs. A buffering adder
	// would save us here.

	kv := event.GetKV()
	if kv == nil {
		return errors.New("kv event expected to have kv")
	}
	mvccKey := storage.MVCCKey{
		Key:       kv.Key,
		Timestamp: kv.Value.Timestamp,
	}
	sip.curBatch = append(sip.curBatch, storage.MVCCKeyValue{Key: mvccKey, Value: kv.Value.RawBytes})
	return nil
}

func (sip *streamIngestionProcessor) bufferCheckpoint(event partitionEvent) error {
	log.Infof(sip.Ctx, "got checkpoint %v", event.GetResolved())
	resolvedTimePtr := event.GetResolved()
	if resolvedTimePtr == nil {
		return errors.New("checkpoint event expected to have a resolved timestamp")
	}
	resolvedTime := *resolvedTimePtr

	// Buffer the checkpoint.
	if lastTimestamp, ok := sip.bufferedCheckpoints[event.partition]; !ok || lastTimestamp.Less(resolvedTime) {
		sip.bufferedCheckpoints[event.partition] = resolvedTime
	}
	sip.metrics.ResolvedEvents.Inc(1)
	return nil
}

func (sip *streamIngestionProcessor) flush() (*jobspb.ResolvedSpans, error) {
	flushedCheckpoints := jobspb.ResolvedSpans{ResolvedSpans: make([]jobspb.ResolvedSpan, 0)}
	// Ensure that the current batch is sorted.
	sort.Sort(sip.curBatch)

	totalSize := 0
	for _, kv := range sip.curBatch {
		if err := sip.batcher.AddMVCCKey(sip.Ctx, kv.Key, kv.Value); err != nil {
			return nil, errors.Wrapf(err, "adding key %+v", kv)
		}
		totalSize += len(kv.Key.Key) + len(kv.Value)
	}

	if err := sip.batcher.Flush(sip.Ctx); err != nil {
		return nil, errors.Wrap(err, "flushing")
	}
	sip.metrics.Flushes.Inc(1)
	sip.metrics.IngestedBytes.Inc(int64(totalSize))
	sip.metrics.IngestedEvents.Inc(int64(len(sip.curBatch)))

	// Go through buffered checkpoint events, and put them on the channel to be
	// emitted to the downstream frontier processor.
	for partition, timestamp := range sip.bufferedCheckpoints {
		// Each partition is represented by a span defined by the
		// partition address.
		spanStartKey := roachpb.Key(partition)
		resolvedSpan := jobspb.ResolvedSpan{
			Span:      roachpb.Span{Key: spanStartKey, EndKey: spanStartKey.Next()},
			Timestamp: timestamp,
		}
		flushedCheckpoints.ResolvedSpans = append(flushedCheckpoints.ResolvedSpans, resolvedSpan)
	}

	// Reset the current batch.
	sip.curBatch = nil
	sip.lastFlushTime = timeutil.Now()
	sip.bufferedCheckpoints = make(map[string]hlc.Timestamp)

	return &flushedCheckpoints, sip.batcher.Reset(sip.Ctx)
}

func init() {
	rowexec.NewStreamIngestionDataProcessor = newStreamIngestionDataProcessor
}
